package capture

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"time"
	"unsafe"

	"github.com/dyike/mirage-server/internal/server/input"
)

// Video uses DMA capture and GPU composition. Raw keeps shared-memory capture.
func runNativeLayers(ctx context.Context, o Options, consume func(Frame) error) error {
	if len(o.Layers) < 2 || len(o.Layers) > 16 || o.FPS < 1 || o.FPS > 120 {
		return fmt.Errorf("invalid native layers or FPS")
	}
	o.Layers = append([]Layer(nil), o.Layers...)
	var allocator uintptr
	var renderPool *input.DMAPool
	if o.Codec != "raw" {
		var err error
		allocator, err = newDMAAllocator()
		if err != nil {
			return err
		}
		defer api.unref(allocator)
	}
	names := make([]string, len(o.Layers))
	feeds := make([]func(context.Context, uintptr) error, len(o.Layers))
	for i := range o.Layers {
		l := &o.Layers[i]
		screen, err := input.OpenScreen(ctx, l.Output, o.EmbeddedCursor)
		if err != nil {
			return err
		}
		defer screen.Close()
		if o.Codec != "raw" && renderPool == nil {
			renderPool, err = screen.NewRenderPool(o.Width, o.Height, 6)
			if err != nil {
				return err
			}
			defer renderPool.Close()
		}
		if o.Codec != "raw" {
			pool, err := screen.NewDMAPool(6)
			if err != nil {
				return err
			}
			defer pool.Close()
			first := pool.Buffers[0]
			transform, err := pool.Capture(ctx, first)
			if err != nil {
				return err
			}
			if transform != 0 {
				return fmt.Errorf("unsupported output transform %d", transform)
			}
			l.SourceWidth, l.SourceHeight, l.Format = first.Width, first.Height, "BGRx"
			if first.Format == 0x34325241 {
				l.Format = "BGRA"
			}
			names[i] = fmt.Sprintf("desktop_source_%d", i)
			available := make(chan *input.DMABuffer, len(pool.Buffers))
			for _, b := range pool.Buffers[1:] {
				available <- b
			}
			feeds[i] = func(ctx context.Context, source uintptr) error {
				return feedDMAContinuous(ctx, o, first, available, pool.Capture, func(b *input.DMABuffer, release func()) error {
					buffer, err := wrapDMA(allocator, b, release)
					if err != nil {
						release()
						return err
					}
					if flow := api.pushBuffer(source, buffer); flow != 0 {
						return fmt.Errorf("DMA layer flow %d", flow)
					}
					return nil
				})
			}
			continue
		}
		first, w, h, err := screen.Next(ctx)
		if err != nil {
			return err
		}
		l.SourceWidth, l.SourceHeight, l.Format = w, h, "BGRx"
		names[i] = fmt.Sprintf("desktop_source_%d", i)
		feeds[i] = func(ctx context.Context, source uintptr) error {
			return feedRawLayer(ctx, screen, bytes.Clone(first), w, h, o.FPS, o.FPSNow, source)
		}
	}
	if o.Codec == "raw" {
		description, err := Pipeline(o)
		if err != nil {
			return err
		}
		return runPipelineSources(ctx, description, func(b []byte) error { return consume(Frame{Data: b, Width: o.Width, Height: o.Height}) }, names, feeds, encoderControls{rawBytes: rawFrameBytes(o)})
	}
	compose := o
	compose.ComposeOnly = true
	description, err := Pipeline(compose)
	if err != nil {
		return err
	}
	encode := o
	encode.Layers = nil
	encode.Output = "composite"
	encode.SourceWidth = o.Width
	encode.SourceHeight = o.Height
	encode.DMA = true
	encode.DMAFormat = "BGRA"
	encoder, err := Pipeline(encode)
	if err != nil {
		return err
	}
	available := make(chan *input.DMABuffer, len(renderPool.Buffers))
	for _, b := range renderPool.Buffers {
		available <- b
	}
	mapsBefore := dmaMapAttempts.Load()
	defer func() { log.Printf("GPU multi-output capture: CPU-map-attempts=%d", dmaMapAttempts.Load()-mapsBefore) }()
	log.Printf("native multi-output capture: screens=%d DMA capture, GPU composition, DMA-BUF VA encoding; no CPU pixel transfers", len(o.Layers))
	return runPipelineFeed(ctx, encoder, func(b []byte) error { return consume(Frame{Data: b, Width: o.Width, Height: o.Height}) }, func(ctx context.Context, source uintptr) error {
		return runPipelineBuffers(ctx, description, func(buffer uintptr) error {
			var target *input.DMABuffer
			select {
			case target = <-available:
			case <-ctx.Done():
				return ctx.Err()
			}
			release := func() { available <- target }
			if err := blitGLBuffer(buffer, target); err != nil {
				release()
				return err
			}
			wrapped, err := wrapDMA(allocator, target, release)
			if err != nil {
				release()
				return err
			}
			if flow := api.pushBuffer(source, wrapped); flow != 0 {
				return fmt.Errorf("composite encoder flow %d", flow)
			}
			return nil
		}, names, feeds)
	}, encoderControls{o.BindBitrate, o.BindKeyframe, rawFrameBytes(o)})
}

func feedRawLayer(ctx context.Context, screen *input.ScreenCapture, first []byte, w, h, fps int, fpsNow func() int, source uintptr) error {
	ctx, cancel := context.WithCancel(ctx)
	ready := make(chan []byte)
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			pixels, width, height, err := screen.Next(ctx)
			if err == nil && (width != w || height != h) {
				err = fmt.Errorf("output dimensions changed")
			}
			if err != nil {
				failed <- err
				return
			}
			select {
			case ready <- bytes.Clone(pixels):
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(time.Second / time.Duration(fps))
	defer ticker.Stop()
	pixels := first
	var pts time.Duration
	for {
		buffer := api.allocBuffer(0, uintptr(len(pixels)), 0)
		if buffer == 0 {
			return fmt.Errorf("cannot allocate layer frame")
		}
		if api.fillBuffer(buffer, 0, &pixels[0], uintptr(len(pixels))) != uintptr(len(pixels)) {
			api.miniUnref(buffer)
			return fmt.Errorf("cannot copy layer frame")
		}
		duration := time.Second / time.Duration(fps)
		if fpsNow != nil {
			duration = time.Second / time.Duration(max(1, min(fps, fpsNow())))
		}
		timestampRawLayer(buffer, pts, duration)
		pts += duration
		if flow := api.pushBuffer(source, buffer); flow != 0 {
			return fmt.Errorf("raw layer flow %d", flow)
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-failed:
				return err
			case next := <-ready:
				pixels = next
			case <-ticker.C:
				if fpsNow != nil {
					ticker.Reset(time.Second / time.Duration(max(1, min(fps, fpsNow()))))
				}
				break wait
			}
		}
	}
}

// Public GstBuffer ABI. Timing must start at zero for every non-live compositor
// pad, even when the physical outputs deliver their first frames at different times.
type gstBufferTimingABI struct {
	Mini               gstMiniObjectABI
	Pool               uintptr
	PTS, DTS, Duration uint64
}

func timestampRawLayer(buffer uintptr, pts, duration time.Duration) {
	b := (*gstBufferTimingABI)(unsafe.Pointer(buffer))
	b.PTS = uint64(pts)
	b.DTS = ^uint64(0)
	b.Duration = uint64(duration)
}
