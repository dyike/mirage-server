package rfb

import (
	"image"
	"io"
)

// dynamicResolutionEncoding is Mirage's dedicated-desktop capability.
const dynamicResolutionEncoding int32 = -13054

// DesktopScreen is a standard RFB screen in framebuffer coordinates.
// IDs and flags are opaque; neither implies Apple's display selection support.
type DesktopScreen struct {
	ID     uint32
	Bounds image.Rectangle
	Flags  uint32
}

type DesktopLayout struct {
	DynamicResolution bool
	Size              image.Point
	Screens           []DesktopScreen
}

// Desktop returns a snapshot of the last accepted ExtendedDesktopSize layout.
// A nil result means the server has not provided this extension.
func (c *Client) Desktop() *DesktopLayout {
	l := c.standardLayout.Load()
	if l == nil {
		return nil
	}
	copy := *l
	copy.DynamicResolution = c.dynamicResolution.Load()
	copy.Screens = append([]DesktopScreen(nil), l.Screens...)
	return &copy
}

func (c *Client) readExtendedDesktopSize(r image.Rectangle) error {
	var h [4]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	l := &DesktopLayout{Size: r.Size(), Screens: make([]DesktopScreen, int(h[0]))}
	for i := range l.Screens {
		var b [16]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return err
		}
		x, y := int(be.Uint16(b[4:])), int(be.Uint16(b[6:]))
		w, h := int(be.Uint16(b[8:])), int(be.Uint16(b[10:]))
		l.Screens[i] = DesktopScreen{ID: be.Uint32(b[:]), Bounds: image.Rect(x, y, x+w, y+h), Flags: be.Uint32(b[12:])}
	}
	// Rejected requests still carry a payload, but its contents are undefined.
	if r.Min.X == 1 && r.Min.Y != 0 {
		return nil
	}
	c.standardLayout.Store(l)
	if c.cfg.OnDesktop != nil {
		c.cfg.OnDesktop(*c.Desktop())
	}
	c.trace("RFB desktop version=%s name=%q size=%v screens=%+v", c.Version, c.Name, l.Size, l.Screens)
	return nil
}

// Resize follows the viewer only when the server explicitly offers a dedicated desktop.
func (c *Client) Resize(w, h int, _ uint32) error {
	if !c.dynamicResolution.Load() || w < 200 || h < 200 || w > 8192 || h > 8192 {
		return nil
	}
	w, h = w&^1, h&^1
	l := c.Desktop()
	if l == nil || len(l.Screens) != 1 || l.Size == image.Pt(w, h) {
		return nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() || c.resizeRequested == image.Pt(w, h) {
		return nil
	}
	b := make([]byte, 24)
	b[0], b[6] = 251, 1
	be.PutUint16(b[2:], uint16(w))
	be.PutUint16(b[4:], uint16(h))
	be.PutUint32(b[8:], l.Screens[0].ID)
	be.PutUint16(b[16:], uint16(w))
	be.PutUint16(b[18:], uint16(h))
	be.PutUint32(b[20:], l.Screens[0].Flags)
	if err := c.send(b); err != nil {
		return err
	}
	c.resizeRequested = image.Pt(w, h)
	return nil
}
