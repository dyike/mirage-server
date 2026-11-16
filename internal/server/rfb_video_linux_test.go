package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/video"
)

func TestRFBHardwareInterop(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("opt-in VA-API and native client decoder")
	}
	t.Run("raw-capture", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := errors.New("captured")
		err := capture.Run(ctx, capture.Options{Test: true, Codec: "raw", Width: 640, Height: 360, FPS: 30, Bitrate: 2000}, func(f capture.Frame) error {
			if len(f.Data) != 640*360*4 {
				t.Errorf("unexpected RGBA length %d", len(f.Data))
			}
			return done
		})
		if !errors.Is(err, done) {
			t.Fatal(err)
		}
	})
	w, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release()
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cfg, _ := testTLS(t)
			sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
			pin := hex.EncodeToString(sum[:])
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			errs := make(chan error, 8)
			s := RFBServer{TLS: cfg, Password: "vnc-test", Width: 640, Height: 360, PreferHEVC: true, OnError: func(e error) {
				select {
				case errs <- e:
				default:
				}
			}, Source: func(ctx context.Context, selected string, send func([]byte) error) error {
				return capture.Run(ctx, capture.Options{Test: os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT") == "", Output: os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT"), Codec: selected, Width: 640, Height: 360, FPS: 30, Bitrate: 2000}, func(f capture.Frame) error { return send(f.Data) })
			}}
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, l) }()
			defer func() { cancel(); <-done }()
			enc := rfb.EncH264
			if codec == "hevc" {
				enc = rfb.EncMirageHEVC
			}
			frames := make(chan struct{}, 32)
			c, e := rfb.Dial(ctx, l.Addr().String(), rfb.Config{Password: "vnc-test", ServerFingerprint: pin, Encodings: []int32{enc, rfb.EncH264, rfb.EncRaw}, OnVideoFrame: func(f video.Frame) {
				if f.Surface != 0 {
					select {
					case frames <- struct{}{}:
					default:
					}
				}
			}})
			if e != nil {
				t.Fatal(e)
			}
			run := make(chan error, 1)
			go func() { run <- c.Run() }()
			defer func() { c.Close(); <-run }()
			for i := 0; i < 10; i++ {
				select {
				case <-frames:
				case e := <-errs:
					t.Fatal(e)
				case <-ctx.Done():
					t.Fatalf("video timeout: %+v", c.VideoStatus())
				}
			}
			status := c.VideoStatus()
			if !status.Hardware || !status.ZeroCopy {
				t.Fatalf("not native hardware: %+v", status)
			}
			t.Logf("unchanged RFB client received 10 %s hardware frames: %+v", codec, status)
		})
	}
}
