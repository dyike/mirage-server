//go:build (windows || linux) && !android && (amd64 || arm64)

// Package eldcodec calls bundled native FDK AAC 2.0.3 through purego.
// See NOTICE and scripts/build-aaceld.py for license and source.
package eldcodec

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var codec struct {
	sync.Once
	err    error
	open   func(int32, uint32) uintptr
	config func(uintptr, **byte, *uint32) int32
	fill   func(uintptr, **byte, *uint32, *uint32) int32
	decode func(uintptr, *int16, int32, uint32) int32
	info   func(uintptr) unsafe.Pointer
	close  func(uintptr)
}

func initialize() {
	dir, err := os.UserCacheDir()
	if err != nil {
		codec.err = err
		return
	}
	sum := sha256.Sum256(library)
	dir = filepath.Join(dir, "Mirage", "codecs", fmt.Sprintf("%x", sum))
	if err = os.MkdirAll(dir, 0700); err != nil {
		codec.err = err
		return
	}
	name := "libmirage-aac.so"
	if runtime.GOOS == "windows" {
		name = "mirage-aac.dll"
	}
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, library) {
		f, e := os.CreateTemp(dir, "codec-*")
		if e != nil {
			codec.err = e
			return
		}
		defer os.Remove(f.Name())
		_, e = f.Write(library)
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e == nil {
			e = os.Chmod(f.Name(), 0700)
		}
		if e == nil {
			e = os.Rename(f.Name(), path)
		}
		if e != nil {
			if existing, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(existing, library) {
				codec.err = e
				return
			}
		}
	}
	symbol, err := loadLibrary(path)
	if err != nil {
		codec.err = err
		return
	}
	for name, target := range map[string]any{"aacDecoder_Open": &codec.open, "aacDecoder_ConfigRaw": &codec.config, "aacDecoder_Fill": &codec.fill, "aacDecoder_DecodeFrame": &codec.decode, "aacDecoder_GetStreamInfo": &codec.info, "aacDecoder_Close": &codec.close} {
		address, e := symbol(name)
		if e != nil {
			codec.err = e
			return
		}
		purego.RegisterFunc(target, address)
	}
}

type Decoder struct {
	handle uintptr
	pcm    [8192]int16
}

func New(config []byte) (*Decoder, error) {
	if len(config) == 0 || len(config) > 8192 {
		return nil, fmt.Errorf("audio: invalid AAC-ELD configuration")
	}
	codec.Do(initialize)
	if codec.err != nil {
		return nil, fmt.Errorf("audio: load native AAC-ELD decoder: %w", codec.err)
	}
	d := &Decoder{handle: codec.open(0, 1)} // TT_MP4_RAW
	if d.handle == 0 {
		return nil, fmt.Errorf("audio: could not allocate AAC-ELD decoder")
	}
	ptr := &config[0]
	size := uint32(len(config))
	status := codec.config(d.handle, &ptr, &size)
	runtime.KeepAlive(config)
	if status != 0 {
		d.Close()
		return nil, fmt.Errorf("audio: AAC-ELD configuration: %#x", status)
	}
	return d, nil
}

// Decode returns borrowed interleaved signed 16-bit PCM until the next call.
func (d *Decoder) Decode(packet []byte) ([]byte, error) {
	if d.handle == 0 {
		return nil, fmt.Errorf("audio: AAC-ELD decoder is closed")
	}
	if len(packet) == 0 || len(packet) > 8192 {
		return nil, fmt.Errorf("audio: invalid AAC-ELD packet size")
	}
	ptr := &packet[0]
	size := uint32(len(packet))
	remaining := size
	status := codec.fill(d.handle, &ptr, &size, &remaining)
	runtime.KeepAlive(packet)
	if status != 0 || remaining != 0 {
		return nil, fmt.Errorf("audio: AAC-ELD input: %#x (%d bytes unconsumed)", status, remaining)
	}
	status = codec.decode(d.handle, &d.pcm[0], int32(len(d.pcm)), 0)
	if status == 0x1002 {
		return nil, nil
	}
	if status != 0 && (status < 0x4000 || status > 0x4fff) {
		return nil, fmt.Errorf("audio: AAC-ELD decode: %#x", status)
	}
	info := codec.info(d.handle)
	if info == nil {
		return nil, fmt.Errorf("audio: missing AAC-ELD stream information")
	}
	format := (*[3]int32)(info) // CStreamInfo's stable INT prefix
	rate, frames, channels := format[0], format[1], format[2]
	if rate != 48000 || channels != 2 || frames <= 0 || frames > 4096 {
		return nil, fmt.Errorf("audio: AAC-ELD output changed: %d Hz, %d channels, %d frames", rate, channels, frames)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&d.pcm[0])), int(frames*channels)*2), nil
}

func (d *Decoder) Close() {
	if d.handle != 0 {
		codec.close(d.handle)
		d.handle = 0
	}
}
