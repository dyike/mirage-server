//go:build windows && (amd64 || arm64)

package video

// Media Foundation and D3D11 are called through their system DLLs and COM
// vtables. No cgo, codec DLLs, or external decoder executable is required.

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ErrNoParameterSets = errors.New("video: no parameter sets yet")

var (
	mfplat          = windows.NewLazySystemDLL("mfplat.dll")
	mfStartup       = mfplat.NewProc("MFStartup")
	mfShutdown      = mfplat.NewProc("MFShutdown")
	mfEnum          = mfplat.NewProc("MFTEnumEx")
	mfMediaType     = mfplat.NewProc("MFCreateMediaType")
	mfSample        = mfplat.NewProc("MFCreateSample")
	mfMemoryBuffer  = mfplat.NewProc("MFCreateMemoryBuffer")
	mfDeviceManager = mfplat.NewProc("MFCreateDXGIDeviceManager")
	d3dCreateDevice = windows.NewLazySystemDLL("d3d11.dll").NewProc("D3D11CreateDevice")
	coInitialize    = windows.NewLazySystemDLL("ole32.dll").NewProc("CoInitializeEx")
	coUninitialize  = windows.NewLazySystemDLL("ole32.dll").NewProc("CoUninitialize")
	coFree          = windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemFree")
)

func mfGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString("{" + s + "}")
	if err != nil {
		panic(err)
	}
	return g
}

var (
	iidTransform   = mfGUID("bf94c121-5b05-4e6f-8000-ba598961414d")
	iidEvents      = mfGUID("2cd0bd52-bcd5-4b89-b62c-eadc0c031e7d")
	iidShutdown    = mfGUID("97ec2ea4-0e42-4937-97ac-9d6d328824e1")
	iidDXGIBuffer  = mfGUID("e7174cfa-1c9e-48b1-8866-626226bfc258")
	iidTexture     = mfGUID("6f15aaf2-d208-4e89-9ab4-489535d34f9c")
	iidMultithread = mfGUID("9b7e4e00-342c-4106-a19f-4f2704f689f0")
	mfVideoDecoder = mfGUID("d6c02d4b-6833-45b4-971a-05a4b04bab91")
	mfVideo        = mfGUID("73646976-0000-0010-8000-00aa00389b71")
	mfH264         = mfGUID("34363248-0000-0010-8000-00aa00389b71")
	mfHEVC         = mfGUID("43564548-0000-0010-8000-00aa00389b71")
	mfNV12         = mfGUID("3231564e-0000-0010-8000-00aa00389b71")
	mfMajorType    = mfGUID("48eba18e-f8c9-4687-bf11-0a74c9f96a8f")
	mfSubtype      = mfGUID("f7e34c9a-42e8-4714-b74b-cb29d72c35e5")
	mfFrameSize    = mfGUID("1652c33d-d6b2-4012-b834-72030849a37d")
	mfFrameRate    = mfGUID("c459a2e8-3d2c-4e44-b132-fee5156c7bb0")
	mfAspectRatio  = mfGUID("c6376a1e-8d0a-4027-be45-6d9a0ad39bb6")
	mfInterlace    = mfGUID("e2724bb8-e676-4806-b4b2-a8d6efb44ccd")
	mfLowLatency   = mfGUID("9c27891a-ed7a-40e1-88e8-b22727a024ee")
	mfD3D11Aware   = mfGUID("206b4fc8-fcf9-4c51-afe3-9764369e33a0")
	mfAsyncUnlock  = mfGUID("e5666d6b-3422-4eb6-a421-da7db1f8e207")
	mfAsync        = mfGUID("f81a699a-649a-497d-8c73-29f8fed6ad7a")
	mfFriendlyName = mfGUID("314ffbae-5b41-4c95-9c19-4e7d586face3")
	mfYUVMatrix    = mfGUID("3e23d450-2c75-4d25-a00e-b91670d12327")
	mfNominalRange = mfGUID("c21b8ee5-b956-4071-8daf-325edf5cab11")
)

const (
	mfNoEvents      uint32 = 0xc00d3e80
	mfNeedMoreInput uint32 = 0xc00d6d72
	mfStreamChange  uint32 = 0xc00d6d61
)

