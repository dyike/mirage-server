package server

import (
	"context"
	"image"
	"net"
	"os"
	"testing"
	"time"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/video"
)

func TestARDDynamicResolutionHEVC(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("hardware capture and decode")
	}
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tlsConfig, _ := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := RFBServer{TLS: tlsConfig, ARD: true, DynamicResolution: true, Username: "test", Password: "test-password", Width: 640, Height: 360, PreferHEVC: true, MaxClients: 1,
		SetDesktop: func(context.Context, DesktopLayout) error { return nil },
		DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
			if r.Codec != "hevc" {
				return context.Canceled
			}
			return capture.Run(ctx, capture.Options{Test: true, Codec: r.Codec, Width: r.Desktop.Size.X, Height: r.Desktop.Size.Y, FPS: 30, Bitrate: 2000}, func(f capture.Frame) error { return send(f.Data) })
		}}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	frames := make(chan image.Point, 64)
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, MediaStream: true, Username: "test", Password: "test-password", NoAudio: true, Network: rfb.NetworkLocal, Dialer: ardLoopbackDialer{}, OnVideoFrame: func(f video.Frame) {
		if f.Surface == 0 {
			t.Error("HEVC frame did not use native GPU surface")
		}
		select {
		case frames <- image.Pt(f.W, f.H):
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	wait := func(size image.Point) {
		t.Helper()
		for {
			select {
			case got := <-frames:
				if got == size {
					return
				}
			case <-ctx.Done():
				t.Fatalf("waiting %v: %+v", size, c.MediaDiagnostics())
			}
		}
	}
	wait(image.Pt(640, 360))
	if l := c.Desktop(); l == nil || !l.DynamicResolution {
		t.Fatalf("missing dynamic capability: %+v", l)
	}
	for _, size := range []image.Point{image.Pt(360, 640), image.Pt(960, 540), image.Pt(640, 360)} {
		if err := c.Resize(size.X, size.Y, 100); err != nil {
			t.Fatal(err)
		}
		wait(size)
		d := c.MediaDiagnostics()
		if d.Fallback != "" || d.Video.Codec != "HEVC" {
			t.Fatalf("not hardware SRTP: %+v", d)
		}
		t.Logf("HEVC SRTP decoded %v, no TCP fallback", size)
	}
}
