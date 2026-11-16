package server

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/video"
)

type ardCongestionDialer struct {
	ardLoopbackDialer
	opens atomic.Int32
}

func (d *ardCongestionDialer) DialUDP(local, remote netip.AddrPort) (net.Conn, error) {
	c, err := d.ardLoopbackDialer.DialUDP(local, remote)
	if err != nil {
		return nil, err
	}
	if d.opens.Add(1) == 1 {
		return &ardCongestedConn{Conn: c, start: time.Now()}, nil
	}
	return c, nil
}

type ardCongestedConn struct {
	net.Conn
	start   time.Time
	packets int
}

func (c *ardCongestedConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil {
			return n, err
		}
		if n > 2 && (b[1] < 192 || b[1] > 223) {
			c.packets++
			if time.Since(c.start) > 2*time.Second && c.packets%3 == 0 {
				continue
			}
		}
		return n, nil
	}
}
func TestARDClientRenegotiatesLowerRate(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("hardware SRTP renegotiation")
	}
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	cfg, _ := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	srv := RFBServer{TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 3840, Height: 2160, PreferHEVC: true, DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
		starts.Add(1)
		return capture.Run(ctx, capture.Options{Test: true, Codec: r.Codec, Width: r.Desktop.Size.X, Height: r.Desktop.Size.Y, FPS: 30, Bitrate: r.Bitrate, BindBitrate: r.BindBitrate, BindKeyframe: r.BindKeyframe}, func(f capture.Frame) error { return send(f.Data) })
	}}
	audioEnabled := os.Getenv("MIRAGE_TEST_ARD_AUDIO") == "1"
	var audioStarts atomic.Int32
	initialOpens, expectedOpens := int32(1), int32(2)
	if audioEnabled {
		initialOpens, expectedOpens = 2, 4
		srv.ARDAudio = func(ctx context.Context, send func([]byte) error) error {
			audioStarts.Add(1)
			return capture.AudioELD(ctx, true, send)
		}
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	d := new(ardCongestionDialer)
	frames := make(chan struct{}, 128)
	var wrongSize atomic.Bool
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, MediaStream: true, Username: "test", Password: "test-password", NoAudio: !audioEnabled, MuteAudio: true, Network: rfb.NetworkLocal, Dialer: d, OnVideoFrame: func(f video.Frame) {
		if f.W != 3840 || f.H != 2160 {
			wrongSize.Store(true)
		}
		if d.opens.Load() > initialOpens {
			select {
			case frames <- struct{}{}:
			default:
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	for i := 0; i < 15; i++ {
		select {
		case <-frames:
		case <-ctx.Done():
			t.Fatalf("client failed to renegotiate: UDP opens=%d diagnostics=%+v", d.opens.Load(), c.MediaDiagnostics())
		}
	}
	if audioEnabled && audioStarts.Load() != 2 {
		t.Fatalf("audio not restarted with fresh keys: %d", audioStarts.Load())
	}
	diag := c.MediaDiagnostics()
	if wrongSize.Load() {
		t.Fatal("bitrate renegotiation changed video resolution")
	}
	if diag.Fallback != "" {
		t.Fatalf("renegotiation was refused: %+v", diag)
	}
	if d.opens.Load() != expectedOpens {
		t.Fatalf("unexpected UDP sessions: %d", d.opens.Load())
	}
	t.Logf("unchanged client lowered cap to %d bit/s, rotated SRTP keys on the same port and decoded 15 frames; capture sessions=%d", diag.MaxBitrate, starts.Load())
}
