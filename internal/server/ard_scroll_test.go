package server

import (
	"bytes"
	"context"
	"image"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/input"
)

// Exercise capability negotiation, encrypted ARD records and both companion
// messages through the real client and server, not just the event parser.
func TestARDScrollConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controls := &testInput{events: make(chan input.Event, 32), closed: make(chan struct{})}
	cfg, _ := testTLS(t)
	srv := RFBServer{TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 200, Height: 100,
		OpenInput: func() (InputSession, error) { return controls, nil },
		Source: func(ctx context.Context, _ string, send func([]byte) error) error {
			if err := send(make([]byte, 200*100*4)); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	ready := make(chan struct{}, 1)
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, Username: "test", Password: "test-password", Encodings: []int32{rfb.EncRaw}, NoAudio: true,
		OnUpdate: func(*frame.Framebuffer, []image.Rectangle) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.SmoothScroll() {
		c.Close()
		t.Fatal("continuous scroll not negotiated")
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("no initial ARD picture")
	}
	if err := c.Scroll(50, 75, 12, -24); err != nil {
		t.Fatal(err)
	}
	for _, want := range []input.Event{{Type: "scroll", X: .25, Y: .75, DX: 2.4, DY: -4.8}, {Type: "scroll", X: .25, Y: .75, ScrollStop: true}} {
		select {
		case got := <-controls.events:
			if got != want {
				t.Fatalf("got %+v want %+v", got, want)
			}
		case <-ctx.Done():
			t.Fatal("scroll timed out")
		}
	}
	// ARD numbers right/middle buttons differently from ordinary RFB.
	for _, mask := range []uint8{1, 0, 2, 0, 4, 0, 7, 0} {
		if err := c.PointerEvent(mask, 160, 20); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-controls.events:
			if got.Type != "pointer" || got.Buttons != uint16(mask) || got.X != .8 || got.Y != .2 {
				t.Fatalf("ARD pointer changed button/position: mask=%d event=%+v", mask, got)
			}
		case <-ctx.Done():
			t.Fatal("pointer timed out")
		}
	}
	// A following key is an ordering barrier: companion messages must neither
	// add a scroll nor consume bytes from the next input message.
	if err := c.KeyEvent('a', true); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-controls.events:
		if got.Type != "key" || got.Symbol != 'a' || !got.Down {
			t.Fatalf("unexpected input after scroll: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("key timed out")
	}
}

type ardEventConn struct {
	net.Conn
	r *bytes.Reader
}

func (c ardEventConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func ardTestEvent(version, subtype uint16, body []byte) []byte {
	b := []byte{23, 0}
	b = networkOrder.AppendUint16(b, uint16(4+len(body)))
	b = networkOrder.AppendUint16(b, version)
	b = networkOrder.AppendUint16(b, subtype)
	return append(b, body...)
}
func ardTestWheel(x, y int, dx, dy float64, phase, momentum uint32) []byte {
	b := make([]byte, 50)
	for i, v := range []float64{dx, dy} {
		networkOrder.PutUint16(b[i*2:], uint16(int16(math.Round(v/10))))
		networkOrder.PutUint32(b[6+i*4:], uint32(int32(v/10*65536)))
		networkOrder.PutUint32(b[18+i*4:], uint32(int32(math.Round(v))))
	}
	networkOrder.PutUint32(b[30:], phase)
	networkOrder.PutUint32(b[34:], momentum)
	networkOrder.PutUint32(b[42:], 2)
	networkOrder.PutUint16(b[46:], uint16(x))
	networkOrder.PutUint16(b[48:], uint16(y))
	return ardTestEvent(1, 11, b)
}
func ardScrollPeer(wire []byte) (*rfbPeer, *testInput, *bytes.Reader) {
	r := bytes.NewReader(wire)
	controls := &testInput{events: make(chan input.Event, 32)}
	return &rfbPeer{conn: ardEventConn{r: r}, controls: controls, state: &rfbState{presented: DesktopLayout{Size: image.Pt(200, 100)}}}, controls, r
}
func readTestARDEvent(t *testing.T, p *rfbPeer) error {
	t.Helper()
	b, err := readBytes(p.conn, 1)
	if err != nil {
		return err
	}
	handled, err := p.readARD(b[0])
	if !handled {
		t.Fatalf("unhandled ARD event %d", b[0])
	}
	return err
}
func TestARDContinuousScroll(t *testing.T) {
	if ardName(nil)[6+23/8]&(0x80>>(23%8)) == 0 {
		t.Fatal("native event capability missing")
	}
	for _, tc := range []struct {
		name            string
		dx, dy          float64
		phase, momentum uint32
		stop            bool
	}{
		{"begin", 12, -24, 1, 0, false},
		{"fraction", .25, -.125, 2, 0, false},
		{"end", 0, 0, 4, 0, true},
		{"cancel", 0, 0, 8, 0, true},
		{"momentum-begin", 3, -2, 4, 1, false},
		{"momentum", 2, -1, 0, 2, false},
		{"momentum-end", 0, 0, 0, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, c, _ := ardScrollPeer(ardTestWheel(50, 75, tc.dx, tc.dy, tc.phase, tc.momentum))
			if err := readTestARDEvent(t, p); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-c.events:
				if got.Type != "scroll" || got.X != .25 || got.Y != .75 || math.Abs(got.DX+tc.dx/5) > .0002 || math.Abs(got.DY+tc.dy/5) > .0002 || got.ScrollStop != tc.stop {
					t.Fatalf("unexpected input: %+v", got)
				}
			default:
				t.Fatal("missing scroll")
			}
		})
	}
}
func TestARDScrollCompanionAndUnknownEventsDoNotRepeatOrDesync(t *testing.T) {
	wire := ardTestWheel(50, 75, 12, -24, 1, 0)
	wire = append(wire, ardTestEvent(1, 8, make([]byte, 28))...)
	wire = append(wire, ardTestEvent(1, 3, make([]byte, 28))...)
	wire = append(wire, ardTestEvent(2, 11, make([]byte, 50))...)
	wire = append(wire, ardTestWheel(50, 75, 0, 0, 2, 0)...)
	wire = append(wire, ardTestWheel(50, 75, -1, 2, 2, 0)...)
	p, c, r := ardScrollPeer(wire)
	for range 6 {
		if err := readTestARDEvent(t, p); err != nil {
			t.Fatal(err)
		}
	}
	if r.Len() != 0 || len(c.events) != 2 {
		t.Fatalf("remaining bytes=%d events=%d", r.Len(), len(c.events))
	}
}
func TestARDScrollMalformed(t *testing.T) {
	oversized := ardTestWheel(0, 0, 4097, 0, 1, 0)
	for _, wire := range [][]byte{
		{23, 0, 0, 3}, {23, 0, 0, 4, 0}, ardTestEvent(1, 11, make([]byte, 49)), ardTestEvent(1, 8, make([]byte, 27)), oversized,
	} {
		p, c, _ := ardScrollPeer(wire)
		if err := readTestARDEvent(t, p); err == nil {
			t.Fatal("malformed message accepted")
		}
		if len(c.events) != 0 {
			t.Fatal("malformed message injected input")
		}
	}
	// Every truncated prefix of a valid event must fail without injecting input.
	wire := ardTestWheel(0, 0, 1, 1, 1, 0)
	for i := 0; i < len(wire); i++ {
		p, c, _ := ardScrollPeer(wire[:i])
		if err := readTestARDEvent(t, p); err == nil {
			t.Fatalf("accepted prefix %d", i)
		}
		if len(c.events) != 0 {
			t.Fatal("truncated message injected input")
		}
	}
}
func TestARDDiscreteNativeWheel(t *testing.T) {
	wire := ardTestWheel(50, 75, 10, -20, 0, 0)
	networkOrder.PutUint32(wire[8+42:], 0)
	p, c, _ := ardScrollPeer(wire)
	if err := readTestARDEvent(t, p); err != nil {
		t.Fatal(err)
	}
	got := <-c.events
	if got.DX != -15 || got.DY != 30 {
		t.Fatalf("wheel distance changed: %+v", got)
	}
}

