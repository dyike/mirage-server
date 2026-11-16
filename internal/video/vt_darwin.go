//go:build darwin && arm64

package video

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/dyike/mirage-server/internal/frame"
)

// VideoToolbox through purego, without cgo, as keel does for AppKit.
//
// The decoder's output callback takes two CMTime structs by value. CMTime
// is 24 bytes, and arm64 passes composites over 16 bytes as pointers, so
// the callback is an ordinary seven-argument function there. amd64 passes
// them on the stack, which purego callbacks cannot express: Intel Macs are
// not supported.

var vt struct {
	once sync.Once
	err  error

	cfRelease          func(uintptr)
	cfDictionaryCreate func(alloc uintptr, keys, values *uintptr, n int, kcb, vcb uintptr) uintptr
	cfNumberCreate     func(alloc uintptr, typ int, v *int32) uintptr
	keyCallBacks       uintptr
	valueCallBacks     uintptr

	fmtH264     func(alloc uintptr, n uint, sets *uintptr, sizes *uint, nalLen int32, out *uintptr) int32
	fmtHEVC     func(alloc uintptr, n uint, sets *uintptr, sizes *uint, nalLen int32, ext uintptr, out *uintptr) int32
	blockCreate func(alloc, mem uintptr, size uint, blockAlloc, src uintptr, off, length uint, flags uint32, out *uintptr) int32
	blockFill   func(src unsafe.Pointer, bb uintptr, off, length uint) int32
	sampleReady func(alloc, bb, desc uintptr, n, nTiming int, timing uintptr, nSizes int, sizes *uint, out *uintptr) int32

	sessionCreate func(alloc, desc, spec, attrs uintptr, cb *outputCallback, out *uintptr) int32
	decodeFrame   func(s, sample uintptr, flags uint32, ref uintptr, info *uint32) int32
	canAccept     func(s, desc uintptr) bool
	invalidate    func(s uintptr)
	copyProperty  func(s, key, alloc uintptr, out *uintptr) int32
	booleanValue  func(uintptr) bool
	hardwareKey   uintptr

	lock     func(pb uintptr, flags uint64) int32
	unlock   func(pb uintptr, flags uint64) int32
	base     func(pb uintptr) uintptr
	rowBytes func(pb uintptr) uint
	width    func(pb uintptr) uint
	height   func(pb uintptr) uint
	fmtKey   uintptr // kCVPixelBufferPixelFormatTypeKey
	surfKey  uintptr // kCVPixelBufferIOSurfacePropertiesKey
	metalKey uintptr // kCVPixelBufferMetalCompatibilityKey
	cfTrue   uintptr // kCFBooleanTrue
	surface  func(pb uintptr) uintptr
	retain   func(cf uintptr) uintptr

	callback uintptr
}

type outputCallback struct{ fn, refcon uintptr }

