package server

import (
	"bytes"
	"context"
	"github.com/dyike/mirage-server/internal/rfb"
	"image"
	"net"
	"testing"
	"time"
)

func TestNativeDisplaySelectionIsPerConnection(t *testing.T) {
	s := &RFBServer{Width: 1920, Height: 1080, DisplayNames: map[uint32]string{1: "DP-2", 2: "HDMI-A-1"},
		AvailableScreens: []DesktopScreen{{ID: 1}, {ID: 2}}}
	newPeer := func() *rfbPeer {
		return &rfbPeer{server: s, state: &rfbState{desktop: s.desktop(), presented: s.desktop()}, requests: make(chan struct{}, 1)}
	}
	a, b := newPeer(), newPeer()
	if err := a.selectNativeDisplay(2); err != nil {
		t.Fatal(err)
	}
	if a.state.desktop.Screens[0].ID != 2 || a.state.desktop.Screens[0].Bounds != image.Rect(0, 0, 1920, 1080) {
		t.Fatalf("wrong selection: %+v", a.state.desktop)
	}
	if a.state.revision != 1 || !a.state.layoutPending || len(a.requests) != 1 {
		t.Fatal("capture was not restarted")
	}
	if b.state.desktop.Screens[0].ID != 1 || a.state.presented.Screens[0].ID != 1 {
		t.Fatal("selection changed another connection or input before presentation")
	}
	if err := a.selectNativeDisplay(99); err == nil {
		t.Fatal("accepted unknown output")
	}
	if a.state.desktop.Screens[0].ID != 2 || a.state.revision != 1 {
		t.Fatal("invalid selection mutated capture")
	}
}

func TestARDClientSelectsNativeDisplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, _ := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan uint32, 32)
	layouts := make(chan rfb.DisplayLayout, 16)
	srv := RFBServer{OnError: func(err error) { t.Logf("server: %v", err) }, TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 64, Height: 64,
		DisplayNames:     map[uint32]string{1: "DP-2", 2: "HDMI-A-1"},
		AvailableScreens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 64, 64)}, {ID: 2, Bounds: image.Rect(64, 0, 128, 64)}},
		DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
			select {
			case captured <- func() uint32 {
				if len(r.Desktop.Screens) > 1 {
					return rfb.AllDisplays
				}
				return r.Desktop.Screens[0].ID
			}():
			case <-ctx.Done():
				return ctx.Err()
			}
			for {
				if err := send(bytes.Repeat([]byte{17, 93, 201, 255}, 64*64)); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, Username: "test", Password: "test-password", OnDisplays: func(l rfb.DisplayLayout) { layouts <- l }, Encodings: []int32{rfb.EncRaw}, NoAudio: true, Network: rfb.NetworkLocal})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := c.Run()
		if err != nil {
			t.Logf("client run: %v", err)
		}
		done <- err
	}()
	defer func() { c.Close(); <-done }()
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal("no initial capture")
	}
	select {
	case l := <-layouts:
		if len(l.Displays) != 2 || l.Displays[0].Name != "DP-2" || l.Displays[1].Name != "HDMI-A-1" || !l.Displays[0].Main || l.SingleDisplayOnly {
			t.Fatalf("wrong metadata: %+v", l)
		}
	case <-ctx.Done():
		t.Fatal("no layout")
	}
	for _, want := range []uint32{2, rfb.AllDisplays, 1} {
		wireID := want
		if want == rfb.AllDisplays {
			// Native macOS uses combine=1, id=0, rather than the sentinel.
			wireID = 0
		}
		if err := c.SelectDisplay(wireID); err != nil {
			t.Fatal(err)
		}
	wait:
		for {
			select {
			case id := <-captured:
				if id == want {
					break wait
				}
			case <-ctx.Done():
				t.Fatal("selection did not reach capture")
			}
		}
	}
}

func TestNativeAllDisplayGeometry(t *testing.T) {
	s := &RFBServer{Width: 4608, Height: 2592, AvailableScreens: []DesktopScreen{{ID: 1, Bounds: image.Rect(-2400, 0, 0, 1350)}, {ID: 2, Bounds: image.Rect(0, 0, 3072, 1728)}}}
	p := &rfbPeer{server: s, state: &rfbState{desktop: s.desktop(), presented: s.desktop()}, requests: make(chan struct{}, 1)}
	if err := p.selectNativeDisplay(rfb.AllDisplays); err != nil {
		t.Fatal(err)
	}
	d := p.state.desktop
	if len(d.Screens) != 2 || d.Size != image.Pt(4608, 1454) || d.Screens[0].Bounds.Min != image.Pt(0, 0) || d.Screens[0].Bounds.Max.X != d.Screens[1].Bounds.Min.X || d.Screens[1].Bounds.Max != d.Size {
		t.Fatalf("bad combined geometry: %+v", d)
	}
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	if err := p.selectNativeDisplay(2); err != nil {
		t.Fatal(err)
	}
	if p.state.desktop.Size != image.Pt(4608, 2592) || len(p.state.desktop.Screens) != 1 {
		t.Fatal("single output size not restored")
	}
}

type selectedDisplayInput struct {
	*testInput
	id   uint32
	x, y float64
}

