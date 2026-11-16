package rfb

import (
	"fmt"
	"image"
	"io"
	"math"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestOnLink(t *testing.T) {
	nets := []netip.Prefix{netip.MustParsePrefix("192.168.5.82/24"), netip.MustParsePrefix("fe80::1c/64")}
	for _, c := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"192.168.5.181", true},
		{"192.168.6.181", false}, // the same Mac, reached through a VPN from another network
		{"10.8.0.1", false},
		{"fe80::1%en0", true}, // a Bonjour address
		{"203.0.113.5", false},
	} {
		if got := onLink(netip.MustParseAddr(c.addr), nets); got != c.want {
			t.Errorf("onLink(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// remoteConn reports a chosen peer address.
type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

func pushClient(t *testing.T, remote string, interval time.Duration) (*Client, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() })
	c := &Client{conn: remoteConn{client, net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remote))}, cfg: Config{UpdateInterval: interval}, appleExt: true}
	c.appleCmds[msgAppleAutoUpdate/8] |= 0x80 >> (msgAppleAutoUpdate % 8)
	c.size.Store(uint64(640)<<32 | 480)
	return c, server
}

func TestPushOnlyOnLocalNetwork(t *testing.T) {
	// TEST-NET-3 is on no interface: a server over a VPN or the internet.
	c, server := pushClient(t, "203.0.113.5:5900", 0)
	go c.armAutoUpdate()
	server.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 16)
	if _, err := io.ReadFull(server, b); err != nil || b[0] != msgAppleAutoUpdate || be.Uint32(b[4:]) != math.MaxUint32 {
		t.Fatalf("polling setup did not explicitly disable push: % x, %v", b, err)
	}
	if c.pushing.Load() {
		t.Fatal("pushing to a server off the local network")
	}
	c, server = pushClient(t, "127.0.0.1:5900", 0)
	go c.armAutoUpdate()
	server.SetReadDeadline(time.Now().Add(time.Second))
	b = make([]byte, 16)
	if _, err := io.ReadFull(server, b); err != nil || b[0] != msgAppleAutoUpdate {
		t.Fatalf("no push setup for a local server: % x, %v", b, err)
	}
}

func TestExplicitIntervalAlwaysPushes(t *testing.T) {
	c, server := pushClient(t, "203.0.113.5:5900", 50*time.Millisecond)
	go c.armAutoUpdate()
	server.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 16)
	if _, err := io.ReadFull(server, b); err != nil || b[0] != msgAppleAutoUpdate || !c.pushing.Load() {
		t.Fatalf("explicit interval did not push: % x, %v", b, err)
	}
}

