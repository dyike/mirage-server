package capture

import (
	"context"
	"errors"
	"github.com/dyike/mirage-server/internal/server/input"
	"os"
	"testing"
	"time"
)

// Validate channel order and orientation across the GL -> linear GBM -> VA
// boundary. Only the final test-only RGBA sink is read back for assertions.
func TestGPUBridgeColors(t *testing.T) {
	name := os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT")
	if name == "" {
		t.Skip("requires EGL and a compositor output")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	screen, err := input.OpenScreen(ctx, name, false)
	if err != nil {
		t.Fatal(err)
	}
	defer screen.Close()
	pool, err := screen.NewRenderPool(128, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	allocator, err := newDMAAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer api.unref(allocator)
	available := make(chan *input.DMABuffer, 3)
	for _, b := range pool.Buffers {
		available <- b
	}
	before := dmaMapAttempts.Load()
	stop := errors.New("validated")
	frames := 0
	err = runPipelineFeed(ctx, `appsrc name=desktop_source is-live=true format=time do-timestamp=true block=true max-buffers=2 caps=video/x-raw,format=BGRA,width=128,height=64,framerate=30/1 ! vapostproc ! video/x-raw,format=NV12 ! videoconvert ! video/x-raw,format=RGBA ! appsink name=encoded sync=false`, func(pixels []byte) error {
		frames++
		if frames < 3 {
			return nil
		}
		for _, c := range []struct {
			x, y    int
			r, g, b byte
		}{{32, 16, 255, 0, 0}, {96, 16, 0, 255, 0}, {96, 48, 0, 0, 255}} {
			offset := (c.y*128 + c.x) * 4
			if len(pixels) < offset+4 {
				t.Error("short frame")
				return stop
			}
			for i, want := range []byte{c.r, c.g, c.b} {
				delta := int(pixels[offset+i]) - int(want)
				if delta < -32 || delta > 32 {
					t.Errorf("pixel (%d,%d): got %v", c.x, c.y, pixels[offset:offset+4])
					break
				}
			}
		}
		return stop
	}, func(ctx context.Context, source uintptr) error {
		return runPipelineBuffers(ctx, `glvideomixerelement name=m sink_0::xpos=0 sink_1::xpos=64 sink_2::xpos=64 sink_2::ypos=32 ! video/x-raw(memory:GLMemory),format=RGBA,width=128,height=64,framerate=30/1 ! appsink name=encoded sync=false max-buffers=2 drop=true videotestsrc is-live=true pattern=red ! video/x-raw,width=64,height=64,framerate=30/1 ! glupload ! glcolorconvert ! m.sink_0 videotestsrc is-live=true pattern=green ! video/x-raw,width=64,height=32,framerate=30/1 ! glupload ! glcolorconvert ! m.sink_1 videotestsrc is-live=true pattern=blue ! video/x-raw,width=64,height=32,framerate=30/1 ! glupload ! glcolorconvert ! m.sink_2`, func(buffer uintptr) error {
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
				return errors.New("test encoder stopped")
			}
			return nil
		}, nil, nil)
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if dmaMapAttempts.Load() != before {
		t.Fatal("CPU mapped the GPU render target")
	}
}
