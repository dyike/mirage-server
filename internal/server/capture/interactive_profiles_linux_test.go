package capture

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"
)

// Opt-in measurement on the actual desktop. No input is injected and no
// captured image is written to disk. Cadence is not input-to-display latency.
func TestNativeInteractiveProfiles(t *testing.T) {
	output := os.Getenv("MIRAGE_TEST_OUTPUT")
	if output == "" {
		t.Skip("requires explicit native output")
	}
	for _, codec := range []string{"hevc", "h264"} {
		for _, mode := range []struct {
			name      string
			w, h, fps int
		}{{"4k30", 3840, 2160, 30}, {"1440p60", 2560, 1440, 60}} {
			t.Run(codec+"/"+mode.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				start := time.Now()
				var first, previous time.Time
				var gaps []time.Duration
				frames, total, peak := 0, 0, 0
				stop := errors.New("measured")
				err := Run(ctx, Options{Output: output, RepeatIdle: true, MinQP: 30, Codec: codec, Width: mode.w, Height: mode.h, FPS: mode.fps, Bitrate: 6000}, func(f Frame) error {
					now := time.Now()
					if frames == 0 {
						first = now
					} else {
						gaps = append(gaps, now.Sub(previous))
					}
					previous = now
					frames++
					total += len(f.Data)
					peak = max(peak, len(f.Data))
					if frames >= mode.fps*5 {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
				sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
				t.Logf("frames=%d fps=%.1f startup=%s gap-p50=%s gap-p95=%s gap-max=%s peak-bytes=%d avg-kbps=%.0f", frames, float64(frames-1)/previous.Sub(first).Seconds(), first.Sub(start).Round(time.Millisecond), gaps[len(gaps)/2].Round(time.Microsecond), gaps[len(gaps)*95/100].Round(time.Microsecond), gaps[len(gaps)-1].Round(time.Microsecond), peak, float64(total)*8/previous.Sub(start).Seconds()/1000)
			})
		}
	}
}