func TestSlowPushedUpdateEndsPush(t *testing.T) {
	c, server := pushClient(t, "127.0.0.1:5900", 0)
	c.pushing.Store(true)
	if err := c.checkPushPace(pushSlowUpdate / 2); err != nil || !c.pushing.Load() {
		t.Fatalf("a quick update ended push: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.checkPushPace(pushSlowUpdate + time.Second) }()
	server.SetReadDeadline(time.Now().Add(time.Second))
	// InFlight-1 incremental requests, without the push setup appended.
	for i := range 3 {
		b := make([]byte, 10)
		if _, err := io.ReadFull(server, b); err != nil || b[0] != 3 || b[1] != 1 {
			t.Fatalf("request %d: % x, %v", i, b, err)
		}
	}
	if err := <-done; err != nil || c.pushing.Load() {
		t.Fatalf("slow update kept pushing: %v", err)
	}
	server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatalf("sent %d extra bytes", n)
	}
}

func TestSlowTunnelRFBUsesEconomyDespiteLocalAddress(t *testing.T) {
	for _, explicitScale := range []float64{0, 1} {
		t.Run(fmt.Sprint(explicitScale), func(t *testing.T) {
			c, server := pushClient(t, "127.0.0.1:5900", 0)
			c.apple = true
			c.cfg.AppleScale = explicitScale
			c.pushing.Store(true)
			done := make(chan error, 1)
			go func() { done <- c.checkPushPace(2 * time.Second) }()
			server.SetReadDeadline(time.Now().Add(time.Second))
			b := make([]byte, 10)
			if explicitScale == 0 {
				if _, err := io.ReadFull(server, b); err != nil || b[0] != 8 || math.Float64frombits(be.Uint64(b[2:])) != 0.5 {
					t.Fatalf("slow tunnel did not reduce RFB size: % x, %v", b, err)
				}
			}
			for range 3 {
				if _, err := io.ReadFull(server, b); err != nil || b[0] != 3 || b[1] != 1 {
					t.Fatalf("no incremental request after stopping push: % x, %v", b, err)
				}
			}
			if err := <-done; err != nil || c.pushing.Load() {
				t.Fatalf("failed to switch from push to RFB economy: %v", err)
			}
		})
	}
}

// The layout of the Mac this was found on: a 5K display beside a shorter
// one, so the framebuffer has bands above and below the second display
// that no display shows and the server never paints.
func twoDisplays(fb image.Point) *DisplayLayout {
	return &DisplayLayout{
		Desktop: image.Pt(4864, 1440), Framebuffer: fb, Current: AllDisplays,
		Displays: []Display{
			{ID: 3, Scale: 1, Backing: image.Rect(0, 0, 5120, 2880), Main: true},
			{ID: 2, Scale: 1, Backing: image.Rect(5120, 144, 9728, 2736)},
		},
	}
}

func TestFramebufferRects(t *testing.T) {
	l := twoDisplays(image.Pt(9728, 2880))
	got := l.framebufferRects()
	if len(got) != 2 || got[0] != image.Rect(0, 0, 5120, 2880) || got[1] != image.Rect(5120, 144, 9728, 2736) {
		t.Fatalf("all displays: %v", got)
	}
	l = twoDisplays(image.Pt(4864, 1440)) // ScaleFactor 0.5
	if got := l.framebufferRects(); got[1] != image.Rect(2560, 72, 4864, 1368) {
		t.Fatalf("half scale: %v", got)
	}
	l = twoDisplays(image.Pt(4608, 2592))
	l.Current = 2 // the second display alone
	if got := l.framebufferRects(); len(got) != 1 || got[0] != image.Rect(0, 0, 4608, 2592) {
		t.Fatalf("one display: %v", got)
	}
}

func TestCoverageIgnoresAreasOutsideDisplays(t *testing.T) {
	for _, fb := range []image.Point{{9728, 2880}, {4864, 1440}, {2432, 720}} {
		l := twoDisplays(fb)
		var cv coverage
		cv.reset(fb.X, fb.Y)
		rects := l.framebufferRects()
		cv.setDisplays(rects)
		cv.mark(rects[0])
		if cv.missing() == nil {
			t.Fatalf("%v: second display counted as painted", fb)
		}
		cv.mark(rects[1])
		if m := cv.missing(); m != nil {
			t.Fatalf("%v: both displays painted, still missing %v", fb, m)
		}
	}
}

func TestCoverageCellOnTwoDisplaysNeedsBoth(t *testing.T) {
	var cv coverage
	cv.reset(64, 32)
	// Displays meet inside the cell at x 16..32.
	cv.setDisplays([]image.Rectangle{image.Rect(0, 0, 24, 32), image.Rect(24, 8, 64, 24)})
	cv.mark(image.Rect(0, 0, 24, 32))
	if m := cv.missing(); len(m) == 0 || !image.Rect(16, 0, 32, 32).Overlaps(m[0]) {
		t.Fatalf("shared cell painted by one display: %v", m)
	}
	cv.mark(image.Rect(24, 8, 64, 24))
	if m := cv.missing(); m != nil {
		t.Fatalf("still missing %v", m)
	}
}

func TestHoleRetryWaitsForAnswer(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := &Client{conn: client}
	defer c.Close()
	h := &c.holes
	h.mu.Lock()
	h.bands, h.armed, h.asked = []image.Rectangle{image.Rect(0, 0, 16, 16)}, true, time.Now()
	h.mu.Unlock()
	c.retryHoles()
	server.SetReadDeadline(time.Now().Add(holeRetryDelay + 200*time.Millisecond))
	n, err := server.Read(make([]byte, 1))
	h.mu.Lock()
	h.bands = nil // ends the rescheduled retries
	h.mu.Unlock()
	if err == nil {
		t.Fatalf("asked again (%d bytes) before the last request was answered", n)
	}
}
