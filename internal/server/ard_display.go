package server

import (
	"errors"
	"github.com/dyike/mirage-server/internal/displaymeta"
	"image"
	"log"
	"math"
)

// Apple DisplayInfo2 has distinct point and framebuffer coordinate systems.
// In particular its record +0 double is not reserved: it is the backing scale.
func ardLayoutBody(d DesktopLayout, available []DesktopScreen) []byte {
	catalog := available
	if len(catalog) == 0 {
		catalog = d.Screens
	}
	var whole, selected image.Rectangle
	ids := map[uint32]bool{}
	for _, s := range d.Screens {
		ids[s.ID] = true
	}
	for _, s := range catalog {
		whole = whole.Union(s.Bounds)
		if ids[s.ID] {
			selected = selected.Union(s.Bounds)
		}
	}
	if selected.Empty() {
		selected = whole
	}
	current := uint32(0xffffffff)
	if len(d.Screens) == 1 {
		current = d.Screens[0].ID
	}
	b := make([]byte, 20+56*len(catalog))
	networkOrder.PutUint16(b, 5)
	networkOrder.PutUint16(b[2:], uint16(selected.Dx()))
	networkOrder.PutUint16(b[4:], uint16(selected.Dy()))
	networkOrder.PutUint16(b[6:], uint16(d.Size.X))
	networkOrder.PutUint16(b[8:], uint16(d.Size.Y))
	networkOrder.PutUint32(b[10:], current)
	networkOrder.PutUint32(b[14:], 4) // on-console, physical displays
	networkOrder.PutUint16(b[18:], uint16(len(catalog)))
	sx := float64(d.Size.X) / float64(selected.Dx())
	sy := float64(d.Size.Y) / float64(selected.Dy())
	for i, s := range catalog {
		r := b[20+i*56:]
		networkOrder.PutUint64(r, math.Float64bits(sx))
		networkOrder.PutUint64(r[8:], math.Float64bits(1))
		networkOrder.PutUint32(r[16:], s.ID)
		logical := s.Bounds.Sub(whole.Min)
		backing := image.Rect(int(math.Round(float64(logical.Min.X)*sx)), int(math.Round(float64(logical.Min.Y)*sy)), int(math.Round(float64(logical.Max.X)*sx)), int(math.Round(float64(logical.Max.Y)*sy)))
		for j, rect := range []image.Rectangle{logical, backing} {
			at := 20 + j*8
			networkOrder.PutUint16(r[at:], uint16(rect.Min.Y))
			networkOrder.PutUint16(r[at+2:], uint16(rect.Min.X))
			networkOrder.PutUint16(r[at+4:], uint16(rect.Max.Y))
			networkOrder.PutUint16(r[at+6:], uint16(rect.Max.X))
		}
		if i == 0 {
			networkOrder.PutUint32(r[36:], 1)
		}
		copy(r[40:], defaultRFBFormat[:])
	}
	return b
}

// Standard framebuffer rectangles are already in transmitted pixel coordinates.
// Describe that same coordinate space to the viewer instead of asking it to
// apply a second HiDPI transform derived from the compositor's logical layout.
func ardPixelLayoutBody(d DesktopLayout, available []DesktopScreen) []byte {
	b := ardLayoutBody(d, available)
	copy(b[2:6], b[6:10])
	for i := 0; i < int(networkOrder.Uint16(b[18:])); i++ {
		r := b[20+i*56:]
		networkOrder.PutUint64(r, math.Float64bits(1))
		copy(r[20:28], r[28:36])
	}
	return b
}

// Consume all descriptors before applying anything. Client sizes are requests,
// bounded by our encoding budget; they never reconfigure the physical outputs.
func ardRequestedCanvas(b []byte) (image.Point, error) {
	invalid := errors.New("invalid ARD display configuration")
	if len(b) < 8 || networkOrder.Uint16(b) != 1 {
		return image.Point{}, invalid
	}
	count := int(networkOrder.Uint16(b[2:]))
	if count < 1 || count > 16 {
		return image.Point{}, invalid
	}
	at := 8
	var requested image.Point
	for i := 0; i < count; i++ {
		if len(b)-at < 156 {
			return image.Point{}, invalid
		}
		d := b[at:]
		size, n := int(networkOrder.Uint16(d)), int(networkOrder.Uint16(d[154:]))
		preferred := int(networkOrder.Uint16(d[148:]))
		if size < 156+n*28 || size > len(d) || n == 0 || preferred >= n {
			return image.Point{}, invalid
		}
		mode := d[156+preferred*28:]
		w, h := networkOrder.Uint32(mode), networkOrder.Uint32(mode[4:])
		if w < 64 || h < 64 || w > 8192 || h > 8192 {
			return image.Point{}, invalid
		}
		if i == 0 {
			requested = image.Pt(int(w), int(h))
		}
		at += size
	}
	if at != len(b) {
		return image.Point{}, invalid
	}
	return requested, nil
}