func load() error {
	vt.once.Do(func() {
		open := func(name string) uintptr {
			h, err := purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil && vt.err == nil {
				vt.err = err
			}
			return h
		}
		sym := func(lib uintptr, name string) uintptr {
			p, err := purego.Dlsym(lib, name)
			if err != nil && vt.err == nil {
				vt.err = err
			}
			return p
		}
		cf, cm, cv, vtb := open("CoreFoundation"), open("CoreMedia"), open("CoreVideo"), open("VideoToolbox")
		if vt.err != nil {
			return
		}
		purego.RegisterLibFunc(&vt.cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&vt.cfDictionaryCreate, cf, "CFDictionaryCreate")
		purego.RegisterLibFunc(&vt.cfNumberCreate, cf, "CFNumberCreate")
		purego.RegisterLibFunc(&vt.booleanValue, cf, "CFBooleanGetValue")
		vt.keyCallBacks = sym(cf, "kCFTypeDictionaryKeyCallBacks")
		vt.valueCallBacks = sym(cf, "kCFTypeDictionaryValueCallBacks")

		purego.RegisterLibFunc(&vt.fmtH264, cm, "CMVideoFormatDescriptionCreateFromH264ParameterSets")
		purego.RegisterLibFunc(&vt.fmtHEVC, cm, "CMVideoFormatDescriptionCreateFromHEVCParameterSets")
		purego.RegisterLibFunc(&vt.blockCreate, cm, "CMBlockBufferCreateWithMemoryBlock")
		purego.RegisterLibFunc(&vt.blockFill, cm, "CMBlockBufferReplaceDataBytes")
		purego.RegisterLibFunc(&vt.sampleReady, cm, "CMSampleBufferCreateReady")

		purego.RegisterLibFunc(&vt.sessionCreate, vtb, "VTDecompressionSessionCreate")
		purego.RegisterLibFunc(&vt.decodeFrame, vtb, "VTDecompressionSessionDecodeFrame")
		purego.RegisterLibFunc(&vt.canAccept, vtb, "VTDecompressionSessionCanAcceptFormatDescription")
		purego.RegisterLibFunc(&vt.invalidate, vtb, "VTDecompressionSessionInvalidate")
		purego.RegisterLibFunc(&vt.copyProperty, vtb, "VTSessionCopyProperty")

		purego.RegisterLibFunc(&vt.lock, cv, "CVPixelBufferLockBaseAddress")
		purego.RegisterLibFunc(&vt.unlock, cv, "CVPixelBufferUnlockBaseAddress")
		purego.RegisterLibFunc(&vt.base, cv, "CVPixelBufferGetBaseAddress")
		purego.RegisterLibFunc(&vt.rowBytes, cv, "CVPixelBufferGetBytesPerRow")
		purego.RegisterLibFunc(&vt.width, cv, "CVPixelBufferGetWidth")
		purego.RegisterLibFunc(&vt.height, cv, "CVPixelBufferGetHeight")
		global := func(lib uintptr, name string) uintptr {
			if p := sym(lib, name); p != 0 {
				return **(**uintptr)(unsafe.Pointer(&p)) // the variable holds a CF object
			}
			return 0
		}
		vt.fmtKey = global(cv, "kCVPixelBufferPixelFormatTypeKey")
		vt.surfKey = global(cv, "kCVPixelBufferIOSurfacePropertiesKey")
		vt.metalKey = global(cv, "kCVPixelBufferMetalCompatibilityKey")
		vt.cfTrue = global(cf, "kCFBooleanTrue")
		vt.hardwareKey = global(vtb, "kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder")
		purego.RegisterLibFunc(&vt.surface, cv, "CVPixelBufferGetIOSurface")
		purego.RegisterLibFunc(&vt.retain, cf, "CFRetain")
		vt.callback = purego.NewCallback(decoded)
	})
	return vt.err
}

// Decoders by callback refcon: the callback runs on a VideoToolbox thread
// and must not hold Go pointers across the C boundary.
var (
	decodersMu sync.Mutex
	decoders   = map[uintptr]*Decoder{}
	nextID     uintptr
)

// Decoder is a hardware decoder session. Decode and Close must not be
// called concurrently.
type Decoder struct {
	codec      Codec
	onFrame    func(Frame)
	id         uintptr
	sets       [][]byte // parameter sets of the current format
	desc       uintptr  // CMVideoFormatDescription
	session    uintptr
	attrs      uintptr // destination image buffer attributes
	err        error   // last decode error from the callback
	noCPU      bool    // frames without Pix
	status     DecoderStatus
	unit       accessUnit // reusable parameter-set list and length-prefixed sample
	timing     frameTimes
	frameID    uint64
	sourceTime frame.Timing
	// Native output arguments live with the decoder so passing their
	// addresses through purego does not allocate Go objects for every frame.
	block, sample uintptr
	sampleSize    uint
	decodeInfo    uint32
}

// NewDecoder returns a decoder that calls onFrame, on a decoder thread,
// for each picture. The session starts at the first parameter sets.
func NewDecoder(c Codec, onFrame func(Frame)) (*Decoder, error) {
	if err := load(); err != nil {
		return nil, fmt.Errorf("video: VideoToolbox: %w", err)
	}
	d := &Decoder{codec: c, onFrame: onFrame, status: DecoderStatus{Codec: c.String(), Backend: "VideoToolbox"}}
	decodersMu.Lock()
	nextID++
	d.id = nextID
	decoders[d.id] = d
	decodersMu.Unlock()

	const kCFNumberSInt32Type = 3
	bgra := int32(0x42475241) // kCVPixelFormatType_32BGRA
	num := vt.cfNumberCreate(0, kCFNumberSInt32Type, &bgra)
	defer vt.cfRelease(num)
	// IOSurface-backed and Metal-compatible, so a renderer can sample the
	// decoder's output in place.
	empty := vt.cfDictionaryCreate(0, nil, nil, 0, vt.keyCallBacks, vt.valueCallBacks)
	defer vt.cfRelease(empty)
	keys := []uintptr{vt.fmtKey, vt.surfKey, vt.metalKey}
	values := []uintptr{num, empty, vt.cfTrue}
	d.attrs = vt.cfDictionaryCreate(0, &keys[0], &values[0], len(keys), vt.keyCallBacks, vt.valueCallBacks)
	return d, nil
}

