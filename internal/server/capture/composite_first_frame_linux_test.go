package capture

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestNativeRawCompositeFirstFrameHasBothScreens(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("requires GStreamer")
	}
	o := Options{Codec: "raw", Width: 128, Height: 64, FPS: 30, Bitrate: 2000, Layers: []Layer{{Output: "left", SourceWidth: 64, SourceHeight: 64, Format: "BGRx", Width: 64, Height: 64}, {Output: "right", SourceWidth: 64, SourceHeight: 64, Format: "BGRx", X: 64, Width: 64, Height: 64}}}
	description, err := Pipeline(o)
	if err != nil {
		t.Fatal(err)
	}
	feeds := make([]func(context.Context, uintptr) error, 2)
	for i, color := range [][]byte{{0, 0, 255, 255}, {0, 255, 0, 255}} {
		pixels := bytes.Repeat(color, 64*64)
		feeds[i] = func(ctx context.Context, source uintptr) error {
			timer := time.NewTicker(time.Second / 30)
			defer timer.Stop()
			// Appsrc buffers arrive after PLAYING, like actual screen capture.
			select {
			case <-time.After(time.Duration(60+i*100) * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			var pts time.Duration
			for {
				b := api.allocBuffer(0, uintptr(len(pixels)), 0)
				api.fillBuffer(b, 0, &pixels[0], uintptr(len(pixels)))
				timestampRawLayer(b, pts, time.Second/30)
				pts += time.Second / 30
				if api.pushBuffer(source, b) != 0 {
					return ctx.Err()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := errors.New("first frame checked")
	err = runPipelineSources(ctx, description, func(b []byte) error {
		if len(b) != 128*64*4 {
			t.Fatalf("frame bytes %d", len(b))
		}
		for i, want := range [][]byte{{255, 0, 0}, {0, 255, 0}} {
			at := (32*128 + 32 + i*64) * 4
			if !bytes.Equal(b[at:at+3], want) {
				t.Errorf("screen %d first pixel %v, want %v", i, b[at:at+3], want)
			}
		}
		return done
	}, []string{"desktop_source_0", "desktop_source_1"}, feeds, encoderControls{rawBytes: 128 * 64 * 4})
	if !errors.Is(err, done) {
		t.Fatal(err)
	}
}
