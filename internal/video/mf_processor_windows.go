//go:build windows && (amd64 || arm64)

package video

import (
	"errors"
	"unsafe"
)

var (
	errMFReadbackBusy = errors.New("video: GPU readback ring is full")
	iidVideoDevice    = mfGUID("10ec4d5b-975a-4689-b9e4-d0aac30fe333")
	iidVideoContext   = mfGUID("61f21c45-3c0e-4a74-9cea-67100d9ad5e4")
)

// Keep color conversion on the GPU. The previous scalar NV12 conversion
// exceeded the entire 60 Hz frame budget at 4K, stalling RTP reception and
// accumulating latency. Only the final BGRA image is mapped for the viewer.
type mfVideoProcessor struct {
	owner                                  *mfNative
	device, context, enumerator, processor *mfCOM
	output, outputView                     *mfCOM
	inputDesc                              mfTextureDesc
	slots                                  [3]mfReadback
	sequence, delivered                    uint64
}

type mfReadback struct {
	texture  *mfCOM
	emit     func(Frame)
	sequence uint64
}

func (p *mfVideoProcessor) close() {
	for i := range p.slots {
		p.slots[i].texture.release()
		p.slots[i] = mfReadback{}
	}
	p.sequence, p.delivered = 0, 0
	p.outputView.release()
	p.outputView = nil
	p.output.release()
	p.output = nil
	p.processor.release()
	p.processor = nil
	p.enumerator.release()
	p.enumerator = nil
	p.context.release()
	p.context = nil
	p.device.release()
	p.device = nil
}

func (p *mfVideoProcessor) init(desc mfTextureDesc) error {
	p.close()
	n := p.owner
	var err error
	p.device, err = n.device.query(&iidVideoDevice)
	if err != nil {
		return err
	}
	p.context, err = n.context.query(&iidVideoContext)
	if err != nil {
		return err
	}
	content := struct {
		FrameFormat, InputRateN, InputRateD, InputWidth, InputHeight uint32
		OutputRateN, OutputRateD, OutputWidth, OutputHeight, Usage   uint32
	}{0, 60, 1, desc.Width, desc.Height, 60, 1, uint32(n.w), uint32(n.h), 1}
	if err := mfCheck("CreateVideoProcessorEnumerator", p.device.call(10, uintptr(unsafe.Pointer(&content)), uintptr(unsafe.Pointer(&p.enumerator)))); err != nil {
		return err
	}
	var flags uint32
	if err := mfCheck("CheckVideoProcessorFormat", p.enumerator.call(8, 87, uintptr(unsafe.Pointer(&flags)))); err != nil {
		return err
	}
	if flags&2 == 0 {
		return errors.New("video: GPU video processor cannot output BGRA")
	}
	if err := mfCheck("CreateVideoProcessor", p.device.call(4, uintptr(unsafe.Pointer(p.enumerator)), 0, uintptr(unsafe.Pointer(&p.processor)))); err != nil {
		return err
	}
	outDesc := mfTextureDesc{Width: uint32(n.w), Height: uint32(n.h), MipLevels: 1, ArraySize: 1, Format: 87, SampleCount: 1, BindFlags: 0x20}
	if err := mfCheck("Create BGRA texture", n.device.call(5, uintptr(unsafe.Pointer(&outDesc)), 0, uintptr(unsafe.Pointer(&p.output)))); err != nil {
		return err
	}
	viewDesc := [4]uint32{1, 0, 0, 0} // D3D11_VPOV_DIMENSION_TEXTURE2D
	if err := mfCheck("CreateVideoProcessorOutputView", p.device.call(9, uintptr(unsafe.Pointer(p.output)), uintptr(unsafe.Pointer(p.enumerator)), uintptr(unsafe.Pointer(&viewDesc)), uintptr(unsafe.Pointer(&p.outputView)))); err != nil {
		return err
	}
	outDesc.Usage, outDesc.BindFlags, outDesc.CPUAccessFlags = 3, 0, 0x20000
	for i := range p.slots {
		if err := mfCheck("Create BGRA staging texture", n.device.call(5, uintptr(unsafe.Pointer(&outDesc)), 0, uintptr(unsafe.Pointer(&p.slots[i].texture)))); err != nil {
			return err
		}
	}
	p.inputDesc = desc
	return nil
}

func (p *mfVideoProcessor) frame(texture *mfCOM, desc mfTextureDesc, sub uint32, emit func(Frame)) error {
	if p.processor == nil || p.inputDesc != desc {
		if err := p.init(desc); err != nil {
			return err
		}
	}
	if err := p.render(texture, desc, sub, p.slots[0].texture); err != nil {
		return err
	}
	_, err := p.mapFrame(p.slots[0].texture, 0, emit)
	return err
}

