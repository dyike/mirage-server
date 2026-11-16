package capture

import (
	"context"
	"errors"
	"testing"

	"github.com/dyike/mirage-server/internal/server/input"
)

func TestDMAIdleFramesKeepGPUOwnership(t *testing.T) {
	first, second := new(input.DMABuffer), new(input.DMABuffer)
	available := make(chan *input.DMABuffer, 2)
	available <- second
	var releases []func()
	stop := errors.New("received three idle frames")
	err := feedDMAContinuous(context.Background(), Options{FPS: 120}, first, available, func(ctx context.Context, b *input.DMABuffer) (uint32, error) {
		if b == first {
			t.Error("compositor allowed to overwrite a GPU-owned frame")
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}, func(b *input.DMABuffer, release func()) error {
		if b != first {
			t.Error("idle frame did not reuse the captured buffer")
		}
		releases = append(releases, release)
		if len(releases) == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if len(releases) != 3 {
		t.Fatalf("idle capture stopped encoding: frames=%d", len(releases))
	}
	if len(available) != 1 {
		t.Fatalf("cached frame reused before GPU release: free=%d", len(available))
	}
	for _, release := range releases {
		release()
	}
	if len(available) != 2 {
		t.Fatalf("DMA ownership leaked after shutdown: free=%d", len(available))
	}
}
