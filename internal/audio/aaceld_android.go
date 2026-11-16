package audio

import (
	"fmt"
	"github.com/ebitengine/purego"
	"runtime"
	"sync"
	"unsafe"
)

var androidELD struct {
	sync.Once
	err                 error
	create              func(string) uintptr
	name                func(uintptr, **byte) int32
	releaseName         func(uintptr, *byte)
	configure           func(uintptr, uintptr, uintptr, uintptr, uint32) int32
	start, stop, delete func(uintptr) int32
	inputIndex          func(uintptr, int64) int64
	inputBuffer         func(uintptr, uintptr, *uintptr) uintptr
	queue               func(uintptr, uintptr, uintptr, uintptr, uint64, uint32) int32
	outputIndex         func(uintptr, *androidAudioBuffer, int64) int64
	outputBuffer        func(uintptr, uintptr, *uintptr) uintptr
	release             func(uintptr, uintptr, bool) int32
	outputFormat        func(uintptr) uintptr
	formatNew           func() uintptr
	formatDelete        func(uintptr)
	setString           func(uintptr, string, string)
	setInt              func(uintptr, string, int32)
	getInt              func(uintptr, string, *int32) bool
	setBuffer           func(uintptr, string, *byte, uintptr)
}

type androidAudioBuffer struct {
	Offset, Size int32
	PTS          int64
	Flags        uint32
}
type androidAudioDecoder struct {
	codec uintptr
	pts   uint64
	pcm   []byte
}

func loadAndroidELD() {
	h, err := purego.Dlopen("libmediandk.so", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		androidELD.err = err
		return
	}
	for name, target := range map[string]any{
		"AMediaCodec_createDecoderByType": &androidELD.create, "AMediaCodec_configure": &androidELD.configure,
		"AMediaCodec_start": &androidELD.start, "AMediaCodec_stop": &androidELD.stop, "AMediaCodec_delete": &androidELD.delete,
		"AMediaCodec_dequeueInputBuffer": &androidELD.inputIndex, "AMediaCodec_getInputBuffer": &androidELD.inputBuffer, "AMediaCodec_queueInputBuffer": &androidELD.queue,
		"AMediaCodec_dequeueOutputBuffer": &androidELD.outputIndex, "AMediaCodec_getOutputBuffer": &androidELD.outputBuffer, "AMediaCodec_releaseOutputBuffer": &androidELD.release, "AMediaCodec_getOutputFormat": &androidELD.outputFormat,
		"AMediaFormat_new": &androidELD.formatNew, "AMediaFormat_delete": &androidELD.formatDelete, "AMediaFormat_setString": &androidELD.setString, "AMediaFormat_setInt32": &androidELD.setInt, "AMediaFormat_getInt32": &androidELD.getInt, "AMediaFormat_setBuffer": &androidELD.setBuffer,
	} {
		address, e := purego.Dlsym(h, name)
		if e != nil {
			androidELD.err = e
			return
		}
		purego.RegisterFunc(target, address)
	}
	// Name queries were added in API 28. Missing queries must not prevent
	// playback on the older Android versions we still support.
	name, nameErr := purego.Dlsym(h, "AMediaCodec_getName")
	release, releaseErr := purego.Dlsym(h, "AMediaCodec_releaseName")
	if nameErr == nil && releaseErr == nil {
		purego.RegisterFunc(&androidELD.name, name)
		purego.RegisterFunc(&androidELD.releaseName, release)
	}
}

func NewAACELD(f Format) (*Player, error) {
	if err := validateELD(f); err != nil {
		return nil, err
	}
	androidELD.Do(loadAndroidELD)
	if androidELD.err != nil {
		return nil, androidELD.err
	}
	d := &androidAudioDecoder{codec: androidELD.create("audio/mp4a-latm")}
	if d.codec == 0 {
		return nil, fmt.Errorf("audio: Android has no AAC-ELD decoder")
	}
	format := androidELD.formatNew()
	if format == 0 {
		d.Close()
		return nil, fmt.Errorf("audio: could not allocate MediaFormat")
	}
	defer androidELD.formatDelete(format)
	androidELD.setString(format, "mime", "audio/mp4a-latm")
	androidELD.setInt(format, "sample-rate", 48000)
	androidELD.setInt(format, "channel-count", 2)
	androidELD.setInt(format, "aac-profile", 39)
	androidELD.setInt(format, "pcm-encoding", 2)
	androidELD.setBuffer(format, "csd-0", &RemoteDesktopELD[0], uintptr(len(RemoteDesktopELD)))
	if status := androidELD.configure(d.codec, format, 0, 0, 0); status != 0 {
		d.Close()
		return nil, fmt.Errorf("audio: configure AAC-ELD MediaCodec: %d", status)
	}
	if status := androidELD.start(d.codec); status != 0 {
		d.Close()
		return nil, fmt.Errorf("audio: start AAC-ELD MediaCodec: %d", status)
	}
	p, err := NewPCM(48000, 2)
	if err != nil {
		d.Close()
		return nil, err
	}
	p.decoder = d
	p.decoderName = d.name()
	return p, nil
}

