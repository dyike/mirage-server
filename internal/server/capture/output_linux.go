package capture

import (
	"context"
	"fmt"
	"time"

	"github.com/dyike/mirage-server/internal/server/input"
)

func runOutput(ctx context.Context, o Options, consume func(Frame) error) error {
	if o.FPS < 1 || o.FPS > 120 {
		return fmt.Errorf("invalid FPS")
	}
	if o.Codec != "raw" {
		return runDMAOutput(ctx, o, consume)
	}
	screen, err := input.OpenScreen(ctx, o.Output, o.EmbeddedCursor)
	if err != nil {
		return err
	}
	defer screen.Close()
	pixels, width, height, err := screen.Next(ctx)
	if err != nil {
		return err
	}
	o.SourceWidth, o.SourceHeight = width, height
	description, err := Pipeline(o)
	if err != nil {
		return err
	}
	return runPipelineFeed(ctx, description, func(data []byte) error { return consume(Frame{Data: data, Width: o.Width, Height: o.Height}) }, func(ctx context.Context, source uintptr) error {
		currentFPS := o.FPS
		ticker := time.NewTicker(time.Second / time.Duration(currentFPS))
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			buffer := api.allocBuffer(0, uintptr(len(pixels)), 0)
			if buffer == 0 {
				return fmt.Errorf("cannot allocate native frame")
			}
			if api.fillBuffer(buffer, 0, &pixels[0], uintptr(len(pixels))) != uintptr(len(pixels)) {
				api.miniUnref(buffer)
				return fmt.Errorf("cannot copy native frame")
			}
			if flow := api.pushBuffer(source, buffer); flow != 0 {
				return fmt.Errorf("native capture appsrc flow %d", flow)
			}
			if o.FPSNow != nil {
				if fps := max(1, min(o.FPS, o.FPSNow())); fps != currentFPS {
					currentFPS = fps
					ticker.Reset(time.Second / time.Duration(fps))
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			pixels, width, height, err = screen.Next(ctx)
			if err != nil {
				return err
			}
			if width != o.SourceWidth || height != o.SourceHeight {
				return fmt.Errorf("output dimensions changed; reconnect")
			}
		}
	}, encoderControls{o.BindBitrate, o.BindKeyframe, rawFrameBytes(o)})
}
