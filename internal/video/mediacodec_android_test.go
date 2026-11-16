//go:build android && cgo

package video

import (
	_ "embed"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/video/internal/devicetest"
)

// Small ffmpeg-generated 8-bit, no-B-frame Annex B streams. Embedding lets
// this test binary run directly over adb without ffmpeg or fixture files.
//
//go:embed testdata/red.hevc
var androidRedHEVC []byte

//go:embed testdata/red.h264
var androidRedH264 []byte

//go:embed testdata/blue.hevc
var androidBlueHEVC []byte

// 17 red frames followed by a blue final frame, 320x242, 30 fps, no B frames.
// ffmpeg lavfi color sources, concat, libx265 bframes=0:repeat-headers=1:aud=1.
//
//go:embed testdata/red_blue.hevc
var androidRedBlueHEVC []byte

// One intra frame, 3840x2160. Repeating it measures decoder output without
// network variability; it does not measure application drawing or screen FPS.
// ffmpeg color=red:s=3840x2160:r=60, libx265 ultrafast, bframes=0:aud=1.
//
//go:embed testdata/red_4k.hevc
var androidRed4KHEVC []byte

func BenchmarkMediaCodec4KOutput(b *testing.B) {
	if !devicetest.StartBinderPool() {
		b.Fatal("standalone test requires Binder thread pool initialization")
	}
	frames := make(chan [4]byte, 1)
	d, err := NewDecoder(HEVC, func(f Frame) {
		offset := (f.H/2)*f.Stride + (f.W/2)*4
		frames <- [4]byte(f.Pix[offset : offset+4])
	})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	units := accessUnits(HEVC, SplitAnnexB(androidRed4KHEVC))
	if len(units) != 1 {
		b.Fatal("expected one intra frame", len(units))
	}
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	frame := func() {
		b.Helper()
		if err := d.Decode(units[0]); err != nil {
			b.Fatal(err)
		}
		select {
		case p := <-frames:
			if p[0] > 25 || p[1] > 25 || p[2] < 230 || p[3] != 255 {
				b.Fatal("incorrect red BGRA frame", p)
			}
		case <-timeout.C:
			b.Fatal("4K output timed out", d.Status())
		}
	}
	frame() // exclude codec startup
	b.ResetTimer()
	for b.Loop() {
		frame()
	}
	b.StopTimer()
	b.ReportMetric(1e9/float64(b.Elapsed().Nanoseconds())*float64(b.N), "output-fps")
	b.Logf("decoder output: %+v", d.Status())
}

func TestMediaCodecSlowRendererKeepsLatestFrame(t *testing.T) {
	if !devicetest.StartBinderPool() {
		t.Fatal("standalone test requires Binder thread pool initialization")
	}
	frames := make(chan [4]byte, 32)
	d, err := NewDecoder(HEVC, func(f Frame) {
		offset := (f.H/2)*f.Stride + (f.W/2)*4
		frames <- [4]byte(f.Pix[offset : offset+4])
		// Model an expensive framebuffer copy/draw while input keeps arriving.
		time.Sleep(50 * time.Millisecond)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	units := accessUnits(HEVC, SplitAnnexB(androidRedBlueHEVC))
	start := time.Now()
	for _, au := range units {
		if err := d.Decode(au); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(3 * time.Second)
	for shown := 1; ; shown++ {
		select {
		case p := <-frames:
			if p[0] > 230 && p[1] < 25 && p[2] < 25 && p[3] == 255 {
				t.Logf("final blue frame after %v; displayed %d of %d frames", time.Since(start), shown, len(units))
				if shown >= len(units) {
					t.Fatal("slow renderer replayed every stale frame")
				}
				return
			}
		case <-deadline:
			t.Fatalf("final blue frame never arrived without more input: %+v", d.Status())
		}
	}
}

func TestMediaCodecDevice(t *testing.T) {
	if !devicetest.StartBinderPool() {
		t.Fatal("standalone test requires Binder thread pool initialization; run in an APK on older Android")
	}
	for _, codec := range []Codec{HEVC, H264} {
		t.Run(codec.String(), func(t *testing.T) {
			type sample struct {
				w, h   int
				pixel  [4]byte
				source frame.Timing
			}
			frames := make(chan sample, 16)
			d, err := NewDecoder(codec, func(f Frame) {
				offset := (f.H/2)*f.Stride + (f.W/2)*4
				frames <- sample{f.W, f.H, [4]byte(f.Pix[offset : offset+4]), f.Timing}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.SetCPUFrames(false) // renderer callback still requires Android pixels
			check := func(stream []byte, w, h int, red bool) {
				t.Helper()
				units := accessUnits(codec, SplitAnnexB(stream))
				for _, au := range units {
					sourceTime := frame.Timing{Source: time.Now().Add(-100 * time.Millisecond), ClockSynced: true, ClockError: 2 * time.Millisecond}
					if err := d.DecodeTimed(au, sourceTime); err != nil {
						t.Fatal(err)
					}
					// Submit separately: a burst may intentionally coalesce
					// decoded output to keep the desktop current.
					select {
					case frame := <-frames:
						if frame.source != sourceTime {
							t.Fatalf("decoder lost the source timestamp: got %v, want %v", frame.source, sourceTime)
						}
						if frame.w != w || frame.h != h {
							t.Fatalf("frame size %dx%d; want %dx%d", frame.w, frame.h, w, h)
						}
						p := frame.pixel
						if p[3] != 255 || p[1] > 25 || red && (p[2] < 230 || p[0] > 25) || !red && (p[0] < 230 || p[2] > 25) {
							t.Fatalf("wrong BGRA color %v (red=%v)", p, red)
						}
					case <-time.After(5 * time.Second):
						t.Fatalf("last frame was not delivered without another input: %+v", d.Status())
					}
				}
				status := d.Status()
				t.Logf("decoder: %+v", status)
				if status.Backend != "MediaCodec" || status.Name == "" || status.Error != "" {
					t.Fatalf("invalid status: %+v", status)
				}
				if status.HardwareKnown && !status.Hardware {
					t.Fatalf("software decoder selected: %+v", status)
				}
			}
			stream := androidRedHEVC
			if codec == H264 {
				stream = androidRedH264
			}
			check(stream, 320, 242, true)
			if codec == HEVC {
				check(androidBlueHEVC, 640, 360, false)
			}
			d.Close()
		})
	}
}
