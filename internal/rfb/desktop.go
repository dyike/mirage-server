package rfb

import (
	"image"
	"io"
)

// DesktopScreen is a standard RFB screen in framebuffer coordinates.
// IDs and flags are opaque; neither implies Apple's display selection support.
type DesktopScreen struct {
	ID     uint32
	Bounds image.Rectangle
	Flags  uint32
}

type DesktopLayout struct {
	Size    image.Point
	Screens []DesktopScreen
}

// Desktop returns a snapshot of the last accepted ExtendedDesktopSize layout.
// A nil result means the server has not provided this extension.
func (c *Client) Desktop() *DesktopLayout {
	l := c.standardLayout.Load()
	if l == nil {
		return nil
	}
	copy := *l
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
