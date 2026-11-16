package input

import (
	"errors"
	"image"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

type action struct {
	kind  string
	code  uint32
	down  bool
	x, y  float64
	step  int32
	masks [4]uint32
}
type recorder struct {
	actions []action
	mapErr  error
}

func (r *recorder) motion(x, y float64) error {
	r.actions = append(r.actions, action{kind: "motion", x: x, y: y})
	return nil
}
func (r *recorder) button(c uint32, d bool) error {
	r.actions = append(r.actions, action{kind: "button", code: c, down: d})
	return nil
}
func (r *recorder) axis(c uint32, v float64, s int32) error {
	r.actions = append(r.actions, action{kind: "axis", code: c, x: v, step: s})
	return nil
}
func (r *recorder) frame() error { r.actions = append(r.actions, action{kind: "frame"}); return nil }
func (r *recorder) axisStop(axis uint32) error {
	r.actions = append(r.actions, action{kind: "axis-stop", code: axis})
	return nil
}
func (r *recorder) key(c uint32, d bool) error {
	r.actions = append(r.actions, action{kind: "key", code: c, down: d})
	return nil
}
func (r *recorder) mods(d, l, k, g uint32) error {
	r.actions = append(r.actions, action{kind: "mods", masks: [4]uint32{d, l, k, g}})
	return nil
}
func (r *recorder) keymap(b []byte) error {
	if len(b) < 2 || b[len(b)-1] != 0 {
		return errors.New("invalid keymap")
	}
	return r.mapErr
}
func (r *recorder) close() error { return nil }
func testController(t *testing.T, layout string) (*Controller, *recorder) {
	t.Helper()
	r := new(recorder)
	k, e := newKeyboard(r, layout, "")
	if e != nil {
		t.Fatal(e)
	}
	c := &Controller{native: r, keyboard: k}
	t.Cleanup(func() { c.Close() })
	return c, r
}
func apply(t *testing.T, c *Controller, e Event) {
	t.Helper()
	if err := c.Apply(e); err != nil {
		t.Fatal(err)
	}
}
func TestTransforms(t *testing.T) {
	want := [][2]float64{{.2, .3}, {.3, .8}, {.8, .7}, {.7, .2}, {.8, .3}, {.3, .2}, {.2, .7}, {.7, .8}}
	for i, w := range want {
		x, y := TransformPoint(.2, .3, uint32(i))
		if math.Abs(x-w[0]) > 1e-9 || math.Abs(y-w[1]) > 1e-9 {
			t.Fatalf("transform %d: %v,%v", i, x, y)
		}
	}
}
func TestButtonsWheelAndDisconnectRelease(t *testing.T) {
	c, r := testController(t, "us")
	apply(t, c, Event{Type: "pointer", X: .5, Y: .25, Buttons: 1 | 2 | 4 | 128 | 256 | 8})
	for _, a := range r.actions {
		if a.kind == "axis" {
			t.Fatal("wheel fired on press")
		}
	}
	r.actions = nil
	apply(t, c, Event{Type: "pointer", X: .5, Y: .25, Buttons: 1 | 2 | 4 | 128 | 256})
	if !reflect.DeepEqual(r.actions, []action{{kind: "axis", x: -15, step: -1}, {kind: "frame"}}) {
		t.Fatalf("release wheel: %+v", r.actions)
	}
	apply(t, c, Event{Type: "key", Symbol: 0xffe3, Down: true})
	r.actions = nil
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	count := 0
	keyUp := false
	for _, a := range r.actions {
		if a.kind == "button" {
			count++
			if a.down {
				t.Fatal("press on close")
			}
		}
		if a.kind == "key" && a.code == 29 && !a.down {
			keyUp = true
		}
		if a.kind == "axis" {
			t.Fatal("wheel on close")
		}
	}
	if count != 5 || !keyUp {
		t.Fatalf("held state not released: %+v", r.actions)
	}
	if err := c.Apply(Event{Type: "reset"}); err == nil {
		t.Fatal("closed session accepts input")
	}
}
func TestWheelDirections(t *testing.T) {
	c, r := testController(t, "us")
	for i := 0; i < 4; i++ {
		apply(t, c, Event{Type: "pointer", Buttons: uint16(8 << i)})
		r.actions = nil
		apply(t, c, Event{Type: "pointer"})
		a := r.actions[0]
		sign := int32(1)
		if i%2 == 0 {
			sign = -1
		}
		if a.kind != "axis" || a.code != uint32(i/2) || a.step != sign || a.x != float64(sign)*15 {
			t.Fatalf("wheel %d: %+v", i, a)
		}
	}
}
func TestKeyboardModifiersDedupAndPhysical(t *testing.T) {
	c, r := testController(t, "us")
	for _, sym := range []uint32{0xffe3, 'c', 'c'} {
		apply(t, c, Event{Type: "key", Symbol: sym, Down: true})
	}
	keys := 0
	for _, a := range r.actions {
		if a.kind == "key" {
			keys++
		}
	}
	if keys != 2 || c.keyboard.masks()[0]&4 == 0 {
		t.Fatalf("Ctrl+C/dedup: %+v", r.actions)
	}
	apply(t, c, Event{Type: "reset"})
	if len(c.keyboard.held) != 0 || c.keyboard.masks()[0] != 0 {
		t.Fatal("reset retained modifiers")
	}
	r.actions = nil
	apply(t, c, Event{Type: "key", Symbol: 'A', Down: true})
	apply(t, c, Event{Type: "key", Symbol: 'A'})
	shifted := false
	for _, a := range r.actions {
		if a.kind == "mods" && a.masks[0]&1 != 0 {
			shifted = true
		}
	}
	if !shifted || c.keyboard.masks()[0] != 0 {
		t.Fatalf("uppercase modifier restoration: %+v", r.actions)
	}
	r.actions = nil
	apply(t, c, Event{Type: "keycode", Code: 30, Down: true})
	apply(t, c, Event{Type: "keycode", Code: 30, Down: true})
	apply(t, c, Event{Type: "keycode", Code: 30})
	if !reflect.DeepEqual(r.actions, []action{{kind: "key", code: 30, down: true}, {kind: "key", code: 30}}) {
		t.Fatalf("physical code: %+v", r.actions)
	}
	apply(t, c, Event{Type: "key", Symbol: 0xffe5, Down: true})
	apply(t, c, Event{Type: "key", Symbol: 0xffe5})
	if c.LEDs()&4 == 0 {
		t.Fatal("Caps LED missing")
	}
	apply(t, c, Event{Type: "key", Symbol: 0x100ffff, Down: true}) // Unmapped symbol must not kill a session.
}
func TestKeyboardLayoutAndErrors(t *testing.T) {
	c, _ := testController(t, "us,ru")
	apply(t, c, Event{Type: "key", Symbol: 0x6c6, Down: true}) // Cyrillic ef
	if c.keyboard.group() != 1 {
		t.Fatal("did not select symbol's layout")
	}
	apply(t, c, Event{Type: "key", Symbol: 'a', Down: true})
	apply(t, c, Event{Type: "reset"})
	if len(c.keyboard.held) != 0 {
		t.Fatal("layout change stranded key")
	}
	if _, err := newKeyboard(&recorder{mapErr: errors.New("keymap transfer failed")}, "us", ""); err == nil {
		t.Fatal("missing keymap error")
	}
	if _, err := newKeyboard(&recorder{}, "mirage-invalid-layout", ""); err == nil {
		t.Fatal("invalid layout accepted")
	}
	for _, e := range []Event{{Type: "pointer", X: math.NaN()}, {Type: "pointer", Buttons: 512}, {Type: "scroll", DY: 4097}, {Type: "keycode", Code: 768}, {Type: "key"}, {Type: "invalid"}} {
		if e.Validate() == nil {
			t.Fatalf("invalid event accepted: %+v", e)
		}
	}
}

// Opt-in: creates and destroys virtual devices, without moving the pointer or typing.
func TestLiveWaylandLifecycle(t *testing.T) {
	name := os.Getenv("MIRAGE_TEST_INPUT_OUTPUT")
	if name == "" {
		t.Skip("set MIRAGE_TEST_INPUT_OUTPUT for compositor integration")
	}
	c, e := Open(Options{Output: name, Layout: "us"})
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	if e = c.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestCrossScreenDragKeepsButtonDown(t *testing.T) {
	c, r := testController(t, "us")
	m := &MultiController{controller: c, outputs: map[uint32]image.Rectangle{1: image.Rect(-100, 0, 0, 100), 2: image.Rect(0, 0, 100, 100)}, desktop: image.Rect(-100, 0, 100, 100)}
	if err := m.PointerOnScreen(1, .5, .5, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.PointerOnScreen(2, .5, .5, 1); err != nil {
		t.Fatal(err)
	}
	presses, releases := 0, 0
	for _, a := range r.actions {
		if a.kind == "button" {
			if a.down {
				presses++
			} else {
				releases++
			}
		}
	}
	if presses != 1 || releases != 0 {
		t.Fatalf("drag split across devices: %+v", r.actions)
	}
	if c.x != .75 || c.y != .5 {
		t.Fatalf("logical coordinate mismatch %v %v", c.x, c.y)
	}
}

func TestContinuousScrollPreservesFractionAndStops(t *testing.T) {
	r := new(recorder)
	c := &Controller{native: r}
	if err := c.Apply(Event{Type: "scroll", X: .25, Y: .5, DX: .25, DY: -.125, ScrollStop: true}); err != nil {
		t.Fatal(err)
	}
	want := []action{{kind: "motion", x: .25, y: .5}, {kind: "axis", code: 0, x: -.125}, {kind: "axis-stop", code: 0}, {kind: "axis", code: 1, x: .25}, {kind: "axis-stop", code: 1}, {kind: "frame"}}
	if !reflect.DeepEqual(r.actions, want) {
		t.Fatalf("scroll sequence: %+v", r.actions)
	}
}
func TestContinuousScrollOnSecondOutput(t *testing.T) {
	r := new(recorder)
	c := &Controller{native: r}
	m := &MultiController{controller: c, outputs: map[uint32]image.Rectangle{2: image.Rect(0, 0, 100, 100)}, desktop: image.Rect(-100, 0, 100, 100)}
	if err := m.ScrollOnScreen(2, Event{Type: "scroll", X: .5, Y: .25, DY: .125}); err != nil {
		t.Fatal(err)
	}
	if c.x != .75 || c.y != .25 {
		t.Fatalf("wrong logical position: %v %v", c.x, c.y)
	}
	if r.actions[1].kind != "axis" || r.actions[1].x != .125 || r.actions[1].step != 0 {
		t.Fatalf("fractional axis lost: %+v", r.actions)
	}
}

func TestScrollEndDoesNotRestoreStalePointerPosition(t *testing.T) {
	for _, mask := range []uint16{0, 1} {
		c, r := testController(t, "us")
		apply(t, c, Event{Type: "scroll", X: .2, Y: .3, DY: 10})
		apply(t, c, Event{Type: "pointer", X: .8, Y: .7, Buttons: mask})
		r.actions = nil
		// A delayed ARD end event still carries the earlier scroll coordinates.
		apply(t, c, Event{Type: "scroll", X: .2, Y: .3, ScrollStop: true})
		if !reflect.DeepEqual(r.actions, []action{{kind: "axis-stop", code: 0}, {kind: "frame"}}) {
			t.Fatalf("scroll end injected movement or buttons (held=%d): %+v", mask, r.actions)
		}
		if c.x != .8 || c.y != .7 || c.mask != mask {
			t.Fatal("scroll end changed pointer state")
		}
		r.actions = nil
		apply(t, c, Event{Type: "scroll", X: .2, Y: .3, ScrollStop: true})
		if !reflect.DeepEqual(r.actions, []action{{kind: "frame"}}) {
			t.Fatalf("duplicate end moved pointer: %+v", r.actions)
		}
	}
}
