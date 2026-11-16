// Input behavior ported from WayVNC; Go implementation maintained by Mirage.
/*
 * Copyright (c) 2019 - 2025 Andri Yngvason
 *
 * Permission to use, copy, modify, and/or distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
 * REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
 * AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
 * INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
 * LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE
 * OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
 * PERFORMANCE OF THIS SOFTWARE.
 *
 * Acknowledgements: Reading Josef Gajdusek's wvnc code helped me understand
 * how to use the xkbcommon API to interface with the wayland virtual keyboard
 * interface.
 */

package input

import (
	"errors"
	"log"
	"sync"
)

type Options struct {
	Output, Seat, Layout, Variant string
	Rules, Model, XKBOptions      string
	Transform                     uint32
}
type Controller struct {
	mu                 sync.Mutex
	native             nativeInput
	keyboard           *keyboard
	mask               uint16
	scrollAxes         [2]bool
	x, y               float64
	positioned, closed bool
	transform          uint32
}

func Open(o Options) (*Controller, error) {
	if o.Transform > 7 {
		return nil, errors.New("invalid output transform")
	}
	n, err := openWayland(o.Output, o.Seat)
	if err != nil {
		return nil, err
	}
	k, err := newKeyboardWithOptions(n, o)
	if err != nil {
		n.close()
		return nil, err
	}
	return &Controller{native: n, keyboard: k, transform: o.Transform}, nil
}
func TransformPoint(x, y float64, t uint32) (float64, float64) {
	x = max(0, min(1, x))
	y = max(0, min(1, y))
	if t >= 4 {
		x = 1 - x
	}
	switch t % 4 {
	case 1:
		return y, 1 - x
	case 2:
		return 1 - x, 1 - y
	case 3:
		return 1 - y, x
	}
	return x, y
}
func (c *Controller) Apply(e Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("input session closed")
	}
	switch e.Type {
	case "key":
		return c.keyboard.symbol(e.Symbol, e.Down)
	case "keycode":
		return c.keyboard.keycode(e.Code+8, e.Down)
	case "reset":
		return c.reset()
	case "pointer":
		return c.pointer(e.X, e.Y, e.Buttons)
	case "scroll":
		// End-only events may arrive after a newer pointer move or press.
		// Their stored scroll coordinates must not move an idle/held pointer.
		// A final event with real deltas still scrolls at its stated position.
		if e.DX != 0 || e.DY != 0 {
			if err := c.move(e.X, e.Y); err != nil {
				return err
			}
		}
		for axis, delta := range []float64{e.DY, e.DX} {
			if delta != 0 {
				if err := c.native.axis(uint32(axis), delta, 0); err != nil {
					return err
				}
				c.scrollAxes[axis] = true
			}
			if e.ScrollStop && c.scrollAxes[axis] {
				if err := c.native.axisStop(uint32(axis)); err != nil {
					return err
				}
				c.scrollAxes[axis] = false
			}
		}
		return c.native.frame()
	}
	return nil
}
func (c *Controller) move(x, y float64) error {
	x, y = TransformPoint(x, y, c.transform)
	if !c.positioned || x != c.x || y != c.y {
		if err := c.native.motion(x, y); err != nil {
			return err
		}
		c.x, c.y, c.positioned = x, y, true
	}
	return nil
}

var buttons = []struct {
	bit  uint16
	code uint32
}{{1, 0x110}, {2, 0x112}, {4, 0x111}, {128, 0x116}, {256, 0x115}}

func (c *Controller) pointer(x, y float64, mask uint16) error {
	if err := c.move(x, y); err != nil {
		return err
	}
	diff := c.mask ^ mask
	for _, b := range buttons {
		if diff&b.bit != 0 {
			if b.bit == 2 {
				log.Printf("RFB paste input: pointer=%p middle-button down=%t", c, mask&b.bit != 0)
			}
			if err := c.native.button(b.code, mask&b.bit != 0); err != nil {
				return err
			}
		}
	}
	for i := 0; i < 4; i++ {
		bit := uint16(1 << (i + 3))
		if diff&bit != 0 && mask&bit == 0 {
			step := int32(1)
			if i%2 == 0 {
				step = -1
			}
			if err := c.native.axis(uint32(i/2), float64(step)*15, step); err != nil {
				return err
			}
		}
	}
	c.mask = mask
	return c.native.frame()
}
func (c *Controller) reset() error {
	err := c.keyboard.reset()
	for _, b := range buttons {
		if c.mask&b.bit != 0 {
			if e := c.native.button(b.code, false); err == nil {
				err = e
			}
		}
	}
	c.mask = 0
	for axis, active := range c.scrollAxes {
		if active {
			if e := c.native.axisStop(uint32(axis)); err == nil {
				err = e
			}
		}
	}
	c.scrollAxes = [2]bool{}
	if e := c.native.frame(); err == nil {
		err = e
	}
	return err
}
func (c *Controller) LEDs() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0
	}
	return c.keyboard.leds()
}
func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	err := c.reset()
	c.closed = true
	c.keyboard.close()
	if e := c.native.close(); err == nil {
		err = e
	}
	return err
}