// mfCOM has the standard IUnknown prefix. Interface indexes below follow the
// Windows SDK's mfobjects.h, mftransform.h, and d3d11.h declarations.
type mfCOM struct{ vtable *[128]uintptr }

//go:uintptrescapes
func (p *mfCOM) call(method int, args ...uintptr) uint32 {
	a := append([]uintptr{uintptr(unsafe.Pointer(p))}, args...)
	r, _, _ := syscall.SyscallN(p.vtable[method], a...)
	runtime.KeepAlive(p)
	return uint32(r)
}
func (p *mfCOM) release() {
	if p != nil {
		p.call(2)
	}
}
func (p *mfCOM) query(iid *windows.GUID) (*mfCOM, error) {
	var out *mfCOM
	err := mfCheck("QueryInterface", p.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))))
	return out, err
}
func (p *mfCOM) set32(key *windows.GUID, v uint32) error {
	return mfCheck("SetUINT32", p.call(21, uintptr(unsafe.Pointer(key)), uintptr(v)))
}
func (p *mfCOM) set64(key *windows.GUID, v uint64) error {
	return mfCheck("SetUINT64", p.call(22, uintptr(unsafe.Pointer(key)), uintptr(v)))
}
func (p *mfCOM) setGUID(key, v *windows.GUID) error {
	return mfCheck("SetGUID", p.call(24, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(v))))
}
func (p *mfCOM) get32(key *windows.GUID) uint32 {
	var v uint32
	p.call(7, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	return v
}
func mfCheck(op string, hr uint32) error {
	if int32(hr) < 0 {
		return fmt.Errorf("video: Media Foundation %s: HRESULT 0x%08x", op, hr)
	}
	return nil
}

//go:uintptrescapes
func mfProc(proc *windows.LazyProc, args ...uintptr) error {
	if err := proc.Find(); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupported, proc.Name, err)
	}
	hr, _, _ := proc.Call(args...)
	return mfCheck(proc.Name, uint32(hr))
}

type mfRequest struct {
	nals   [][]byte
	result chan error
}

// Decoder owns a dedicated COM MTA thread. That thread also polls asynchronous
// transform events, so a still desktop's last frame does not need more input.
// Decode and Close must not run concurrently; Status is safe during callbacks.
type Decoder struct {
	codec        Codec
	onFrame      func(Frame)
	requests     chan mfRequest
	stop, exited chan struct{}
	closeOnce    sync.Once
	mu           sync.Mutex
	status       DecoderStatus
}

func NewDecoder(codec Codec, onFrame func(Frame)) (*Decoder, error) {
	if codec != H264 && codec != HEVC {
		return nil, ErrUnsupported
	}
	d := &Decoder{codec: codec, onFrame: onFrame, requests: make(chan mfRequest),
		stop: make(chan struct{}), exited: make(chan struct{}),
		status: DecoderStatus{Codec: codec.String(), Backend: "Media Foundation / D3D11"}}
	ready := make(chan error, 1)
	go d.run(ready)
	if err := <-ready; err != nil {
		<-d.exited
		return nil, err
	}
	return d, nil
}
func (d *Decoder) Status() DecoderStatus { d.mu.Lock(); defer d.mu.Unlock(); return d.status }
func (d *Decoder) fail(err error)        { d.mu.Lock(); d.status.Error = err.Error(); d.mu.Unlock() }
func (d *Decoder) Decode(nals [][]byte) error {
	r := mfRequest{nals: nals, result: make(chan error, 1)}
	select {
	case d.requests <- r:
		return <-r.result // the worker no longer borrows nals when it replies
	case <-d.exited:
		if msg := d.Status().Error; msg != "" {
			return errors.New(msg)
		}
		return errors.New("video: decoder is closed")
	}
}
func (d *Decoder) Close()                           { d.closeOnce.Do(func() { close(d.stop); <-d.exited }) }
func (*Decoder) SetCPUFrames(bool)                  {}
func RetainBuffer(uintptr)                          {}
func ReleaseBuffer(uintptr)                         {}
func Snapshot(uintptr) *image.RGBA                  { return nil }
func CopyBGRA(uintptr, []byte, int, int, int) error { return ErrUnsupported }

