package rfb

import (
	"math"
	"sync"
	"time"
)

// Smooth scrolling in the Apple dialect. Wheel clicks (pointer events with
// buttons 4 and 5) scroll macOS a line at a time, and smooth trackpad input
// turned into hundreds of them per swipe, which the server worked through
// long after the fingers stopped. Message 0x17 instead carries one event per
// scroll: a continuous scroll-wheel event with point deltas, and a gesture
// scroll event, with the phase of the gesture (began, changed, ended), the
// way a Mac's own trackpad reports it. Layouts follow the open-screen-sharing
// project's notes on ScreenSharing.app.

const (
	msgAppleEvent      = 0x17
	eventGestureScroll = 8
	eventScrollWheel   = 11
	phaseBegan         = 1
	phaseChanged       = 2
	phaseEnded         = 4
	scrollIdleEnd      = 120 * time.Millisecond // no scroll this long ends the gesture
)

type scrollGesture struct {
	mu         sync.Mutex
	active     bool
	x, y       int // wire position of the gesture
	remX, remY float64
	timer      *time.Timer
}

// SmoothScroll reports whether Scroll sends native scroll gestures; when
// false the caller sends wheel clicks with PointerEvent.
func (c *Client) SmoothScroll() bool {
	return c.apple && c.appleAccepts(msgAppleEvent)
}

// Scroll scrolls by dx, dy pixels of the shown picture at (x, y), positive
// toward the bottom and right of the content, as a trackpad does.
func (c *Client) Scroll(x, y int, dx, dy float64) error {
	if !c.SmoothScroll() {
		return protoErr("server takes no scroll gestures")
	}
	// Shown pixels to the Mac's points; macOS's positive deltas scroll up
	// and left.
	k := c.pointsPerPixel()
	px, py := -dx*k, -dy*k
	wx, wy, ok := c.applePointer(x, y)
	if !ok {
		wx, wy = x, y
	}
	g := &c.scroll
	g.mu.Lock()
	phase := uint32(phaseChanged)
	if !g.active {
		phase, g.active, g.remX, g.remY = phaseBegan, true, 0, 0
	}
	g.x, g.y = wx, wy
	ix, iy := math.Round(px+g.remX), math.Round(py+g.remY)
	g.remX += px - ix
	g.remY += py - iy
	if g.timer == nil {
		g.timer = time.AfterFunc(scrollIdleEnd, c.endScroll)
	} else {
		g.timer.Reset(scrollIdleEnd)
	}
	g.mu.Unlock()
	return c.sendScroll(wx, wy, ix, iy, px, py, phase)
}

// endScroll ends the gesture after the scrolling stopped.
func (c *Client) endScroll() {
	g := &c.scroll
	g.mu.Lock()
	if !g.active {
		g.mu.Unlock()
		return
	}
	g.active = false
	x, y := g.x, g.y
	g.mu.Unlock()
	c.sendScroll(x, y, 0, 0, 0, 0, phaseEnded)
}

func (c *Client) sendScroll(x, y int, wholeX, wholeY, px, py float64, phase uint32) error {
	m := scrollWheelEvent(x, y, wholeX, wholeY, phase)
	m = append(m, gestureScrollEvent(x, y, px, py, phase)...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() {
		return nil
	}
	return c.send(m)
}

// pointsPerPixel converts the shown picture's pixels to the Mac's points:
// the main display's logical width over the shown width.
func (c *Client) pointsPerPixel() float64 {
	l := c.layout.Load()
	w := float64(c.size.Load() >> 32)
	if l == nil || len(l.Displays) == 0 || w == 0 || l.Displays[0].Logical.Dx() <= 0 {
		return 1
	}
	return float64(l.Displays[0].Logical.Dx()) / w
}

func appleEvent(subtype uint16, body []byte, x, y int) []byte {
	body = be.AppendUint16(body, uint16(min(max(x, 0), math.MaxUint16)))
	body = be.AppendUint16(body, uint16(min(max(y, 0), math.MaxUint16)))
	m := []byte{msgAppleEvent, 0}
	m = be.AppendUint16(m, uint16(4+len(body)))
	m = be.AppendUint16(m, 1) // version
	m = be.AppendUint16(m, subtype)
	return append(m, body...)
}

// scrollWheelEvent is a continuous (trackpad) scroll-wheel event: line
// deltas, fixed-point line deltas and point deltas (x, y and an unused
// third axis each), the phase, and the continuous flag.
func scrollWheelEvent(x, y int, px, py float64, phase uint32) []byte {
	const pointsPerLine = 10
	clamp := func(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }
	lines := func(p float64) uint16 {
		return uint16(int16(clamp(math.Round(p/pointsPerLine), math.MinInt16, math.MaxInt16)))
	}
	fixed := func(p float64) uint32 {
		return uint32(int32(clamp(p/pointsPerLine*65536, math.MinInt32, math.MaxInt32)))
	}
	points := func(p float64) uint32 { return uint32(int32(clamp(math.Round(p), math.MinInt32, math.MaxInt32))) }
	b := be.AppendUint16(nil, lines(px))
	b = be.AppendUint16(b, lines(py))
	b = be.AppendUint16(b, 0)
	b = be.AppendUint32(b, fixed(px))
	b = be.AppendUint32(b, fixed(py))
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, points(px))
	b = be.AppendUint32(b, points(py))
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, phase)
	b = be.AppendUint32(b, 0) // no momentum: the local system already eases out
	b = be.AppendUint32(b, 0) // click count 0 and flag 2: continuous
	b = be.AppendUint32(b, 2)
	return appleEvent(eventScrollWheel, b, x, y)
}

// gestureScrollEvent is the gesture companion: float point deltas and the
// phase.
func gestureScrollEvent(x, y int, px, py float64, phase uint32) []byte {
	b := be.AppendUint32(nil, math.Float32bits(float32(px)))
	b = be.AppendUint32(b, math.Float32bits(float32(py)))
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, phase)
	b = be.AppendUint32(b, 0)
	return appleEvent(eventGestureScroll, b, x, y)
}
