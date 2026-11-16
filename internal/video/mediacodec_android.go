//go:build android && cgo

package video

/*
#cgo LDFLAGS: -lmediandk -landroid -ldl
#include <stdlib.h>
#include "mediacodec_android.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"log"
	"sync"
	"time"
	"unsafe"

	"github.com/dyike/mirage-server/internal/frame"
)

var ErrNoParameterSets = errors.New("video: no parameter sets yet")

// Decoder feeds Annex B access units to MediaCodec. Its output worker drains
// asynchronously, including the last frame of a still desktop. Decode, Close
// and SetCPUFrames are called by the owner; Status may run during callbacks.
type Decoder struct {
	codec        Codec
	onFrame      func(Frame)
	sets         map[int][]byte
	native       *C.MirageCodec
	stop, exited chan struct{}
	pts          uint64
	timing       frameTimes
	sourceTime   frame.Timing
	mu           sync.Mutex
	status       DecoderStatus
}

func NewDecoder(codec Codec, onFrame func(Frame)) (*Decoder, error) {
	if codec != H264 && codec != HEVC {
		return nil, ErrUnsupported
	}
	native := C.mirage_codec_new()
	if native == nil {
		return nil, fmt.Errorf("%w: MediaCodec ImageReader requires Android 7+", ErrUnsupported)
	}
	return &Decoder{codec: codec, onFrame: onFrame, native: native, sets: make(map[int][]byte),
		status: DecoderStatus{Codec: codec.String(), Backend: "MediaCodec"}}, nil
}

// CPU pixels remain necessary for Android's current renderer. This controls
// readback on Apple only; it never selects software video decoding on Android.
func (*Decoder) SetCPUFrames(bool) {}
func RetainBuffer(uintptr)         {}
func ReleaseBuffer(uintptr)        {}
func Snapshot(uintptr) *image.RGBA { return nil }

// MediaCodec delivers CPU pixels in Frame.Pix and exposes no retained native
// buffer. RDP uses those pixels directly, so native-buffer readback is unavailable.
func CopyBGRA(uintptr, []byte, int, int, int) error { return ErrUnsupported }

func (d *Decoder) Status() DecoderStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *Decoder) Decode(nals [][]byte) error {
	changed := false
	for _, nal := range nals {
		if IsParameterSet(d.codec, nal) {
			typ := NALType(d.codec, nal)
			if !bytes.Equal(d.sets[typ], nal) {
				d.sets[typ] = bytes.Clone(nal)
				changed = true
			}
		}
	}
	if changed || d.exited == nil {
		if err := d.configure(); err != nil {
			return err
		}
	}
	if err := d.Status().Error; err != "" {
		return errors.New(err)
	}
	var buf []byte
	for _, nal := range nals {
		buf = append(buf, 0, 0, 0, 1)
		buf = append(buf, nal...)
	}
	if len(buf) == 0 {
		return nil
	}
	d.pts += 16667
	d.timing.put(d.pts, d.sourceTime)
	if C.mirage_codec_input(d.native, unsafe.Pointer(&buf[0]), C.size_t(len(buf)), C.uint64_t(d.pts)) != 0 {
		d.timing.take(d.pts)
		return errors.New("video: " + C.GoString(C.mirage_codec_error(d.native)))
	}
	return nil
}

func (d *Decoder) DecodeAt(nals [][]byte, at time.Time) error {
	return d.DecodeTimed(nals, frame.Timing{Source: at})
}

func (d *Decoder) DecodeTimed(nals [][]byte, timing frame.Timing) error {
	d.sourceTime = timing
	defer func() { d.sourceTime = frame.Timing{} }()
	return d.Decode(nals)
}

func (d *Decoder) configure() error {
	sps, pps := 7, 8
	if d.codec == HEVC {
		sps, pps = 33, 34
	}
	if len(d.sets[sps]) == 0 || len(d.sets[pps]) == 0 || d.codec == HEVC && len(d.sets[32]) == 0 {
		return ErrNoParameterSets
	}
	w, h, err := spsDimensions(d.codec, d.sets[sps])
	if err != nil {
		return err
	}
	d.closeNative()
	d.native = C.mirage_codec_new()
	if d.native == nil {
		return ErrUnsupported
	}
	annex := func(nal []byte) []byte { return append([]byte{0, 0, 0, 1}, nal...) }
	csd0, csd1 := annex(d.sets[sps]), annex(d.sets[pps])
	mime := "video/avc"
	if d.codec == HEVC {
		mime = "video/hevc"
		csd0 = append(annex(d.sets[32]), csd0...)
		csd0 = append(csd0, csd1...)
		csd1 = nil
	}
	cmime := C.CString(mime)
	defer C.free(unsafe.Pointer(cmime))
	var extra unsafe.Pointer
	if len(csd1) != 0 {
		extra = unsafe.Pointer(&csd1[0])
	}
	if C.mirage_codec_configure(d.native, cmime, C.int(w), C.int(h), unsafe.Pointer(&csd0[0]), C.size_t(len(csd0)), extra, C.size_t(len(csd1))) != 0 {
		err := errors.New("video: " + C.GoString(C.mirage_codec_error(d.native)))
		d.closeNative()
		return err
	}
	hardware := int(C.mirage_codec_hardware(d.native))
	d.mu.Lock()
	d.status = DecoderStatus{Codec: d.codec.String(), Backend: "MediaCodec", Name: C.GoString(C.mirage_codec_name(d.native)), HardwareKnown: hardware >= 0, Hardware: hardware == 1, Conversion: "CPU YUV420 → BGRA"}
	status := d.status
	d.mu.Unlock()
	log.Printf("video: %s / %s / %s %dx%d hardware=%v known=%v", status.Backend, status.Codec, status.Name, w, h, status.Hardware, status.HardwareKnown)
	d.stop, d.exited = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(d.exited)
		drained := make(chan struct{})
		go func() { defer close(drained); d.drain() }()
		d.output()
		<-drained
	}()
	return nil
}

func (d *Decoder) outputError(result int) {
	err := fmt.Sprintf("video: MediaCodec output: %d", result)
	d.mu.Lock()
	d.status.Error = err
	d.mu.Unlock()
	log.Print(err)
}

func (d *Decoder) drain() {
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		if result := int(C.mirage_codec_drain(d.native)); result < 0 {
			d.outputError(result)
			return
		}
	}
}

func (d *Decoder) output() {
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		var picture C.MiragePicture
		started := time.Now()
		result := C.mirage_codec_output(d.native, &picture)
		if result < 0 {
			d.outputError(int(result))
			return
		}
		if result > 0 {
			d.mu.Lock()
			d.status.OutputTimeMS = float64(time.Since(started)) / float64(time.Millisecond)
			d.status.ConversionTimeMS = float64(picture.conversion_ms)
			d.mu.Unlock()
			w, h := int(picture.width), int(picture.height)
			d.onFrame(Frame{W: w, H: h, Stride: w * 4, Pix: unsafe.Slice((*byte)(unsafe.Pointer(picture.pixels)), w*h*4), Timing: d.timing.take(uint64(picture.timestamp_ns) / 1000)})
		} else {
			// Surface delivery is asynchronous. Poll for the final image even
			// when no subsequent input arrives, without spinning on an idle screen.
			select {
			case <-d.stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
}

func (d *Decoder) closeNative() {
	if d.exited != nil {
		close(d.stop)
		<-d.exited
		d.stop, d.exited = nil, nil
	}
	if d.native != nil {
		C.mirage_codec_delete(d.native)
		d.native = nil
	}
}

func (d *Decoder) Close() { d.closeNative() }