type mfNative struct {
	d                                      *Decoder
	device, context, manager               *mfCOM
	transform, events, activation, staging *mfCOM
	sets                                   map[int][]byte
	w, h                                   int
	inputID, outputID                      uint32
	credits                                int
	pts                                    uint64
	stageDesc                              mfTextureDesc
	pixels                                 []byte
	matrix, nominal                        uint32
	processor                              *mfVideoProcessor
	processorFailed                        bool
	latestSample                           *mfCOM
}
type mfTextureDesc struct {
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, MiscFlags uint32
}

func (d *Decoder) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(d.exited)
	if err := mfProc(coInitialize, 0, 0); err != nil {
		ready <- err
		return
	}
	defer coUninitialize.Call()
	if err := mfProc(mfStartup, 0x20070, 0); err != nil {
		ready <- err
		return
	}
	defer mfShutdown.Call()
	n := &mfNative{d: d, sets: make(map[int][]byte)}
	defer n.close()
	if err := n.init(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-d.stop:
			return
		case r := <-d.requests:
			err := n.decode(r.nals)
			if err != nil && !errors.Is(err, ErrNoParameterSets) {
				d.fail(err)
			}
			r.result <- err
		case <-tick.C:
			if err := n.pump(); err != nil {
				d.fail(err)
				return
			}
		}
	}
}

func (n *mfNative) init() error {
	// Hardware device only: never substitute WARP software rasterization.
	var level uint32
	if err := mfProc(d3dCreateDevice, 0, 1, 0, 0x20|0x800, 0, 0, 7,
		uintptr(unsafe.Pointer(&n.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&n.context))); err != nil {
		return err
	}
	mt, err := n.context.query(&iidMultithread)
	if err != nil {
		return err
	}
	mt.call(5, 1) // ID3D10Multithread::SetMultithreadProtected
	mt.release()
	var token uint32
	if err := mfProc(mfDeviceManager, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&n.manager))); err != nil {
		return err
	}
	if err := mfCheck("ResetDevice", n.manager.call(7, uintptr(unsafe.Pointer(n.device)), uintptr(token))); err != nil {
		return err
	}
	acts, err := n.enumerate()
	for _, a := range acts {
		a.release()
	}
	return err
}

func (n *mfNative) enumerate() ([]*mfCOM, error) {
	subtype := mfHEVC
	if n.d.codec == H264 {
		subtype = mfH264
	}
	filter := struct{ Major, Subtype windows.GUID }{mfVideo, subtype}
	var array **mfCOM
	var count uint32
	// Microsoft's DXVA-capable decoders are registered
	// as synchronous MFTs, so HARDWARE alone misses them. activate requires
	// D3D11 support and picture requires a GPU decoder surface in either case.
	var err error
	if runtime.GOARCH == "arm64" {
		// A by-value GUID occupies x0/x1 under the Windows ARM64 ABI.
		words := *(*[2]uint64)(unsafe.Pointer(&mfVideoDecoder))
		err = mfProc(mfEnum, uintptr(words[0]), uintptr(words[1]), 0x1|0x2|0x4|0x40,
			uintptr(unsafe.Pointer(&filter)), 0, uintptr(unsafe.Pointer(&array)), uintptr(unsafe.Pointer(&count)))
	} else {
		// Windows x64 passes a 16-byte by-value aggregate indirectly.
		err = mfProc(mfEnum, uintptr(unsafe.Pointer(&mfVideoDecoder)), 0x1|0x2|0x4|0x40,
			uintptr(unsafe.Pointer(&filter)), 0, uintptr(unsafe.Pointer(&array)), uintptr(unsafe.Pointer(&count)))
	}
	if err != nil {
		return nil, err
	}
	if array != nil {
		defer coFree.Call(uintptr(unsafe.Pointer(array)))
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: no Windows %s hardware MFT; check GPU driver and installed codec support", ErrUnsupported, n.d.codec)
	}
	return append([]*mfCOM(nil), unsafe.Slice(array, count)...), nil
}

func (n *mfNative) stopTransform() {
	n.latestSample.release()
	n.latestSample = nil
	if n.processor != nil {
		n.processor.close()
		n.processor = nil
	}
	n.processorFailed = false
	if n.transform != nil {
		if s, err := n.transform.query(&iidShutdown); err == nil {
			s.call(3)
			s.release()
		}
	}
	n.events.release()
	n.events = nil
	n.transform.release()
	n.transform = nil
	if n.activation != nil {
		n.activation.call(34)
		n.activation.release()
		n.activation = nil
	}
	n.staging.release()
	n.staging = nil
	n.credits = 0
}
func (n *mfNative) close() {
	n.stopTransform()
	n.manager.release()
	n.context.release()
	n.device.release()
}

