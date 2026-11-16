package capture

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeMultiOutputCapture(t *testing.T) {
	names := strings.Split(os.Getenv("MIRAGE_TEST_NATIVE_OUTPUTS"), ",")
	if len(names) != 2 {
		t.Skip("set MIRAGE_TEST_NATIVE_OUTPUTS to two output names")
	}
	for _, codec := range []string{"hevc", "h264", "raw"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			done := errors.New("captured")
			frames := 0
			mapsBefore := dmaMapAttempts.Load()
			target := 10
			if n, err := strconv.Atoi(os.Getenv("MIRAGE_TEST_CAPTURE_FRAMES")); err == nil && n > 0 {
				target = n
			}

			opts := Options{Codec: codec, Width: 4608, Height: 1454, FPS: 60, Bitrate: 40000, Layers: []Layer{{Output: names[0], Width: 2021, Height: 1135}, {Output: names[1], X: 2021, Width: 2587, Height: 1454}}}
			// This device's H.264 encoder caps the width at 4096; HEVC tests
			// the full deployed 4608-pixel composition independently.
			if codec == "h264" {
				opts.Width, opts.Height = 3840, 1212
				opts.Layers[0].Width, opts.Layers[0].Height = 1684, 947
				opts.Layers[1].X, opts.Layers[1].Width, opts.Layers[1].Height = 1684, 2156, 1212
			}
			if codec == "raw" {
				opts.Width, opts.Height = 2914, 818
				opts.Layers[0].Width, opts.Layers[0].Height = 1457, 818
				opts.Layers[1].X, opts.Layers[1].Width, opts.Layers[1].Height = 1457, 1457, 818
			}
			started := time.Now()
			err := Run(ctx, opts, func(f Frame) error {
				if len(f.Data) == 0 {
					return errors.New("empty frame")
				}
				frames++
				if codec == "raw" && frames == 1 {
					counts := [2]int{}
					for y := 0; y < opts.Height; y++ {
						for x := 0; x < opts.Width; x++ {
							at := (y*opts.Width + x) * 4
							if f.Data[at] > 8 || f.Data[at+1] > 8 || f.Data[at+2] > 8 {
								counts[x/1457]++
							}
						}
					}
					t.Logf("raw first composite: elapsed=%s nonblack-pixels left=%d right=%d", time.Since(started), counts[0], counts[1])
					if counts[0] == 0 || counts[1] == 0 {
						t.Error("native composite contains an entirely black output")
					}
				}
				if frames == target {
					return done
				}
				return nil
			})
			if codec != "raw" && dmaMapAttempts.Load() != mapsBefore {
				t.Error("zero-copy path attempted CPU pixel mapping")
			}
			dmaReleases.Range(func(key, value any) bool { t.Error("DMA allocation retained after shutdown"); return false })
			if !errors.Is(err, done) {
				t.Fatalf("frames=%d: %v", frames, err)
			}
		})
	}
}

func TestNativeGPUCompositionPipeline(t *testing.T) {
	o := Options{ComposeOnly: true, Codec: "hevc", Width: 1920, Height: 640, FPS: 60, Bitrate: 8000, Layers: []Layer{{Output: "DP-2", SourceWidth: 3840, SourceHeight: 2160, Format: "BGRx", Width: 840, Height: 472}, {Output: "HDMI-A-1", SourceWidth: 3840, SourceHeight: 2160, Format: "BGRx", X: 840, Width: 1080, Height: 608}}}
	p, err := Pipeline(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"glvideomixerelement", "memory:DMABuf", "memory:GLMemory", "drm-format=XR24"} {
		if !strings.Contains(p, required) {
			t.Fatalf("missing %s", required)
		}
	}
	for _, forbidden := range []string{"gldownload", "videoconvert", "vacompositor"} {
		if strings.Contains(p, forbidden) {
			t.Fatalf("CPU transfer or unsupported composition: %s", p)
		}
	}
}
