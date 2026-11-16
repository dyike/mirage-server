package capture

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dyike/mirage-server/internal/server/input"
)

// The current picture keeps an ownership reference while each GStreamer import
// holds its own reference. Wayland may only overwrite a buffer after both the
// cached picture and every GPU consumer release it. No pixels are CPU-mapped.
type dmaPicture struct {
	buffer *input.DMABuffer
	refs   atomic.Int32
	free   chan<- *input.DMABuffer
}

func holdDMAPicture(b *input.DMABuffer, free chan<- *input.DMABuffer) *dmaPicture {
	p := &dmaPicture{buffer: b, free: free}
	p.refs.Store(1)
	return p
}
func (p *dmaPicture) release() {
	if p.refs.Add(-1) == 0 {
		p.free <- p.buffer
	}
}

// Capture waits for compositor damage independently of the encoding clock.
// When nothing changes, submit another GPU reference to the same picture.
func feedDMAContinuous(ctx context.Context, o Options, first *input.DMABuffer, available chan *input.DMABuffer, capture func(context.Context, *input.DMABuffer) (uint32, error), submit func(*input.DMABuffer, func()) error) error {
	ctx, cancel := context.WithCancel(ctx)
	ready := make(chan *dmaPicture)
	done := make(chan struct{})
	failed := make(chan error, 1)
	current := holdDMAPicture(first, available)
	go func() {
		defer close(done)
		for {
			var b *input.DMABuffer
			select {
			case <-ctx.Done():
				return
			case b = <-available:
			}
			transform, err := capture(ctx, b)
			if err != nil || transform != 0 {
				available <- b
				if err == nil {
					err = fmt.Errorf("output transform changed to %d", transform)
				}
				failed <- err
				return
			}
			next := holdDMAPicture(b, available)
			select {
			case ready <- next:
			case <-ctx.Done():
				next.release()
				return
			}
		}
	}()
	defer func() { cancel(); <-done; current.release() }()
	fps := o.FPS
	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()
	send := func() error {
		p := current
		p.refs.Add(1)
		// submit consumes the reference even if it fails before importing it.
		return submit(p.buffer, p.release)
	}
	if err := send(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failed:
			return err
		case next := <-ready:
			current.release()
			current = next
		case <-ticker.C:
			if o.FPSNow != nil {
				if next := max(1, min(o.FPS, o.FPSNow())); next != fps {
					fps = next
					ticker.Reset(time.Second / time.Duration(fps))
				}
			}
			if err := send(); err != nil {
				return err
			}
		}
	}
}
