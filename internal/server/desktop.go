package server

import (
	"errors"
	"github.com/dyike/mirage-server/internal/server/input"
	"image"
	"slices"
)

type DesktopScreen struct {
	ID     uint32
	Bounds image.Rectangle
	Flags  uint32
}
type DesktopLayout struct {
	Size    image.Point
	Screens []DesktopScreen
}
type CaptureRequest struct {
	MinQP        int // quality ceiling used to bound interactive video frame sizes
	RepeatIdle   bool
	BindFPS      func(func(int))
	BindKeyframe func(func() bool)
	// BindBitrate registers an in-place encoder update, used on the send goroutine.
	BindBitrate  func(func(int))
	Bitrate, FPS int
	Codec        string
	Desktop      DesktopLayout
}

func (d DesktopLayout) clone() DesktopLayout {
	d.Screens = append([]DesktopScreen(nil), d.Screens...)
	return d
}
func (d DesktopLayout) validate() error {
	if d.Size.X < 64 || d.Size.Y < 64 || d.Size.X > 8192 || d.Size.Y > 8192 || d.Size.X%2 != 0 || d.Size.Y%2 != 0 || len(d.Screens) < 1 || len(d.Screens) > 16 {
		return errors.New("invalid desktop size")
	}
	ids := map[uint32]bool{}
	for _, s := range d.Screens {
		if ids[s.ID] || s.Bounds.Empty() || !s.Bounds.In(image.Rectangle{Max: d.Size}) {
			return errors.New("invalid screen layout")
		}
		ids[s.ID] = true
	}
	return nil
}
func (s *RFBServer) desktop() DesktopLayout {
	if len(s.Screens) != 0 {
		return DesktopLayout{image.Pt(s.Width, s.Height), append([]DesktopScreen(nil), s.Screens...)}
	}
	return DesktopLayout{image.Pt(s.Width, s.Height), []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, s.Width, s.Height)}}}
}
func desktopRectangle(d DesktopLayout, reason, result int) []byte {
	b := rectHeader(image.Rect(reason, result, reason+d.Size.X, result+d.Size.Y), 0xfffffecc)
	b = append(b, byte(len(d.Screens)), 0, 0, 0)
	for _, s := range d.Screens {
		v := make([]byte, 16)
		networkOrder.PutUint32(v, s.ID)
		networkOrder.PutUint16(v[4:], uint16(s.Bounds.Min.X))
		networkOrder.PutUint16(v[6:], uint16(s.Bounds.Min.Y))
		networkOrder.PutUint16(v[8:], uint16(s.Bounds.Dx()))
		networkOrder.PutUint16(v[10:], uint16(s.Bounds.Dy()))
		networkOrder.PutUint32(v[12:], s.Flags)
		b = append(b, v...)
	}
	return b
}
func (p *rfbPeer) resize() error {
	b, err := readBytes(p.conn, 7)
	if err != nil {
		return err
	}
	n := int(b[5])
	if n < 1 || n > 16 {
		return errors.New("invalid screen count")
	}
	bodies, err := readBytes(p.conn, n*16)
	if err != nil {
		return err
	}
	d := DesktopLayout{Size: image.Pt(int(networkOrder.Uint16(b[1:])), int(networkOrder.Uint16(b[3:])))}
	for i := 0; i < n; i++ {
		v := bodies[i*16:]
		x, y := int(networkOrder.Uint16(v[4:])), int(networkOrder.Uint16(v[6:]))
		d.Screens = append(d.Screens, DesktopScreen{networkOrder.Uint32(v), image.Rect(x, y, x+int(networkOrder.Uint16(v[8:])), y+int(networkOrder.Uint16(v[10:]))), networkOrder.Uint32(v[12:])})
	}
	p.state.Lock()
	current := p.state.desktop.clone()
	p.state.Unlock()
	reject := func(result int) error { return p.send(updateMessage(desktopRectangle(current, 1, result))) }
	if !p.advertised[0xfffffecc] || p.server.DesktopSource == nil || p.server.SetDesktop == nil {
		return reject(1)
	}
	if err = d.validate(); err != nil {
		return reject(3)
	}
	allowed := map[uint32]bool{}
	available := p.server.desktop().Screens
	if len(p.server.AvailableScreens) > 0 {
		available = p.server.AvailableScreens

	}
	for _, s := range available {
		allowed[s.ID] = true
	}
	for _, s := range d.Screens {
		if !allowed[s.ID] {
			return reject(3)
		}
	}
	if err = p.server.SetDesktop(p.ctx, d.clone()); err != nil {
		return reject(1)
	}
	if p.server.RefreshInputOnResize {
		if controls, ok := p.controls.(interface{ RefreshOutputs() error }); ok {
			if err := controls.RefreshOutputs(); err != nil {
				return err
			}
		}
	}
	p.state.Lock()
	p.state.desktop = d
	p.state.invalidateCaptureLocked()
	p.state.layoutPending = true
	p.state.Unlock()
	select {
	case p.requests <- struct{}{}:
	default:
	}
	return nil
}
func (p *rfbPeer) pointer(mask uint16, x, y int) error {
	if p.ard != nil {
		// Apple sends right=bit 1 and middle=bit 2. The input controller
		// uses RFB ordering (middle=bit 1, right=bit 2).
		mask = mask&^6 | (mask&2)<<1 | (mask&4)>>1
	}
	p.state.Lock()
	d := p.state.presented.clone()
	if p.ard != nil && p.state.ardScale > 0 {
		d = p.state.ardScaleBase.clone()
	}
	p.state.Unlock()
	if router, ok := p.controls.(interface {
		PointerOnScreen(uint32, float64, float64, uint16) error
	}); ok {
		point := image.Pt(x, y)
		for _, s := range d.Screens {
			if point.In(s.Bounds) {
				return router.PointerOnScreen(s.ID, float64(x-s.Bounds.Min.X)/float64(s.Bounds.Dx()), float64(y-s.Bounds.Min.Y)/float64(s.Bounds.Dy()), mask)
			}
		}
		return nil
	}
	return p.apply(input.Event{Type: "pointer", Buttons: mask, X: float64(x) / float64(d.Size.X), Y: float64(y) / float64(d.Size.Y)})
}

