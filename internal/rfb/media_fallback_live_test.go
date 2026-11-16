package rfb

import (
	"context"
	"image"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

type noInboundMedia struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *noInboundMedia) Read([]byte) (int, error) { <-c.done; return 0, net.ErrClosed }
func (c *noInboundMedia) Close() error {
	c.once.Do(func() { close(c.done); c.Conn.Close() })
	return nil
}

type noInboundMediaDialer struct{}

func (noInboundMediaDialer) DialContext(ctx context.Context, n, a string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, n, a)
}
func (noInboundMediaDialer) DialUDP(l, r netip.AddrPort) (net.Conn, error) {
	l = netip.AddrPortFrom(l.Addr(), 0)
	c, e := net.DialUDP("udp", net.UDPAddrFromAddrPort(l), net.UDPAddrFromAddrPort(r))
	if e != nil {
		return nil, e
	}
	return &noInboundMedia{Conn: c, done: make(chan struct{})}, nil
}

// Against a real Mac, suppress inbound media to exercise the complete
// timeout-to-RFB transition. The Mac must return actual opaque picture pixels.
func TestMediaFallbackWithBlockedUDPLiveServer(t *testing.T) {
	if os.Getenv("MIRAGE_LIVE_FALLBACK_TEST") != "1" {
		t.Skip("explicit live probe")
	}
	home, _ := os.UserHomeDir()
	b, e := os.ReadFile(filepath.Join(home, ".config/mirage/test.env"))
	if e != nil {
		t.Fatal(e)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	var accepted, falling atomic.Bool
	shown := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, e := Dial(ctx, env["MIRAGE_HOST"], Config{Username: env["MIRAGE_USER"], Password: env["MIRAGE_PASS"], Apple: true, MediaStream: true, MediaRemoteDesktop: true, NoAudio: true, MuteAudio: true, Network: NetworkRemote, Dialer: noInboundMediaDialer{}, Encodings: append([]int32{EncAppleRGB555}, DefaultEncodings...),
		OnMediaAnswer: func([]byte) { accepted.Store(true) },
		OnMediaDiagnostics: func(d MediaDiagnostics) {
			if d.Fallback != "" {
				falling.Store(true)
			}
		},
		OnUpdate: func(fb *frame.Framebuffer, _ []image.Rectangle) {
			if !falling.Load() {
				return
			}
			for i := 0; i+3 < len(fb.Pix); i += 4 * 127 {
				if fb.Pix[i]|fb.Pix[i+1]|fb.Pix[i+2] != 0 && fb.Pix[i+3] == 255 {
					select {
					case shown <- struct{}{}:
					default:
					}
					return
				}
			}
		},
	})
	if e != nil {
		t.Fatal("live dial failed")
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	select {
	case <-shown:
		if strings.HasPrefix(c.MediaDiagnostics().Fallback, "refused") {
			t.Skip("server refused media before the timeout")
		}
		if !accepted.Load() {
			t.Fatalf("server never accepted the media offer: %s", c.MediaDiagnostics().Fallback)
		}
		t.Log("Inbound UDP dropped; ordinary RGB555 pixels resumed after timeout")
		t.Logf("fallback=%s accepted=%v", c.MediaDiagnostics().Fallback, accepted.Load())
		for range 3 {
			time.Sleep(500 * time.Millisecond)
			select {
			case <-shown:
			default:
			}
			if err := c.Refresh(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-shown:
			case <-time.After(3 * time.Second):
				t.Fatal("ordinary pictures stopped after fallback")
			case <-done:
				t.Fatal("connection ended after fallback")
			}
		}
	case <-time.After(14 * time.Second):
		t.Errorf("accepted=%v fallback=%v: no valid ordinary picture: %s", accepted.Load(), falling.Load(), c.MediaDiagnostics().Fallback)
	case <-done:
		t.Fatal("connection ended before fallback picture")
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("reader did not stop")
	}
}
