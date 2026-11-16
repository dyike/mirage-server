package server

import (
	"errors"
	"image"
	"math"

	"github.com/dyike/mirage-server/internal/server/input"
)

// ardEvent consumes a length-delimited Apple input event (message 0x17).
// Layouts match internal/rfb/applescroll.go. The length includes the version
// and subtype, but excludes the four-byte message header.
func (p *rfbPeer) ardEvent() error {
	h, err := readBytes(p.conn, 3)
	if err != nil {
		return err
	}
	n := int(networkOrder.Uint16(h[1:]))
	if n < 4 {
		return errors.New("short ARD event header")
	}
	b, err := readBytes(p.conn, n)
	if err != nil {
		return err
	}
	// Other versions/subtypes include magnify, rotate and gesture boundaries.
	// Consume them without injecting input; they must not desynchronize RFB.
	if networkOrder.Uint16(b) != 1 {
		return nil
	}
	subtype, body := networkOrder.Uint16(b[2:]), b[4:]
	if subtype == 8 {
		if len(body) < 28 {
			return errors.New("short ARD scroll gesture")
		}
		// This accompanies subtype 11, rather than adding another displacement.
		return nil
	}
	if subtype != 11 {
		return nil
	}
	if len(body) < 50 {
		return errors.New("short ARD scroll wheel")
	}
	u32 := func(at int) uint32 { return networkOrder.Uint32(body[at:]) }
	continuous := u32(42)&2 != 0
	delta := func(axis int) float64 {
		points := float64(int32(u32(18 + axis*4)))
		if continuous {
			if points != 0 {
				return -points
			}
			// Fixed-point line deltas preserve movement below one point.
			return -float64(int32(u32(6+axis*4))) * 10 / 65536
		}
		// Non-continuous wheels retain the ordinary RFB click distance.
		lines := int16(networkOrder.Uint16(body[axis*2:]))
		if lines != 0 {
			return -float64(lines) * 15
		}
		return -points
	}
	e := input.Event{Type: "scroll", DX: delta(0), DY: delta(1)}
	phase, momentum := u32(30), u32(34)
	// Momentum deltas are already produced by the client. Do not start a
	// second inertia animation on Linux. End/cancel closes the axis sequence.
	// CG scroll phases use ended=4/cancelled=8; momentum is an enum
	// (begin=1, continue=2, end=3), not the NSEvent phase bitmask.
	e.ScrollStop = continuous && (momentum == 3 || momentum == 0 && phase&(4|8) != 0)
	if err := e.Validate(); err != nil {
		return err
	}
	// Reduce Apple continuous input, including client-generated momentum,
	// without rounding away fractional movement. Validate wire deltas first.
	if continuous {
		e.DX /= 5
		e.DY /= 5
	}
	if e.DX == 0 && e.DY == 0 && !e.ScrollStop {
		return nil
	}
	return p.scrollAt(int(networkOrder.Uint16(body[46:])), int(networkOrder.Uint16(body[48:])), e)
}

func (p *rfbPeer) scrollAt(x, y int, e input.Event) error {
	p.state.Lock()
	d := p.state.presented.clone()
	p.state.Unlock()
	if router, ok := p.controls.(interface {
		ScrollOnScreen(uint32, input.Event) error
	}); ok {
		point := image.Pt(x, y)
		for _, s := range d.Screens {
			if point.In(s.Bounds) {
				e.X = float64(x-s.Bounds.Min.X) / float64(s.Bounds.Dx())
				e.Y = float64(y-s.Bounds.Min.Y) / float64(s.Bounds.Dy())
				return router.ScrollOnScreen(s.ID, e)
			}
		}
		return nil
	}
	if d.Size.X <= 0 || d.Size.Y <= 0 {
		return errors.New("invalid ARD scroll desktop geometry")
	}
	e.X = math.Max(0, math.Min(1, float64(x)/float64(d.Size.X)))
	e.Y = math.Max(0, math.Min(1, float64(y)/float64(d.Size.Y)))
	return p.apply(e)
}
