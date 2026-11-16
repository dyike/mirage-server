package server

import (
	"image"
	"math"
	"testing"
)

func TestARDLayoutRetinaAndSelection(t *testing.T) {
	screens := []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 2560, 1440)}, {ID: 2, Bounds: image.Rect(2560, 0, 5120, 1440)}}
	for _, tc := range []struct {
		id     uint32
		size   image.Point
		points image.Point
		scale  float64
	}{{1, image.Pt(4608, 2592), image.Pt(2560, 1440), 1.8}, {2, image.Pt(3840, 2160), image.Pt(2560, 1440), 1.5}, {0xffffffff, image.Pt(4608, 1296), image.Pt(5120, 1440), .9}} {
		selected := []DesktopScreen{{ID: tc.id}}
		if tc.id == 0xffffffff {
			selected = screens
		}
		b := ardLayoutBody(DesktopLayout{Size: tc.size, Screens: selected}, screens)
		if networkOrder.Uint16(b) != 5 || int(networkOrder.Uint16(b[2:])) != tc.points.X || int(networkOrder.Uint16(b[4:])) != tc.points.Y || int(networkOrder.Uint16(b[6:])) != tc.size.X || int(networkOrder.Uint16(b[8:])) != tc.size.Y || networkOrder.Uint32(b[10:]) != tc.id {
			t.Fatalf("wrong header: %x", b[:20])
		}
		for i := 0; i < 2; i++ {
			r := b[20+i*56:]
			if math.Abs(math.Float64frombits(networkOrder.Uint64(r))-tc.scale) > 1e-9 || math.Float64frombits(networkOrder.Uint64(r[8:])) != 1 {
				t.Fatal("invalid backing/viewer scale")
			}
			width := int(networkOrder.Uint16(r[34:])) - int(networkOrder.Uint16(r[30:]))
			height := int(networkOrder.Uint16(r[32:])) - int(networkOrder.Uint16(r[28:]))
			expected := tc.size.X
			if tc.id == 0xffffffff {
				expected /= 2
			}
			if width != expected || height != tc.size.Y {
				t.Fatalf("backing rectangle %d: %dx%d", i, width, height)
			}
			if r[40] != 32 || r[43] != 1 {
				t.Fatal("missing pixel format")
			}
		}
	}
}

func TestARDNativeHighPerformanceCapabilities(t *testing.T) {
	b := ardName([]byte("Mirage"))
	if networkOrder.Uint32(b[2:])>>5 != 1 {
		t.Fatal("no configurable canvas advertised")
	}
	for _, cmd := range []byte{18, 28, 29} {
		if b[6+cmd/8]&(0x80>>(cmd%8)) == 0 {
			t.Fatalf("missing command %d", cmd)
		}
	}
}

func TestARDDisplayRequestBounds(t *testing.T) {
	b := make([]byte, 8+156+28)
	networkOrder.PutUint16(b, 1)
	networkOrder.PutUint16(b[2:], 1)
	networkOrder.PutUint16(b[8:], 184)
	networkOrder.PutUint16(b[8+154:], 1)
	networkOrder.PutUint32(b[8+156:], 3840)
	networkOrder.PutUint32(b[8+160:], 2160)
	got, err := ardRequestedCanvas(b)
	if err != nil || got != image.Pt(3840, 2160) {
		t.Fatalf("%v %v", got, err)
	}
	for n := 0; n < len(b); n++ {
		if _, err := ardRequestedCanvas(b[:n]); err == nil {
			t.Fatalf("accepted truncated length %d", n)
		}
	}
	networkOrder.PutUint16(b[8+148:], 1)
	if _, err := ardRequestedCanvas(b); err == nil {
		t.Fatal("accepted out-of-range preferred mode")
	}
	networkOrder.PutUint16(b[8+148:], 0)
	networkOrder.PutUint32(b[8+156:], 0xffffffff)
	if _, err := ardRequestedCanvas(b); err == nil {
		t.Fatal("accepted oversized canvas")
	}
}

func TestARDStandardLayoutUsesFramebufferCoordinates(t *testing.T) {
	screens := []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 2560, 1440)}, {ID: 2, Bounds: image.Rect(2560, 0, 5120, 1440)}}
	for _, id := range []uint32{1, 2, 0xffffffff} {
		selected := []DesktopScreen{{ID: id}}
		size := image.Pt(4608, 2592)
		if id == 0xffffffff {
			selected = screens
			size.Y = 1296
		}
		b := ardPixelLayoutBody(DesktopLayout{Size: size, Screens: selected}, screens)
		if int(networkOrder.Uint16(b[2:])) != size.X || int(networkOrder.Uint16(b[4:])) != size.Y {
			t.Fatal("points differ from transmitted pixels")
		}
		for i := 0; i < 2; i++ {
			r := b[20+i*56:]
			if math.Float64frombits(networkOrder.Uint64(r)) != 1 {
				t.Fatal("standard framebuffer must not receive a second HiDPI transform")
			}
			for j := 0; j < 8; j++ {
				if r[20+j] != r[28+j] {
					t.Fatal("logical and backing rectangles differ")
				}
			}
		}
	}
}