func (n *mfNative) configure(w, h int) error {
	n.stopTransform()
	n.w, n.h = w, h
	acts, err := n.enumerate()
	if err != nil {
		return err
	}
	defer func() {
		for _, a := range acts {
			a.release()
		}
	}()
	var last error
	for _, a := range acts {
		a.call(1)
		n.activation = a
		if err := n.activate(); err != nil {
			last = err
			n.stopTransform()
			continue
		}
		var name [256]uint16
		a.call(12, uintptr(unsafe.Pointer(&mfFriendlyName)), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)), 0)
		n.d.mu.Lock()
		n.d.status.Name = windows.UTF16ToString(name[:])
		n.d.status.Error = ""
		n.d.status.HardwareKnown, n.d.status.Hardware = false, false
		n.d.mu.Unlock()
		return nil
	}
	return fmt.Errorf("video: could not configure %s hardware decoder: %w", n.d.codec, last)
}

func (n *mfNative) activate() error {
	if err := mfCheck("ActivateObject", n.activation.call(33, uintptr(unsafe.Pointer(&iidTransform)), uintptr(unsafe.Pointer(&n.transform)))); err != nil {
		return err
	}
	var attrs *mfCOM
	if err := mfCheck("GetAttributes", n.transform.call(8, uintptr(unsafe.Pointer(&attrs)))); err != nil {
		return err
	}
	defer attrs.release()
	async := attrs.get32(&mfAsync) != 0
	if async {
		if err := attrs.set32(&mfAsyncUnlock, 1); err != nil {
			return err
		}
	}
	if attrs.get32(&mfD3D11Aware) == 0 {
		return errors.New("video: hardware MFT is not D3D11 aware")
	}
	attrs.set32(&mfLowLatency, 1) // optional on older drivers
	if err := mfCheck("SET_D3D_MANAGER", n.transform.call(23, 2, uintptr(unsafe.Pointer(n.manager)))); err != nil {
		return err
	}
	var err error
	if async {
		n.events, err = n.transform.query(&iidEvents)
		if err != nil {
			return err
		}
	}
	n.inputID, n.outputID = 0, 0
	var inputs, outputs uint32
	if err := mfCheck("GetStreamCount", n.transform.call(4, uintptr(unsafe.Pointer(&inputs)), uintptr(unsafe.Pointer(&outputs)))); err != nil {
		return err
	}
	if inputs != 1 || outputs != 1 {
		return errors.New("video: hardware MFT must have one input and one output")
	}
	hr := n.transform.call(5, 1, uintptr(unsafe.Pointer(&n.inputID)), 1, uintptr(unsafe.Pointer(&n.outputID)))
	if hr != 0x80004001 {
		if err := mfCheck("GetStreamIDs", hr); err != nil {
			return err
		}
	} // E_NOTIMPL means IDs are zero
	var input *mfCOM
	if err := mfProc(mfMediaType, uintptr(unsafe.Pointer(&input))); err != nil {
		return err
	}
	defer input.release()
	subtype := mfHEVC
	if n.d.codec == H264 {
		subtype = mfH264
	}
	for _, err := range []error{
		input.setGUID(&mfMajorType, &mfVideo), input.setGUID(&mfSubtype, &subtype),
		input.set64(&mfFrameSize, uint64(n.w)<<32|uint64(n.h)),
		input.set64(&mfFrameRate, uint64(60)<<32|1), input.set64(&mfAspectRatio, uint64(1)<<32|1),
		input.set32(&mfInterlace, 2),
	} {
		if err != nil {
			return err
		}
	}
	if err := mfCheck("SetInputType", n.transform.call(15, uintptr(n.inputID), uintptr(unsafe.Pointer(input)), 0)); err != nil {
		return err
	}
	if err := n.outputType(); err != nil {
		return err
	}
	for _, msg := range []uintptr{0x10000000, 0x10000003} {
		if err := mfCheck("start streaming", n.transform.call(23, msg, 0)); err != nil {
			return err
		}
	}
	return nil
}