// enqueue never waits for GPU readback. At most three transfers are in
// flight, and a full ring drops presentation only, never reference decoding.
func (p *mfVideoProcessor) enqueue(texture *mfCOM, desc mfTextureDesc, sub uint32, emit func(Frame)) error {
	if p.processor == nil || p.inputDesc != desc {
		if err := p.init(desc); err != nil {
			return err
		}
	}
	if err := p.poll(); err != nil {
		return err
	}
	for i := range p.slots {
		slot := &p.slots[i]
		if slot.emit != nil {
			continue
		}
		if err := p.render(texture, desc, sub, slot.texture); err != nil {
			return err
		}
		p.sequence++
		slot.sequence, slot.emit = p.sequence, emit
		// A standalone still desktop must reach the GPU without waiting for
		// another input frame to fill D3D11's command buffer.
		p.owner.context.call(111) // Flush submits, but does not wait
		return nil
	}
	return errMFReadbackBusy
}

func (p *mfVideoProcessor) poll() error {
	// Prefer the newest completed transfer. Old transfers must not build
	// a presentation queue when the renderer or GPU stalls temporarily.
	ceiling := ^uint64(0)
	for range len(p.slots) {
		var slot *mfReadback
		for i := range p.slots {
			s := &p.slots[i]
			if s.emit != nil && s.sequence < ceiling && (slot == nil || s.sequence > slot.sequence) {
				slot = s
			}
		}
		if slot == nil {
			return nil
		}
		ceiling = slot.sequence
		ready, err := p.mapFrame(slot.texture, 0x100000, slot.emit) // D3D11_MAP_FLAG_DO_NOT_WAIT
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		p.delivered = slot.sequence
		for i := range p.slots {
			if p.slots[i].sequence <= p.delivered {
				p.slots[i].emit = nil
			}
		}
		return nil
	}
	return nil
}

func (p *mfVideoProcessor) render(texture *mfCOM, desc mfTextureDesc, sub uint32, staging *mfCOM) error {
	n := p.owner
	var input *mfCOM
	// Decode surfaces are commonly an array. The MFT's subresource names
	// both mip and array slice; never silently display the first array item.
	inDesc := [4]uint32{0, 1, sub % desc.MipLevels, sub / desc.MipLevels}
	if err := mfCheck("CreateVideoProcessorInputView", p.device.call(8, uintptr(unsafe.Pointer(texture)), uintptr(unsafe.Pointer(p.enumerator)), uintptr(unsafe.Pointer(&inDesc)), uintptr(unsafe.Pointer(&input)))); err != nil {
		return err
	}
	defer input.release()
	source := [4]int32{0, 0, int32(n.w), int32(n.h)}
	inputSpace := uint32(1 << 4) // studio range
	if n.nominal == 1 {
		inputSpace = 2 << 4
	}
	if n.matrix == 1 {
		inputSpace |= 1 << 2
	}
	outputSpace := uint32(2 << 4) // full-range RGB
	proc := uintptr(unsafe.Pointer(p.processor))
	p.context.call(27, proc, 0, 0) // progressive
	p.context.call(28, proc, 0, uintptr(unsafe.Pointer(&inputSpace)))
	p.context.call(15, proc, uintptr(unsafe.Pointer(&outputSpace)))
	p.context.call(30, proc, 0, 1, uintptr(unsafe.Pointer(&source)))
	p.context.call(31, proc, 0, 1, uintptr(unsafe.Pointer(&source)))
	p.context.call(13, proc, 1, uintptr(unsafe.Pointer(&source)))
	p.context.call(37, proc, 0, 0) // disable automatic quality enhancements
	p.context.call(16, proc, 0, 0) // opaque alpha
	stream := struct {
		Enable, OutputIndex, InputFrameOrField, PastFrames, FutureFrames uint32
		PastSurfaces                                                     *uintptr
		InputSurface                                                     *mfCOM
		FutureSurfaces, PastRight                                        *uintptr
		InputRight                                                       *mfCOM
		FutureRight                                                      *uintptr
	}{Enable: 1, InputSurface: input}
	if err := mfCheck("VideoProcessorBlt", p.context.call(53, proc, uintptr(unsafe.Pointer(p.outputView)), 0, 1, uintptr(unsafe.Pointer(&stream)))); err != nil {
		return err
	}
	n.context.call(47, uintptr(unsafe.Pointer(staging)), uintptr(unsafe.Pointer(p.output)))
	return nil
}

func (p *mfVideoProcessor) mapFrame(staging *mfCOM, flags uintptr, emit func(Frame)) (bool, error) {
	n := p.owner
	mapped := struct {
		Data                 *byte
		RowPitch, DepthPitch uint32
	}{}
	hr := n.context.call(14, uintptr(unsafe.Pointer(staging)), 0, 1, flags, uintptr(unsafe.Pointer(&mapped)))
	if hr == 0x887a000a {
		return false, nil
	} // DXGI_ERROR_WAS_STILL_DRAWING
	if err := mfCheck("BGRA texture Map", hr); err != nil {
		return false, err
	}
	defer n.context.call(15, uintptr(unsafe.Pointer(staging)), 0)
	if mapped.Data == nil || int(mapped.RowPitch) < n.w*4 || mapped.RowPitch > 1024*1024 {
		return false, errors.New("video: invalid mapped BGRA texture")
	}
	emit(Frame{W: n.w, H: n.h, Stride: int(mapped.RowPitch), Pix: unsafe.Slice(mapped.Data, int(mapped.RowPitch)*(n.h-1)+n.w*4)})
	return true, nil
}