func TestARDScaleUsesOriginalCanvasAndReportsTransform(t *testing.T) {
	base := DesktopLayout{Size: image.Pt(4608, 2592), Screens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 4608, 2592)}}}
	for _, scale := range []float64{0.6171875, 0.6057098765432098, 1} {
		d := scaledARDDesktop(base, scale)
		if d.Size.X%2 != 0 || d.Size.Y%2 != 0 || d.Screens[0].Bounds.Max != d.Size {
			t.Fatalf("invalid scaled layout: %+v", d)
		}
		actual := float64(d.Size.X) / float64(base.Size.X)
		b := ardScaledLayoutBody(base, d, nil, actual)
		if int(networkOrder.Uint16(b[2:])) != base.Size.X || int(networkOrder.Uint16(b[6:])) != d.Size.X || math.Float64frombits(networkOrder.Uint64(b[28:])) != actual {
			t.Fatal("scale metadata does not match capture")
		}
		if scale == 1 && d.Size != base.Size {
			t.Fatal("100% must restore original size")
		}
	}
}

func TestARDDisplaySwitchPreservesWindowScale(t *testing.T) {
	screens := []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 2560, 1440)}, {ID: 2, Bounds: image.Rect(2560, 0, 5120, 1440)}}
	base := DesktopLayout{Size: image.Pt(4608, 2592), Screens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 4608, 2592)}}}
	scale := 2914.0 / 4608
	p := &rfbPeer{server: &RFBServer{Width: 4608, Height: 2592, AvailableScreens: screens}, state: &rfbState{desktop: scaledARDDesktop(base, scale), ardScaleBase: base, ardScale: scale}, requests: make(chan struct{}, 1)}
	size := p.state.desktop.Size
	for _, id := range []uint32{2, 1, 2} {
		if err := p.selectNativeDisplay(id); err != nil {
			t.Fatal(err)
		}
		if p.state.desktop.Size != size || p.state.ardScale != scale || p.state.desktop.Screens[0].ID != id || p.state.ardScaleBase.Screens[0].ID != id {
			t.Fatalf("switch lost scale or input target: %+v", p.state)
		}
		rev := p.state.revision
		if err := p.selectNativeDisplay(id); err != nil {
			t.Fatal(err)
		}
		if p.state.revision != rev {
			t.Fatal("duplicate selection restarts capture")
		}
	}
	if err := p.selectNativeDisplay(0xffffffff); err != nil {
		t.Fatal(err)
	}
	if len(p.state.desktop.Screens) != 2 || p.state.desktop.Size.X != size.X || p.state.desktop.Size.Y >= size.Y {
		t.Fatal("All Displays lost scale or aspect ratio")
	}
	if err := p.selectNativeDisplay(2); err != nil {
		t.Fatal(err)
	}
	if p.state.desktop.Size != size {
		t.Fatal("return from All Displays lost window scale")
	}
}

func TestARDNativeCombinedDisplayFlag(t *testing.T) {
	for _, tc := range []struct {
		combine  byte
		id, want uint32
	}{{1, 0, 0xffffffff}, {1, 2, 0xffffffff}, {0, 0xffffffff, 0xffffffff}, {0, 1, 1}, {0, 2, 2}, {0, 0, 0}} {
		body := make([]byte, 7)
		body[0] = tc.combine
		networkOrder.PutUint32(body[3:], tc.id)
		if got := ardSelectedDisplay(body); got != tc.want {
			t.Fatalf("combine=%d id=%d got=%d", tc.combine, tc.id, got)
		}
	}
}

func TestARDAllDisplaysKeepsPhysicalCatalogStable(t *testing.T) {
	available := []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 2560, 1440)}, {ID: 2, Bounds: image.Rect(2560, 0, 5120, 1440)}}
	reference := image.Pt(4608, 2592)
	single := ardStableDesktop(DesktopLayout{Screens: []DesktopScreen{{ID: 1}}}, available, reference)
	all := ardStableDesktop(DesktopLayout{Screens: available}, available, reference)
	if single.Size != reference || all.Size != image.Pt(9216, 2592) {
		t.Fatalf("invalid fixed catalog: single=%v all=%v", single.Size, all.Size)
	}
	oneWire := ardScaledLayoutBody(single, scaledARDDesktop(single, .632), available, .632)
	allWire := ardScaledLayoutBody(all, scaledARDDesktop(all, .316), available, .316)
	for i := 0; i < 2; i++ {
		for j := 20; j < 36; j++ {
			at := 20 + i*56 + j
			if oneWire[at] != allWire[at] {
				t.Fatalf("screen %d geometry changed when selecting All Displays", i+1)
			}
		}
	}
	if networkOrder.Uint32(allWire[10:]) != 0xffffffff {
		t.Fatal("combined selection lost")
	}
	if all.Screens[0].Bounds.Max.X != all.Screens[1].Bounds.Min.X {
		t.Fatal("combined pointer mapping has gap or overlap")
	}
}