// Selection is per connection. The capture revision restarts the encoder with
// an independent picture; presented geometry follows only when that frame arrives.
func (p *rfbPeer) selectNativeDisplay(id uint32) error {
	if id == 0 {
		id = p.server.AvailableScreens[0].ID
	}
	selected := []DesktopScreen{}
	for _, screen := range p.server.AvailableScreens {
		if id == 0xffffffff || screen.ID == id {
			selected = append(selected, screen)
		}
	}
	if len(selected) == 0 {
		return errors.New("unknown ARD display")
	}
	p.state.Lock()
	previous := p.state.desktop.clone()
	if len(previous.Screens) == len(selected) {
		same := true
		for i := range selected {
			if selected[i].ID != previous.Screens[i].ID {
				same = false
				break
			}
		}
		if same {
			p.state.Unlock()
			return nil
		}
	}
	scale := p.state.ardScale
	if scale > 0 {
		scale = float64(previous.Size.X) / float64(p.server.Width)
	}
	p.state.desktop.Size = image.Pt(p.server.Width, p.server.Height)
	var union image.Rectangle
	for _, s := range selected {
		union = union.Union(s.Bounds)
	}
	if union.Empty() {
		union = image.Rectangle{Max: p.state.desktop.Size}
	}
	if len(selected) > 1 && !union.Empty() {
		// Fit the combined desktop inside the configured encoding budget without
		// stretching two side-by-side screens into a single-screen aspect ratio.
		size := p.state.desktop.Size
		if union.Dx()*size.Y > union.Dy()*size.X {
			size.Y = max(64, (size.X*union.Dy()/union.Dx())&^1)
		} else {
			size.X = max(64, (size.Y*union.Dx()/union.Dy())&^1)
		}
		p.state.desktop.Size = size
	}
	for i := range selected {
		r := selected[i].Bounds.Sub(union.Min)
		if len(selected) == 1 {
			selected[i].Bounds = image.Rectangle{Max: p.state.desktop.Size}
		} else {
			selected[i].Bounds = image.Rect(r.Min.X*p.state.desktop.Size.X/union.Dx(), r.Min.Y*p.state.desktop.Size.Y/union.Dy(), r.Max.X*p.state.desktop.Size.X/union.Dx(), r.Max.Y*p.state.desktop.Size.Y/union.Dy())
		}
	}
	p.state.desktop.Screens = selected
	if scale > 0 || (p.ard != nil && !p.advertised[ardMediaEncoding]) {
		if scale == 0 {
			scale = 1
		}
		target := scaledARDDesktop(p.state.desktop, scale)
		base := ardStableDesktop(p.state.desktop, p.server.AvailableScreens, image.Pt(p.server.Width, p.server.Height))
		p.state.ardScaleBase = base
		p.state.ardScale = float64(target.Size.X) / float64(base.Size.X)
		p.state.desktop = target
	}
	if previous.Size == p.state.desktop.Size && slices.Equal(previous.Screens, p.state.desktop.Screens) {
		p.state.Unlock()
		return nil
	}
	p.state.invalidateCaptureLocked()
	p.state.layoutPending = true
	p.state.Unlock()
	p.wakeCapture()
	return nil
}

// Never send ExtendedDesktopSize unless the viewer advertised it. Apple's
// standard viewer advertises DesktopSize instead and rejects unknown encodings.
func desktopNotification(d DesktopLayout, encoding uint32) []byte {
	switch encoding {
	case 0xfffffecc:
		return desktopRectangle(d, 1, 0)
	case 0xffffff21:
		return rectHeader(image.Rectangle{Max: d.Size}, encoding)
	default:
		return nil
	}
}

// Called with state locked. Cancel idle capture reads immediately on a layout
// change; waiting for the next damaged frame can otherwise stall indefinitely.
func (s *rfbState) invalidateCaptureLocked() {
	s.revision++
	if s.captureCancel != nil {
		s.captureCancel()
	}
}
