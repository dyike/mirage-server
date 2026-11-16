package capture

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativePipelineValidation(t *testing.T) {
	o := Options{Output: "MIRAGE", SourceWidth: 3840, SourceHeight: 2160, Codec: "hevc", Width: 1920, Height: 1080, FPS: 60, Bitrate: 12000}
	p, err := Pipeline(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "appsrc name=desktop_source") || strings.Contains(p, "pipewiresrc") || !strings.Contains(p, "vah265enc") {
		t.Fatal(p)
	}
	o.SourceWidth = 0
	if _, err = Pipeline(o); err == nil {
		t.Fatal("invalid source dimensions accepted")
	}
}

func TestNativeOutputCapture(t *testing.T) {
	name := os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT")
	if name == "" {
		t.Skip("set MIRAGE_TEST_NATIVE_OUTPUT to capture a real output")
	}
	for _, codec := range []string{"raw", "h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := errors.New("captured")
			frames := 0
			err := Run(ctx, Options{Output: name, Width: 640, Height: 360, FPS: 30, Bitrate: 2000, Codec: codec}, func(f Frame) error {
				if f.Width != 640 || f.Height != 360 || len(f.Data) == 0 {
					t.Fatal("invalid frame")
				}
				if codec == "raw" && len(f.Data) != 640*360*4 {
					t.Fatalf("raw size: %d", len(f.Data))
				}
				frames++
				if frames == 3 {
					return done
				}
				return nil
			})
			if !errors.Is(err, done) {
				t.Fatalf("frames=%d: %v", frames, err)
			}
		})
	}
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan error, 1)
		go func() {
			finished <- Run(ctx, Options{Output: name, Width: 640, Height: 360, FPS: 30, Bitrate: 2000, Codec: "hevc"}, func(Frame) error { cancel(); return nil })
		}()
		defer cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("capture did not stop after cancellation")
		}
	})
	dmaReleases.Range(func(key, value any) bool {
		t.Error("DMA buffer release callback leaked after capture stop")
		return true
	})
}