func (c *selectedDisplayInput) PointerOnScreen(id uint32, x, y float64, _ uint16) error {
	c.id, c.x, c.y = id, x, y
	return nil
}
func TestAllDisplaysPointerRouting(t *testing.T) {
	controls := &selectedDisplayInput{}
	p := &rfbPeer{controls: controls, state: &rfbState{presented: DesktopLayout{Size: image.Pt(400, 200), Screens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 200, 150)}, {ID: 2, Bounds: image.Rect(200, 0, 400, 200)}}}}}
	if err := p.pointer(0, 300, 100); err != nil {
		t.Fatal(err)
	}
	if controls.id != 2 || controls.x != .5 || controls.y != .5 {
		t.Fatalf("wrong secondary mapping: %+v", controls)
	}
	if err := p.pointer(0, 100, 75); err != nil {
		t.Fatal(err)
	}
	if controls.id != 1 || controls.x != .5 || controls.y != .5 {
		t.Fatalf("wrong primary mapping: %+v", controls)
	}
	if err := p.pointer(0, 100, 175); err != nil {
		t.Fatal(err)
	}
	if controls.id != 1 {
		t.Fatal("desktop gap generated input")
	}
}

func TestDuplicateNativeDisplaySelectionDoesNotRestartCapture(t *testing.T) {
	s := &RFBServer{Width: 1920, Height: 1080, AvailableScreens: []DesktopScreen{{ID: 1}, {ID: 2}}}
	p := &rfbPeer{server: s, state: &rfbState{desktop: s.desktop()}, requests: make(chan struct{}, 1)}
	if err := p.selectNativeDisplay(2); err != nil {
		t.Fatal(err)
	}
	revision := p.state.revision
	<-p.requests
	if err := p.selectNativeDisplay(2); err != nil {
		t.Fatal(err)
	}
	if p.state.revision != revision || len(p.requests) != 0 {
		t.Fatal("duplicate selection restarted capture")
	}
}

func TestNativeDesktopNotificationUsesNegotiatedEncoding(t *testing.T) {
	d := DesktopLayout{Size: image.Pt(3840, 2160), Screens: []DesktopScreen{{ID: 1}}}
	r := desktopNotification(d, 0xffffff21)
	if len(r) != 12 || networkOrder.Uint32(r[8:]) != 0xffffff21 || networkOrder.Uint16(r[4:]) != 3840 {
		t.Fatalf("invalid basic desktop size: %x", r)
	}
	r = desktopNotification(d, 0xfffffecc)
	if len(r) <= 12 || networkOrder.Uint32(r[8:]) != 0xfffffecc {
		t.Fatal("missing extended layout")
	}
	if desktopNotification(d, 0) != nil {
		t.Fatal("sent unadvertised encoding")
	}
}

func TestARDClientSelectsNativeDisplayWhenIdle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, _ := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan uint32, 32)
	layouts := make(chan rfb.DisplayLayout, 16)
	srv := RFBServer{OnError: func(err error) { t.Logf("server: %v", err) }, TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 64, Height: 64,
		DisplayNames:     map[uint32]string{1: "DP-2", 2: "HDMI-A-1"},
		AvailableScreens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 64, 64)}, {ID: 2, Bounds: image.Rect(64, 0, 128, 64)}},
		DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
			select {
			case captured <- func() uint32 {
				if len(r.Desktop.Screens) > 1 {
					return rfb.AllDisplays
				}
				return r.Desktop.Screens[0].ID
			}():
			case <-ctx.Done():
				return ctx.Err()
			}
			for {
				if err := send(bytes.Repeat([]byte{17, 93, 201, 255}, 64*64)); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					return ctx.Err()

				}
			}
		}}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, Username: "test", Password: "test-password", OnDisplays: func(l rfb.DisplayLayout) { layouts <- l }, Encodings: []int32{rfb.EncRaw}, NoAudio: true, Network: rfb.NetworkLocal})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := c.Run()
		if err != nil {
			t.Logf("client run: %v", err)
		}
		done <- err
	}()
	defer func() { c.Close(); <-done }()
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal("no initial capture")
	}
	select {
	case l := <-layouts:
		if len(l.Displays) != 2 || l.Displays[0].Name != "DP-2" || l.Displays[1].Name != "HDMI-A-1" || !l.Displays[0].Main || l.SingleDisplayOnly {
			t.Fatalf("wrong metadata: %+v", l)
		}
	case <-ctx.Done():
		t.Fatal("no layout")
	}
	for _, want := range []uint32{2, rfb.AllDisplays, 1} {
		started := time.Now()
		deadline := time.NewTimer(time.Second)
		defer deadline.Stop()
		wireID := want
		if want == rfb.AllDisplays {
			// Native macOS uses combine=1, id=0, rather than the sentinel.
			wireID = 0
		}
		if err := c.SelectDisplay(wireID); err != nil {
			t.Fatal(err)
		}
	wait:
		for {
			select {
			case id := <-captured:
				if id == want {
					t.Logf("idle capture switched to %d in %s", want, time.Since(started))
					break wait
				}
			case <-deadline.C:
				t.Fatal("idle capture did not cancel within one second")
			case <-ctx.Done():
				t.Fatal("selection did not reach capture")
			}
		}
	}
}