// SetCPUFrames chooses whether frames carry Pix (default true). Locking a
// picture's memory for the CPU costs time a renderer drawing Surface does
// not need.
func (d *Decoder) SetCPUFrames(on bool) { d.noCPU = !on }

// Status is called by the decoder's owner, not concurrently with Decode/Close.
func (d *Decoder) Status() DecoderStatus { return d.status }

// RetainBuffer keeps a frame's Buffer and Surface valid past the callback.
func RetainBuffer(b uintptr) {
	if b != 0 {
		vt.retain(b)
	}
}

// ReleaseBuffer undoes RetainBuffer.
func ReleaseBuffer(b uintptr) {
	if b != 0 {
		vt.cfRelease(b)
	}
}

func Snapshot(buffer uintptr) *image.RGBA {
	if buffer == 0 || load() != nil || vt.lock(buffer, 1) != 0 {
		return nil
	}
	defer vt.unlock(buffer, 1)
	width, height := int(vt.width(buffer)), int(vt.height(buffer))
	stride, base := int(vt.rowBytes(buffer)), vt.base(buffer)
	if width <= 0 || height <= 0 || stride < width*4 || base == 0 {
		return nil
	}
	pixels := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&base))), stride*height)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for row := range height {
		for column := range width {
			source := row*stride + column*4
			target := row*img.Stride + column*4
			img.Pix[target], img.Pix[target+1], img.Pix[target+2], img.Pix[target+3] = pixels[source+2], pixels[source+1], pixels[source], 255
		}
	}
	return img
}

// CopyBGRA copies a retained native picture's visible top-left region into
// caller-owned storage. It allocates no Go image or pixel slice; purego's
// native call overhead remains subject to the decoder boundary budget.
func CopyBGRA(buffer uintptr, dst []byte, stride, w, h int) error {
	if buffer == 0 || w <= 0 || h <= 0 || stride <= 0 || w > stride/4 || uint64(stride) > uint64(len(dst))/uint64(h) {
		return errors.New("video: invalid native frame copy destination")
	}
	if err := load(); err != nil {
		return err
	}
	if vt.lock(buffer, 1) != 0 {
		return errors.New("video: could not map native frame")
	}
	defer vt.unlock(buffer, 1)
	width, height := int(vt.width(buffer)), int(vt.height(buffer))
	sourceStride, base := int(vt.rowBytes(buffer)), vt.base(buffer)
	if width < w || height < h || sourceStride < width*4 || base == 0 {
		return errors.New("video: native frame does not cover requested copy")
	}
	pixels := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&base))), sourceStride*height)
	for y := range h {
		copy(dst[y*stride:][:w*4], pixels[y*sourceStride:][:w*4])
	}
	return nil
}

// ErrNoParameterSets means a picture came before the stream's SPS/PPS.
var ErrNoParameterSets = errors.New("video: no parameter sets yet")

// Decode decodes one access unit, given as NAL units without start codes.
// Parameter sets in it (re)configure the session.
func (d *Decoder) Decode(nals [][]byte) error {
	d.unit.prepare(d.codec, nals)
	defer d.unit.clearSets()
	if len(d.unit.sets) > 0 && !sameSets(d.unit.sets, d.sets) {
		if err := d.configure(d.unit.sets); err != nil {
			return err
		}
	}
	size := len(d.unit.data)
	if size == 0 {
		return nil
	}
	if d.session == 0 {
		return ErrNoParameterSets
	}
	d.block, d.sample = 0, 0
	if st := vt.blockCreate(0, 0, uint(size), 0, 0, 0, uint(size), 0, &d.block); st != 0 {
		return fmt.Errorf("video: CMBlockBufferCreate: %d", st)
	}
	defer vt.cfRelease(d.block)
	// blockFill copies the bytes into CoreMedia-owned memory before returning.
	if st := vt.blockFill(unsafe.Pointer(&d.unit.data[0]), d.block, 0, uint(size)); st != 0 {
		return fmt.Errorf("video: CMBlockBufferReplaceDataBytes: %d", st)
	}
	d.sampleSize = uint(size)
	if st := vt.sampleReady(0, d.block, d.desc, 1, 0, 0, 1, &d.sampleSize, &d.sample); st != 0 {
		return fmt.Errorf("video: CMSampleBufferCreateReady: %d", st)
	}
	defer vt.cfRelease(d.sample)
	d.err = nil
	d.decodeInfo = 0
	d.frameID++
	d.timing.put(d.frameID, d.sourceTime)
	if st := vt.decodeFrame(d.session, d.sample, 0, uintptr(d.frameID), &d.decodeInfo); st != 0 {
		d.timing.take(d.frameID)
		return fmt.Errorf("video: decode: %d", st)
	}
	return d.err
}