type ardScreenInput struct {
	testInput
	id    uint32
	event input.Event
}

func (c *ardScreenInput) ScrollOnScreen(id uint32, e input.Event) error {
	c.id, c.event = id, e
	return nil
}
func TestARDScrollRoutesPresentedDisplay(t *testing.T) {
	p, _, _ := ardScrollPeer(ardTestWheel(250, 25, 1, -2, 2, 0))
	c := &ardScreenInput{}
	p.controls = c
	p.state.presented = DesktopLayout{Size: image.Pt(300, 100), Screens: []DesktopScreen{{ID: 7, Bounds: image.Rect(0, 0, 200, 100)}, {ID: 9, Bounds: image.Rect(200, 0, 300, 50)}}}
	if err := readTestARDEvent(t, p); err != nil {
		t.Fatal(err)
	}
	if c.id != 9 || c.event.X != .5 || c.event.Y != .5 || c.event.DX != -.2 || c.event.DY != .4 {
		t.Fatalf("wrong display: %d %+v", c.id, c.event)
	}
}

func FuzzARDScrollEvent(f *testing.F) {
	f.Add(ardTestWheel(50, 75, .25, -24, 1, 0))
	f.Add(ardTestEvent(1, 8, make([]byte, 28)))
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) == 0 || wire[0] != 23 {
			return
		}
		p, c, _ := ardScrollPeer(wire)
		_, _ = io.ReadFull(p.conn, make([]byte, 1))
		_ = p.ardEvent()
		for len(c.events) > 0 {
			if err := (<-c.events).Validate(); err != nil {
				t.Fatal(err)
			}
		}
	})
}
