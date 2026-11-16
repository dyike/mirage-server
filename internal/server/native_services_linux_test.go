package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"net"
	"os"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/server/input"
)

// Opt-in: output audio and cursor remain in memory on loopback; no remote
// input or clipboard writes are generated against the user's desktop.
func TestNativeServicesWithExistingClient(t *testing.T) {
	output := os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT")
	if output == "" {
		t.Skip("opt-in native Wayland/PipeWire interoperability")
	}
	cfg, _ := testTLS(t)
	sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	failures := make(chan error, 8)
	s := RFBServer{TLS: cfg, Password: "test", Width: 1920, Height: 1080, OpenClipboard: func() (Clipboard, error) { return input.OpenClipboard("") }, OpenCursor: func() (Cursor, error) { return input.OpenCursors([]string{output}, "") }, Audio: func(ctx context.Context, f AudioFormat, send func([]byte) error) error {
		return capture.Audio(ctx, f.Format, f.Channels, f.Rate, false, send)
	}, Source: func(ctx context.Context, _ string, send func([]byte) error) error {
		pixels := make([]byte, 1920*1080*4)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				if err := send(pixels); err != nil {
					return err
				}
			}
		}
	}, OnError: func(e error) {
		select {
		case failures <- e:
		default:
		}
	}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() { cancel(); <-done }()
	cursors := make(chan image.Point, 8)
	audio := make(chan int, 8)
	c, e := rfb.Dial(ctx, l.Addr().String(), rfb.Config{Password: "test", ServerFingerprint: hex.EncodeToString(sum[:]), OnCursor: func(i *image.RGBA, _ image.Point) {
		if i != nil && !i.Bounds().Empty() {
			select {
			case cursors <- i.Bounds().Size():
			default:
			}
		}
	}, OnAudio: func(_, _ int, b []byte) {
		select {
		case audio <- len(b):
		default:
		}
	}})
	if e != nil {
		t.Fatal(e)
	}
	run := make(chan error, 1)
	go func() { run <- c.Run() }()
	defer func() { c.Close(); <-run }()
	select {
	case size := <-cursors:
		t.Logf("existing client decoded native cursor %v", size)
	case e := <-failures:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal("native cursor timed out")
	}
	for i := 0; i < 3; i++ {
		select {
		case bytes := <-audio:
			if bytes == 0 || bytes%4 != 0 {
				t.Fatal("invalid PCM")
			}
		case e := <-failures:
			t.Fatal(e)
		case <-ctx.Done():
			t.Fatal("native audio timed out")
		}
	}
	t.Log("existing client received three native output-audio packets")
}