func (d *androidAudioDecoder) name() string {
	if androidELD.name == nil || androidELD.releaseName == nil {
		return "MediaCodec"
	}
	var name *byte
	if androidELD.name(d.codec, &name) != 0 || name == nil {
		return "MediaCodec"
	}
	defer androidELD.releaseName(d.codec, name)
	var text []byte
	for p := unsafe.Pointer(name); len(text) < 1024; p = unsafe.Add(p, 1) {
		b := *(*byte)(p)
		if b == 0 {
			break
		}
		text = append(text, b)
	}
	if len(text) == 0 {
		return "MediaCodec"
	}
	return string(text)
}

func (d *androidAudioDecoder) Decode(packet []byte) ([]byte, error) {
	if len(packet) == 0 || len(packet) > 8192 {
		return nil, fmt.Errorf("audio: invalid AAC-ELD packet size")
	}
	index := androidELD.inputIndex(d.codec, 10000)
	if index < 0 {
		return nil, fmt.Errorf("audio: MediaCodec input unavailable: %d", index)
	}
	var capacity uintptr
	ptr := androidELD.inputBuffer(d.codec, uintptr(index), &capacity)
	if ptr == 0 || capacity < uintptr(len(packet)) {
		return nil, fmt.Errorf("audio: invalid MediaCodec input buffer")
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(ptr)), len(packet)), packet)
	status := androidELD.queue(d.codec, uintptr(index), 0, uintptr(len(packet)), d.pts, 0)
	d.pts += 10000
	runtime.KeepAlive(packet)
	if status != 0 {
		return nil, fmt.Errorf("audio: queue AAC-ELD: %d", status)
	}
	for range 4 {
		var info androidAudioBuffer
		index = androidELD.outputIndex(d.codec, &info, 5000)
		if index == -1 {
			return nil, nil
		}
		if index == -2 {
			format := androidELD.outputFormat(d.codec)
			if format == 0 {
				return nil, fmt.Errorf("audio: MediaCodec has no output format")
			}
			var rate, channels, encoding int32
			androidELD.getInt(format, "sample-rate", &rate)
			androidELD.getInt(format, "channel-count", &channels)
			androidELD.getInt(format, "pcm-encoding", &encoding)
			androidELD.formatDelete(format)
			if rate != 48000 || channels != 2 || encoding != 0 && encoding != 2 {
				return nil, fmt.Errorf("audio: MediaCodec output changed: %d Hz, %d channels, PCM %d", rate, channels, encoding)
			}
			continue
		}
		if index == -3 {
			continue
		}
		if index < 0 {
			return nil, fmt.Errorf("audio: MediaCodec output failed: %d", index)
		}
		ptr = androidELD.outputBuffer(d.codec, uintptr(index), &capacity)
		if ptr == 0 || info.Offset < 0 || info.Size < 0 || info.Size > 32768 || uint64(info.Offset)+uint64(info.Size) > uint64(capacity) {
			androidELD.release(d.codec, uintptr(index), false)
			return nil, fmt.Errorf("audio: invalid MediaCodec PCM buffer")
		}
		d.pcm = append(d.pcm[:0], unsafe.Slice((*byte)(unsafe.Pointer(ptr+uintptr(info.Offset))), int(info.Size))...)
		androidELD.release(d.codec, uintptr(index), false)
		return d.pcm, nil
	}
	return nil, nil
}
func (d *androidAudioDecoder) Close() {
	if d.codec != 0 {
		androidELD.stop(d.codec)
		androidELD.delete(d.codec)
		d.codec = 0
	}
}
