package capture

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHardwareKeyframeQuantizerBudget(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("requires VA-API")
	}
	for _, floor := range []string{"", " min-qp=24", " min-qp=30"} {
		p, err := Pipeline(Options{Test: true, Codec: "hevc", Width: 3840, Height: 2160, FPS: 30, Bitrate: 6000})
		if err != nil {
			t.Fatal(err)
		}
		p = strings.Replace(p, "pattern=ball", "pattern=smpte", 1)
		p = strings.Replace(p, "name=video_encoder", "name=video_encoder"+floor, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		frames, peak, total := 0, 0, 0
		stop := errors.New("done")
		err = runPipeline(ctx, p, func(data []byte) error {
			frames++
			peak = max(peak, len(data))
			total += len(data)
			if frames == 65 {
				return stop
			}
			return nil
		})
		cancel()
		if !errors.Is(err, stop) {
			t.Fatal(err)
		}
		t.Logf("%q: peak=%d total=%d", floor, peak, total)
	}
}

func TestNativeOutputKeyframeBudget(t *testing.T) {
	output := os.Getenv("MIRAGE_TEST_OUTPUT")
	if output == "" {
		t.Skip("requires explicit native output")
	}
	for _, qp := range []int{0, 24, 30} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		frames, peak, total := 0, 0, 0
		stop := errors.New("done")
		err := Run(ctx, Options{Output: output, RepeatIdle: true, MinQP: qp, Codec: "hevc", Width: 3840, Height: 2160, FPS: 30, Bitrate: 6000}, func(f Frame) error {
			frames++
			peak = max(peak, len(f.Data))
			total += len(f.Data)
			if frames == 65 {
				return stop
			}
			return nil
		})
		cancel()
		if !errors.Is(err, stop) {
			t.Fatal(err)
		}
		t.Logf("minQP=%d peak=%d total=%d", qp, peak, total)
	}
}
