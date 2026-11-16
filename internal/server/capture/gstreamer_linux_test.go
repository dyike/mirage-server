package capture

import (
	"context"
	"errors"
	"github.com/dyike/mirage-server/internal/video"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPipelineValidation(t *testing.T) {
	base := Options{Test: true, Codec: "hevc", Width: 1920, Height: 1080, FPS: 60, Bitrate: 12000}
	pipeline, err := Pipeline(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pipeline, "vah265enc") || !strings.Contains(pipeline, "alignment=au") || !strings.Contains(pipeline, "b-frames=0") {
		t.Fatal(pipeline)
	}
	for _, modify := range []func(*Options){func(o *Options) { o.Width = 8193 }, func(o *Options) { o.Height = 3 }, func(o *Options) { o.FPS = 0 }, func(o *Options) { o.Codec = "hevc ! filesink location=x" }, func(o *Options) { o.Test = false; o.FD = -1 }} {
		o := base
		modify(&o)
		if _, err := Pipeline(o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
}

func TestCompositeCapture(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("opt-in multi-screen GPU capture")
	}
	for _, codec := range []string{"raw", "h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := errors.New("captured")
			count := 0
			o := Options{Test: true, Codec: codec, Width: 640, Height: 240, FPS: 30, Bitrate: 2000, Layers: []Layer{{Width: 320, Height: 240}, {X: 320, Width: 320, Height: 240}}}
			err := Run(ctx, o, func(f Frame) error {
				if len(f.Data) == 0 {
					t.Error("empty composite")
				}
				if codec == "raw" && len(f.Data) != 640*240*4 {
					t.Errorf("invalid raw frame %d", len(f.Data))
				}
				count++
				if count == 3 {
					return done
				}
				return nil
			})
			if !errors.Is(err, done) {
				t.Fatal(err)
			}
		})
	}
}

func TestHardwareVideoBitrateBudget(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("requires VA-API")
	}
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			var rates []float64
			for _, budget := range []int{2000, 8000} {
				o := Options{Test: true, Codec: codec, Width: 1920, Height: 1080, FPS: 30, Bitrate: budget}
				p, err := Pipeline(o)
				if err != nil {
					t.Fatal(err)
				}
				p = strings.Replace(p, "pattern=ball", "pattern=snow", 1)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				frames, bytes := 0, 0
				done := errors.New("measured")
				err = runPipeline(ctx, p, func(data []byte) error {
					frames++
					if frames > 30 {
						bytes += len(data)
					}
					if frames == 90 {
						return done
					}
					return nil
				})
				cancel()
				if !errors.Is(err, done) {
					t.Fatal(err)
				}
				kbps := float64(bytes*8) / 2 / 1000
				rates = append(rates, kbps)
				t.Logf("%s limit=%d steady payload=%.0f kbit/s", codec, budget, kbps)
				// VA driver rate control is advisory: the server separately
				// paces actual wire bytes, including high-entropy overshoot.
			}
			if rates[0] > rates[1]*0.85 {
				t.Fatalf("lower bitrate did not reduce payload: %v", rates)
			}
		})
	}
}

// Keep one real VA encoder running while repeatedly changing its bitrate.
func TestLiveBitrateDoesNotRestartCapture(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("requires VA-API")
	}
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var update func(int)
			binds, frames := 0, 0
			var previous time.Time
			var maxGap time.Duration
			stop := errors.New("complete")
			o := Options{Test: true, Codec: codec, Width: 3840, Height: 2160, FPS: 30, Bitrate: 2000, BindBitrate: func(f func(int)) {
				update = f
				if f != nil {
					binds++
				}
			}}
			err := Run(ctx, o, func(f Frame) error {
				now := time.Now()
				if frames > 5 {
					maxGap = max(maxGap, now.Sub(previous))
				}
				previous = now
				frames++
				if update == nil {
					t.Fatal("encoder control missing")
				}
				switch frames {
				case 30:
					update(8000)
				case 60:
					update(1000)
				case 90:
					update(4000)
				case 120:
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if binds != 1 || update != nil {
				t.Fatalf("binding lifecycle: binds=%d active=%v", binds, update != nil)
			}
			if maxGap > 500*time.Millisecond {
				t.Fatalf("capture stalled: %v", maxGap)
			}
			t.Logf("%s 4K: %d frames, one pipeline, three live bitrate changes, max frame gap %v", codec, frames, maxGap)
		})
	}
}

func TestHardwareKeyframeWithoutRestart(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("opt-in hardware encoder")
	}
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var request func() bool
			binds, frames := 0, 0
			requested := 0
			found := false
			stop := errors.New("keyframe received")
			opts := Options{Test: true, Codec: codec, Width: 640, Height: 360, FPS: 30, Bitrate: 2000, BindKeyframe: func(f func() bool) {
				request = f
				if f != nil {
					binds++
				}
			}}
			err := Run(ctx, opts, func(frame Frame) error {
				frames++
				if frames == 10 {
					if request == nil || !request() {
						t.Fatal("encoder rejected keyframe request")
					}
					requested = frames
					return nil
				}
				if requested == 0 {
					return nil
				}
				c := video.H264
				if codec == "hevc" {
					c = video.HEVC
				}
				for _, nal := range video.AppendAnnexB(nil, frame.Data) {
					kind := video.NALType(c, nal)
					if c == video.H264 && kind == 5 || c == video.HEVC && kind >= 16 && kind <= 21 {
						found = true
						return stop
					}
				}
				if frames-requested > 5 {
					t.Fatal("keyframe did not arrive promptly")
				}
				return nil
			})
			if !errors.Is(err, stop) || !found || binds != 1 || request != nil {
				t.Fatalf("keyframe lifecycle: err=%v found=%v binds=%d active=%v", err, found, binds, request != nil)
			}
		})
	}
}

func TestPipelineInteractiveQuantizerLimit(t *testing.T) {
	o := Options{Test: true, Codec: "hevc", Width: 3840, Height: 2160, FPS: 30, Bitrate: 6000, MinQP: 30}
	p, err := Pipeline(o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "min-qp=30") || !strings.Contains(p, "max-buffers=1 drop=false") {
		t.Fatal(p)
	}
	o.MinQP = 52
	if _, err := Pipeline(o); err == nil {
		t.Fatal("accepted invalid quantizer")
	}
}
