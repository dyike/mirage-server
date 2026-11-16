//go:build linux && !android

package audio

import (
	"bytes"
	"fmt"
	"github.com/ebitengine/purego"
	"runtime"
	"sync"
	"unsafe"
)

var eldEncoderAPI struct {
	sync.Once
	err    error
	open   func(*uintptr, uint32, uint32) int32
	set    func(uintptr, int32, uint32) int32
	encode func(uintptr, *eldBuffer, *eldBuffer, *eldInput, *eldOutput) int32
	info   func(uintptr, *eldInfo) int32
	close  func(*uintptr) int32
}

type eldBuffer struct {
	Count                int32
	Buffers              *unsafe.Pointer
	IDs, Sizes, Elements *int32
}
type eldInput struct{ Samples, Ancillary int32 }
type eldOutput struct{ Bytes, Samples, Ancillary, Reservoir int32 }
type eldInfo struct {
	MaxBytes, MaxAnc, Fill, Channels, Frames, Delay, CoreDelay uint32
	Config                                                     [64]byte
	ConfigSize                                                 uint32
}

func loadELDEncoder() {
	h, err := purego.Dlopen("libfdk-aac.so.2", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		eldEncoderAPI.err = err
		return
	}
	for name, fn := range map[string]any{"aacEncOpen": &eldEncoderAPI.open, "aacEncoder_SetParam": &eldEncoderAPI.set, "aacEncEncode": &eldEncoderAPI.encode, "aacEncInfo": &eldEncoderAPI.info, "aacEncClose": &eldEncoderAPI.close} {
		p, e := purego.Dlsym(h, name)
		if e != nil {
			eldEncoderAPI.err = e
			return
		}
		purego.RegisterFunc(fn, p)
	}
}

// ELDEncoder encodes 48kHz stereo S16LE into the existing Mirage AAC-ELD format.
// It is owned by one capture goroutine. No microphone access occurs here.
type ELDEncoder struct{ handle uintptr }

func NewELDEncoder() (*ELDEncoder, error) {
	eldEncoderAPI.Do(loadELDEncoder)
	if eldEncoderAPI.err != nil {
		return nil, eldEncoderAPI.err
	}
	e := &ELDEncoder{}
	if status := eldEncoderAPI.open(&e.handle, 0, 2); status != 0 {
		return nil, fmt.Errorf("AAC-ELD open: %#x", status)
	}
	fail := func(err error) (*ELDEncoder, error) { e.Close(); return nil, err }
	for _, p := range [][2]uint32{{0x100, 39}, {0x103, 48000}, {0x106, 2}, {0x105, 480}, {0x101, 128000}, {0x104, 0}, {0x300, 0}} {
		if status := eldEncoderAPI.set(e.handle, int32(p[0]), p[1]); status != 0 {
			return fail(fmt.Errorf("AAC-ELD parameter %#x: %#x", p[0], status))
		}
	}
	if status := eldEncoderAPI.encode(e.handle, nil, nil, nil, nil); status != 0 {
		return fail(fmt.Errorf("AAC-ELD init: %#x", status))
	}
	var info eldInfo
	if status := eldEncoderAPI.info(e.handle, &info); status != 0 {
		return fail(fmt.Errorf("AAC-ELD info: %#x", status))
	}
	if info.Frames != 480 || info.Channels != 2 || info.ConfigSize > 64 || !bytes.Equal(info.Config[:info.ConfigSize], RemoteDesktopELD) {
		return fail(fmt.Errorf("unsupported AAC-ELD config: frames=%d channels=%d config=%x", info.Frames, info.Channels, info.Config[:min(info.ConfigSize, 64)]))
	}
	return e, nil
}
func (e *ELDEncoder) Close() {
	if e.handle != 0 {
		eldEncoderAPI.close(&e.handle)
	}
}
func (e *ELDEncoder) Encode(pcm []byte) ([]byte, error) {
	if e.handle == 0 || len(pcm) != 1920 {
		return nil, fmt.Errorf("AAC-ELD needs an open encoder and 480 stereo samples")
	}
	out := make([]byte, 4096)
	inPtr, outPtr := unsafe.Pointer(&pcm[0]), unsafe.Pointer(&out[0])
	inID, outID := int32(0), int32(3)
	inSize, outSize := int32(len(pcm)), int32(len(out))
	inElem, outElem := int32(2), int32(1)
	in := eldBuffer{1, &inPtr, &inID, &inSize, &inElem}
	dst := eldBuffer{1, &outPtr, &outID, &outSize, &outElem}
	args := eldInput{Samples: 960}
	var result eldOutput
	status := eldEncoderAPI.encode(e.handle, &in, &dst, &args, &result)
	runtime.KeepAlive(pcm)
	runtime.KeepAlive(out)
	if status != 0 || result.Samples != 960 || result.Bytes < 0 || int(result.Bytes) > len(out) {
		return nil, fmt.Errorf("AAC-ELD encode: status=%#x samples=%d bytes=%d", status, result.Samples, result.Bytes)
	}
	return out[:result.Bytes], nil
}