func (d *Decoder) DecodeAt(nals [][]byte, at time.Time) error {
	return d.DecodeTimed(nals, frame.Timing{Source: at})
}

func (d *Decoder) DecodeTimed(nals [][]byte, timing frame.Timing) error {
	d.sourceTime = timing
	defer func() { d.sourceTime = frame.Timing{} }()
	return d.Decode(nals)
}

func sameSets(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// configure builds a format description from the parameter sets and keeps
// the session if it accepts the new format.
func (d *Decoder) configure(sets [][]byte) error {
	var pin runtime.Pinner
	defer pin.Unpin()
	ptrs := make([]uintptr, len(sets))
	sizes := make([]uint, len(sets))
	for i, s := range sets {
		pin.Pin(&s[0])
		ptrs[i] = uintptr(unsafe.Pointer(&s[0]))
		sizes[i] = uint(len(s))
	}
	var desc uintptr
	var st int32
	if d.codec == HEVC {
		st = vt.fmtHEVC(0, uint(len(sets)), &ptrs[0], &sizes[0], 4, 0, &desc)
	} else {
		st = vt.fmtH264(0, uint(len(sets)), &ptrs[0], &sizes[0], 4, &desc)
	}
	if st != 0 {
		return fmt.Errorf("video: %v parameter sets rejected: %d", d.codec, st)
	}
	d.sets = make([][]byte, len(sets))
	for i, s := range sets {
		d.sets[i] = bytes.Clone(s)
	}
	if d.session != 0 && vt.canAccept(d.session, desc) {
		vt.cfRelease(d.desc)
		d.desc = desc
		d.queryHardware()
		return nil
	}
	d.closeSession()
	d.desc = desc
	cb := outputCallback{fn: vt.callback, refcon: d.id}
	if st := vt.sessionCreate(0, desc, 0, d.attrs, &cb, &d.session); st != 0 {
		d.session = 0
		return fmt.Errorf("video: VTDecompressionSessionCreate: %d", st)
	}
	d.queryHardware()
	return nil
}

func (d *Decoder) queryHardware() {
	d.status = DecoderStatus{Codec: d.codec.String(), Backend: "VideoToolbox"}
	var value uintptr
	st := vt.copyProperty(d.session, vt.hardwareKey, 0, &value)
	if st == 0 && value != 0 {
		d.status.HardwareKnown, d.status.Hardware = true, vt.booleanValue(value)
	} else {
		d.status.Error = fmt.Sprintf("VT hardware query: %d (value present: %v)", st, value != 0)
	}
	if value != 0 {
		vt.cfRelease(value)
	}
}

func (d *Decoder) closeSession() {
	if d.session != 0 {
		vt.invalidate(d.session)
		vt.cfRelease(d.session)
		d.session = 0
	}
	if d.desc != 0 {
		vt.cfRelease(d.desc)
		d.desc = 0
	}
}

// Close ends the session.
func (d *Decoder) Close() {
	d.closeSession()
	d.unit = accessUnit{}
	d.sets = nil
	if d.attrs != 0 {
		vt.cfRelease(d.attrs)
		d.attrs = 0
	}
	decodersMu.Lock()
	delete(decoders, d.id)
	decodersMu.Unlock()
}

// decoded is VTDecompressionOutputCallback. pts and duration are pointers
// to CMTime (see the note at the top).
func decoded(refcon, frameID uintptr, status, _ uint32, img, _, _ uintptr) {
	decodersMu.Lock()
	d := decoders[refcon]
	decodersMu.Unlock()
	if d == nil {
		return
	}
	sourceTime := d.timing.take(uint64(frameID))
	if int32(status) != 0 || img == 0 {
		if int32(status) != 0 {
			d.err = fmt.Errorf("video: decoder status %d", int32(status))
		}
		return
	}
	f := Frame{W: int(vt.width(img)), H: int(vt.height(img)), Stride: int(vt.rowBytes(img)), Buffer: img, Surface: vt.surface(img), Timing: sourceTime}
	if f.W == 0 || f.H == 0 {
		return
	}
	if d.noCPU {
		d.onFrame(f)
		return
	}
	const readOnly = 1
	if vt.lock(img, readOnly) != 0 {
		return
	}
	defer vt.unlock(img, readOnly)
	base := vt.base(img)
	if base == 0 {
		return
	}
	f.Pix = unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&base))), f.Stride*f.H) // C memory
	d.onFrame(f)
}