func (n *mfNative) outputType() error {
	for i := uintptr(0); i < 256; i++ {
		var typ *mfCOM
		hr := n.transform.call(14, uintptr(n.outputID), i, uintptr(unsafe.Pointer(&typ)))
		if err := mfCheck("GetOutputAvailableType", hr); err != nil {
			return err
		}
		var subtype windows.GUID
		hr = typ.call(10, uintptr(unsafe.Pointer(&mfSubtype)), uintptr(unsafe.Pointer(&subtype)))
		if int32(hr) >= 0 && subtype == mfNV12 {
			err := mfCheck("SetOutputType", n.transform.call(16, uintptr(n.outputID), uintptr(unsafe.Pointer(typ)), 0))
			n.matrix, n.nominal = typ.get32(&mfYUVMatrix), typ.get32(&mfNominalRange)
			typ.release()
			return err
		}
		typ.release()
	}
	return errors.New("video: hardware MFT has no NV12 output")
}

func (n *mfNative) decode(nals [][]byte) error {
	changed := false
	for _, nal := range nals {
		if IsParameterSet(n.d.codec, nal) {
			t := NALType(n.d.codec, nal)
			if !bytes.Equal(n.sets[t], nal) {
				n.sets[t] = bytes.Clone(nal)
				changed = true
			}
		}
	}
	sps, pps := 7, 8
	if n.d.codec == HEVC {
		sps, pps = 33, 34
	}
	if len(n.sets[sps]) == 0 || len(n.sets[pps]) == 0 || n.d.codec == HEVC && len(n.sets[32]) == 0 {
		return ErrNoParameterSets
	}
	var data []byte
	appendNAL := func(nal []byte) { data = append(data, 0, 0, 0, 1); data = append(data, nal...) }
	if changed || n.transform == nil {
		w, h, err := spsDimensions(n.d.codec, n.sets[sps])
		if err != nil {
			return err
		}
		if w <= 0 || h <= 0 || w > 16384 || h > 16384 || int64(w)*int64(h) > 64*1024*1024 {
			return errors.New("video: invalid hardware decode dimensions")
		}
		if err := n.configure(w, h); err != nil {
			return err
		}
		if n.d.codec == HEVC {
			appendNAL(n.sets[32])
		}
		appendNAL(n.sets[sps])
		appendNAL(n.sets[pps])
	}
	for _, nal := range nals {
		appendNAL(nal)
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > 64*1024*1024 {
		return errors.New("video: access unit is too large")
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for n.events != nil && n.credits == 0 {
		if err := n.pump(); err != nil {
			return err
		}
		if n.credits > 0 {
			break
		}
		select {
		case <-n.d.stop:
			return errors.New("video: decoder is closed")
		case <-deadline.C:
			return errors.New("video: hardware MFT timed out waiting for input")
		case <-time.After(2 * time.Millisecond):
		}
	}
	sample, err := n.makeSample(data)
	if err != nil {
		return err
	}
	defer sample.release()
	if err := mfCheck("ProcessInput", n.transform.call(24, uintptr(n.inputID), uintptr(unsafe.Pointer(sample)), 0)); err != nil {
		return err
	}
	if n.events != nil {
		n.credits--
	}
	return n.pump()
}

func (n *mfNative) makeSample(data []byte) (*mfCOM, error) {
	var sample, buffer *mfCOM
	if err := mfProc(mfSample, uintptr(unsafe.Pointer(&sample))); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			sample.release()
		}
	}()
	if err := mfProc(mfMemoryBuffer, uintptr(len(data)), uintptr(unsafe.Pointer(&buffer))); err != nil {
		return nil, err
	}
	defer buffer.release()
	var ptr *byte
	if err := mfCheck("buffer Lock", buffer.call(3, uintptr(unsafe.Pointer(&ptr)), 0, 0)); err != nil {
		return nil, err
	}
	copy(unsafe.Slice(ptr, len(data)), data)
	if err := mfCheck("buffer Unlock", buffer.call(4)); err != nil {
		return nil, err
	}
	for _, err := range []error{
		mfCheck("SetCurrentLength", buffer.call(6, uintptr(len(data)))),
		mfCheck("AddBuffer", sample.call(42, uintptr(unsafe.Pointer(buffer)))),
		mfCheck("SetSampleTime", sample.call(36, uintptr(n.pts))),
		mfCheck("SetSampleDuration", sample.call(38, 166667)),
	} {
		if err != nil {
			return nil, err
		}
	}
	n.pts += 166667
	ok = true
	return sample, nil
}