func (p *rfbPeer) ardConfigureDisplay() error {
	h, err := readBytes(p.conn, 3)
	if err != nil {
		return err
	}
	b, err := readBytes(p.conn, int(networkOrder.Uint16(h[1:])))
	if err != nil {
		return err
	}
	requested, err := ardRequestedCanvas(b)
	if err != nil {
		return err
	}
	p.state.Lock()
	d := p.state.desktop.clone()
	// Preserve the selected physical desktop's aspect ratio, even if the viewer
	// asks for a differently shaped virtual display. Scale using GPU capture.
	size := requested
	size.X = min(size.X, p.server.Width)
	size.Y = min(size.Y, p.server.Height)
	if size.X*d.Size.Y > size.Y*d.Size.X {
		size.X = size.Y * d.Size.X / d.Size.Y
	} else {
		size.Y = size.X * d.Size.Y / d.Size.X
	}
	size.X = max(64, size.X&^1)
	size.Y = max(64, size.Y&^1)
	if size != d.Size {
		for i := range d.Screens {
			r := d.Screens[i].Bounds
			d.Screens[i].Bounds = image.Rect(r.Min.X*size.X/d.Size.X, r.Min.Y*size.Y/d.Size.Y, r.Max.X*size.X/d.Size.X, r.Max.Y*size.Y/d.Size.Y)
		}
		d.Size = size
		p.state.desktop = d
		p.state.invalidateCaptureLocked()
		p.state.layoutPending = true
	}
	p.state.Unlock()
	log.Printf("ARD display configuration: peer=%s requested=%v actual=%v (physical outputs unchanged)", p.conn.RemoteAddr(), requested, size)
	// A new layout requires fresh SRTP negotiation, including new keys/ports.
	if old := p.ard.media.Swap(nil); old != nil {
		old.close()
		p.ard.close()
		p.ard.socket = nil
		p.ard.audioSocket = nil
		p.ard.offered = false
	}
	p.wakeCapture()
	offers := map[uint32]bool{1105: true}
	if p.advertised[ardMediaEncoding] {
		offers[ardMediaEncoding] = true
	}
	if p.advertised[displaymeta.EncodingWire] {
		offers[displaymeta.EncodingWire] = true
	}
	return p.announceARD(offers)
}

func scaledARDDesktop(base DesktopLayout, scale float64) DesktopLayout {
	d := base.clone()
	d.Size = image.Pt(max(64, int(float64(base.Size.X)*scale)&^1), max(64, int(float64(base.Size.Y)*scale)&^1))
	for i, s := range base.Screens {
		r := s.Bounds
		d.Screens[i].Bounds = image.Rect(r.Min.X*d.Size.X/base.Size.X, r.Min.Y*d.Size.Y/base.Size.Y, r.Max.X*d.Size.X/base.Size.X, r.Max.Y*d.Size.Y/base.Size.Y)
	}
	return d
}

func ardScaledLayoutBody(base, d DesktopLayout, available []DesktopScreen, scale float64) []byte {
	b := ardPixelLayoutBody(base, available)
	networkOrder.PutUint16(b[6:], uint16(d.Size.X))
	networkOrder.PutUint16(b[8:], uint16(d.Size.Y))
	for i := 0; i < int(networkOrder.Uint16(b[18:])); i++ {
		networkOrder.PutUint64(b[20+i*56+8:], math.Float64bits(scale))
	}
	return b
}

func (p *rfbPeer) setARDScale(scale float64) {
	p.state.Lock()
	if p.state.ardScaleBase.Size.X == 0 {
		p.state.ardScaleBase = p.state.desktop.clone()
	}
	base := p.state.ardScaleBase
	// The logical aggregate can exceed the encoder canvas budget. Its
	// geometry remains stable; only the transmitted image is bounded.
	scale = min(scale, float64(p.server.Width)/float64(base.Size.X), float64(p.server.Height)/float64(base.Size.Y))
	d := scaledARDDesktop(base, scale)
	// Account for the encoder's even-pixel alignment in the reported scale.
	p.state.ardScale = float64(d.Size.X) / float64(base.Size.X)
	changed := d.Size != p.state.desktop.Size
	if changed {
		p.state.desktop = d
		p.state.invalidateCaptureLocked()
		p.state.layoutPending = true
	}
	p.state.Unlock()
	log.Printf("ARD scale applied: peer=%s base=%v framebuffer=%v requested=%g", p.conn.RemoteAddr(), base.Size, d.Size, scale)
	if changed {
		p.wakeCapture()
	}
}

// SetDisplay's first body byte selects the aggregate, independently of ID.
// Native Screen Sharing sends {combine=1, id=0}; Mirage also accepts the
// existing all-displays sentinel used by its own clients.
func ardSelectedDisplay(body []byte) uint32 {
	if body[0] != 0 {
		return 0xffffffff
	}
	return networkOrder.Uint32(body[3:])
}

// Called with state locked. Native viewers re-arm after processing DisplayLayout.
func (s *rfbState) armARDFrameLocked() bool {
	if !s.ardAwaitingRefresh || s.ardReady == nil {
		return false
	}
	close(s.ardReady)
	s.ardAwaitingRefresh = false
	return true
}

// Keep each output's unscaled pixel geometry invariant across selection.
// Selecting all displays expands the union; it must not shrink each output's
// bounds in the catalog. The framebuffer budget is applied separately.
func ardStableDesktop(selected DesktopLayout, available []DesktopScreen, reference image.Point) DesktopLayout {
	if len(available) == 0 || available[0].Bounds.Empty() {
		return selected.clone()
	}
	primary := available[0].Bounds.Size()
	ids := map[uint32]bool{}
	for _, screen := range selected.Screens {
		ids[screen.ID] = true
	}
	result := DesktopLayout{}
	var union image.Rectangle
	for _, screen := range available {
		if !ids[screen.ID] {
			continue
		}
		r := screen.Bounds
		screen.Bounds = image.Rect(r.Min.X*reference.X/primary.X, r.Min.Y*reference.Y/primary.Y, r.Max.X*reference.X/primary.X, r.Max.Y*reference.Y/primary.Y)
		result.Screens = append(result.Screens, screen)
		union = union.Union(screen.Bounds)
	}
	if union.Empty() {
		return selected.clone()
	}
	result.Size = union.Size()
	for i := range result.Screens {
		result.Screens[i].Bounds = result.Screens[i].Bounds.Sub(union.Min)
	}
	return result
}
