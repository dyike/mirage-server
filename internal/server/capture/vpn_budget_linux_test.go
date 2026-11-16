package capture

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Explicit hardware probe; logs byte counts, never desktop content.
func TestNativeVPNKeyframeBudget(t *testing.T) {
	output := os.Getenv("MIRAGE_TEST_OUTPUT")
	if output == "" {
		t.Skip("requires explicit native output")
	}
	for _, qp := range []int{30, 34, 38} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		frames, peak, total := 0, 0, 0
		stop := errors.New("probe complete")
		err := Run(ctx, Options{Output: output, RepeatIdle: true, MinQP: qp, Codec: "hevc", Width: 4608, Height: 2592, FPS: 60, Bitrate: 3000}, func(f Frame) error {
			frames++
			peak = max(peak, len(f.Data))
			total += len(f.Data)
			if frames == 125 {
				return stop
			}
			return nil
		})
		cancel()
		if !errors.Is(err, stop) {
			t.Fatal(err)
		}
		t.Logf("qp=%d frames=%d peak=%d total=%d", qp, frames, peak, total)
	}
}