func (n *mfNative) pump() error {
	if n.processor != nil {
		if err := n.processor.poll(); err != nil {
			return err
		}
		if n.latestSample != nil {
			for i := range n.processor.slots {
				if n.processor.slots[i].emit != nil {
					continue
				}
				sample := n.latestSample
				n.latestSample = nil
				err := n.picture(sample)
				sample.release()
				if err != nil {
					return err
				}
				break
			}
		}
	}
	if n.events == nil {
		if n.transform != nil {
			for range 128 {
				produced, err := n.output()
				if err != nil {
					return err
				}
				if !produced {
					break
				}
			}
		}
		return nil
	}
	// Bound a poll so a broken driver cannot starve input or cancellation.
	for range 128 {
		var event *mfCOM
		hr := n.events.call(3, 1, uintptr(unsafe.Pointer(&event))) // MF_EVENT_FLAG_NO_WAIT
		if hr == mfNoEvents {
			return nil
		}
		if err := mfCheck("GetEvent", hr); err != nil {
			return err
		}
		var typ, status uint32
		err := mfCheck("event GetType", event.call(33, uintptr(unsafe.Pointer(&typ))))
		if err == nil {
			err = mfCheck("event GetStatus", event.call(35, uintptr(unsafe.Pointer(&status))))
		}
		event.release()
		if err != nil {
			return err
		}
		if err := mfCheck("decoder event", status); err != nil {
			return err
		}
		switch typ {
		case 601:
			n.credits++ // METransformNeedInput
		case 602:
			if _, err := n.output(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (n *mfNative) output() (bool, error) {
	for range 8 { // a stream-change event must be followed by ProcessOutput again
		out := struct {
			ID     uint32
			Sample *mfCOM
			Status uint32
			Events *mfCOM
		}{ID: n.outputID}
		var status uint32
		hr := n.transform.call(25, 0, 1, uintptr(unsafe.Pointer(&out)), uintptr(unsafe.Pointer(&status)))
		out.Events.release()
		if hr == mfStreamChange {
			out.Sample.release()
			if err := n.outputType(); err != nil {
				return false, err
			}
			continue
		}
		if hr == mfNeedMoreInput {
			out.Sample.release()
			return false, nil
		}
		if err := mfCheck("ProcessOutput", hr); err != nil {
			out.Sample.release()
			return false, err
		}
		if out.Sample == nil {
			return false, nil
		}
		err := n.picture(out.Sample)
		out.Sample.release()
		return true, err
	}
	return false, errors.New("video: repeated hardware decoder output format changes")
}

func (n *mfNative) picture(sample *mfCOM) error {
	started := time.Now()
	// Newer pictures supersede deferred presentation, but retaining the
	// newest sample guarantees the last still picture eventually appears.
	n.latestSample.release()
	n.latestSample = nil
	var buffer *mfCOM
	if err := mfCheck("GetBufferByIndex", sample.call(40, 0, uintptr(unsafe.Pointer(&buffer)))); err != nil {
		return err
	}
	defer buffer.release()
	dxgi, err := buffer.query(&iidDXGIBuffer)
	if err != nil {
		return fmt.Errorf("video: hardware MFT did not return a GPU texture: %w", err)
	}
	defer dxgi.release()
	var texture *mfCOM
	if err := mfCheck("GetResource", dxgi.call(3, uintptr(unsafe.Pointer(&iidTexture)), uintptr(unsafe.Pointer(&texture)))); err != nil {
		return err
	}
	defer texture.release()
	var sub uint32
	if err := mfCheck("GetSubresourceIndex", dxgi.call(4, uintptr(unsafe.Pointer(&sub)))); err != nil {
		return err
	}
	var desc mfTextureDesc
	texture.call(10, uintptr(unsafe.Pointer(&desc)))
	if desc.BindFlags&0x200 == 0 {
		return errors.New("video: output texture is not a D3D11 decoder surface")
	}
	if desc.Format != 103 || int(desc.Width) < n.w || int(desc.Height) < n.h || desc.Width > 16384 || desc.Height > 16384 {
		return errors.New("video: invalid NV12 hardware texture")
	}
	if !n.processorFailed {
		if n.processor == nil {
			n.processor = &mfVideoProcessor{owner: n}
		}
		err := n.processor.enqueue(texture, desc, sub, func(f Frame) { n.deliver(f, "D3D11 video processor (async)", started) })
		if errors.Is(err, errMFReadbackBusy) {
			sample.call(1)
			n.latestSample = sample
			return nil
		}
		if err == nil {
			return nil
		}
		// Some older drivers cannot process NV12. Keep decoding on the GPU
		// and expose CPU conversion explicitly rather than losing the session.
		n.processor.close()
		n.processor = nil
		n.processorFailed = true
	}
	desc.MipLevels, desc.ArraySize = 1, 1
	desc.Usage, desc.BindFlags, desc.CPUAccessFlags, desc.MiscFlags = 3, 0, 0x20000, 0 // staging, CPU read
	if n.staging == nil || n.stageDesc != desc {
		n.staging.release()
		n.staging = nil
		if err := mfCheck("CreateTexture2D", n.device.call(5, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&n.staging)))); err != nil {
			return err
		}
		n.stageDesc = desc
	}
	n.context.call(46, uintptr(unsafe.Pointer(n.staging)), 0, 0, 0, 0, uintptr(unsafe.Pointer(texture)), uintptr(sub), 0)
	mapped := struct {
		Data                 *byte
		RowPitch, DepthPitch uint32
	}{}
	if err := mfCheck("texture Map", n.context.call(14, uintptr(unsafe.Pointer(n.staging)), 0, 1, 0, uintptr(unsafe.Pointer(&mapped)))); err != nil {
		return err
	}
	defer n.context.call(15, uintptr(unsafe.Pointer(n.staging)), 0)
	if mapped.RowPitch < desc.Width || mapped.RowPitch > 1024*1024 || mapped.Data == nil {
		return errors.New("video: invalid mapped NV12 texture")
	}
	length := int(mapped.RowPitch) * (int(desc.Height) + (int(desc.Height)+1)/2)
	if cap(n.pixels) < n.w*n.h*4 {
		n.pixels = make([]byte, n.w*n.h*4)
	}
	n.pixels = n.pixels[:n.w*n.h*4]
	mfNV12ToBGRA(n.pixels, unsafe.Slice(mapped.Data, length), n.w, n.h, int(mapped.RowPitch), int(desc.Height), n.matrix, n.nominal)
	n.deliver(Frame{W: n.w, H: n.h, Stride: n.w * 4, Pix: n.pixels}, "CPU", started)
	return nil
}

func (n *mfNative) deliver(f Frame, conversion string, started time.Time) {
	n.d.mu.Lock()
	n.d.status.HardwareKnown, n.d.status.Hardware, n.d.status.Error = true, true, ""
	n.d.status.Conversion = conversion
	n.d.status.OutputTimeMS = float64(time.Since(started)) / float64(time.Millisecond)
	n.d.mu.Unlock()
	if n.d.onFrame != nil {
		n.d.onFrame(f)
	}
}

// D3D11 NV12 planes use the texture's padded height and row pitch, not the
// SPS's visible crop. Honor the output type's SDR matrix and nominal range.
func mfNV12ToBGRA(dst, src []byte, w, h, pitch, textureHeight int, matrix, nominal uint32) {
	clamp := func(v int) byte {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return byte(v)
	}
	yscale, offset := 298, 16
	rv, gu, gv, bu := 409, 100, 208, 516 // limited-range BT.601
	if matrix == 1 {
		rv, gu, gv, bu = 459, 55, 136, 541
	} // BT.709
	if nominal == 1 {
		yscale, offset = 256, 0
		rv, gu, gv, bu = 359, 88, 183, 454
		if matrix == 1 {
			rv, gu, gv, bu = 403, 48, 120, 475
		}
	}
	uv := pitch * textureHeight
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			luma := (int(src[y*pitch+x]) - offset) * yscale
			u := int(src[uv+(y/2)*pitch+(x&^1)]) - 128
			v := int(src[uv+(y/2)*pitch+(x&^1)+1]) - 128
			i := (y*w + x) * 4
			dst[i], dst[i+1], dst[i+2], dst[i+3] = clamp((luma+bu*u+128)>>8), clamp((luma-gu*u-gv*v+128)>>8), clamp((luma+rv*v+128)>>8), 255
		}
	}
}
