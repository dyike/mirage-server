//go:build linux && !android && (amd64 || arm64)

package video

import (
	"github.com/dyike/mirage-server/internal/frame"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/keel/gio/op"
	"github.com/dyike/keel/gio/op/clip"
	"github.com/dyike/keel/gio/op/paint"
)

func TestGStreamerDMAMetadataABI(t *testing.T) {
	var meta gstVideoMeta
	if unsafe.Offsetof(meta.Offset) != 48 || unsafe.Offsetof(meta.Stride) != 80 || unsafe.Offsetof(meta.Planes) != 44 {
		t.Fatal("GstVideoMeta ABI mismatch")
	}
	for _, tc := range []struct {
		input    string
		fourcc   uint32
		modifier uint64
	}{
		{"NV12", 0x3231564e, 0},
		{"NV12:0x0200000018601b04", 0x3231564e, 0x0200000018601b04},
		{"P010:0x0", 0x30313050, 0},
	} {
		format, modifier, err := parseDRMFormat(tc.input)
		if err != nil || format != tc.fourcc || modifier != tc.modifier {
			t.Fatalf("%q: %#x %#x %v", tc.input, format, modifier, err)
		}
	}
	for _, value := range []string{"", "NV1", "NV12:garbage", "NV12:0x0:0x1"} {
		if _, _, err := parseDRMFormat(value); err == nil {
			t.Fatalf("accepted invalid format %q", value)
		}
	}
}

func TestGStreamerZeroCopy(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("enable the Linux GPU test")
	}
	gstNativeSamples.Lock()
	before := len(gstNativeSamples.all)
	gstNativeSamples.Unlock()
	t.Cleanup(func() {
		gstNativeSamples.Lock()
		defer gstNativeSamples.Unlock()
		if after := len(gstNativeSamples.all); after != before {
			t.Errorf("native samples survived GPU teardown: before=%d after=%d", before, after)
		}
	})
	for _, tc := range []struct {
		name, file    string
		codec         Codec
		width, height int
		blue          bool
	}{
		{"H264", "red.h264", H264, 320, 242, false},
		{"HEVC", "red.hevc", HEVC, 320, 242, false},
		{"HEVCBlue", "blue.hevc", HEVC, 640, 360, true},
		{"HEVC4K", "red_4k.hevc", HEVC, 3840, 2160, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window, err := headless.NewWindow(tc.width, tc.height)
			if err != nil {
				t.Fatal(err)
			}
			defer window.Release()
			if !paint.DMABufSupported() {
				t.Fatal("EGL renderer does not support DMA-BUF")
			}
			frames := make(chan Frame, 1)
			d, err := NewDecoder(tc.codec, func(f Frame) {
				RetainBuffer(f.Buffer)
				select {
				case frames <- f:
				default:
					ReleaseBuffer(f.Buffer)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			d.SetCPUFrames(false)
			if !d.native {
				t.Fatalf("native pipeline unavailable: %+v", d.Status())
			}
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			timing := frame.Timing{Source: time.Now(), ClockSynced: true, ClockError: time.Millisecond}
			if err := d.DecodeTimed(SplitAnnexB(data), timing); err != nil {
				t.Fatal(err)
			}
			var f Frame
			select {
			case f = <-frames:
			case <-time.After(5 * time.Second):
				t.Fatalf("no native picture: %+v", d.Status())
			}
			if f.Timing != timing {
				t.Fatalf("native picture lost timing: %+v", f.Timing)
			}
			defer ReleaseBuffer(f.Buffer)
			d.Close()
			for len(frames) > 0 {
				ReleaseBuffer((<-frames).Buffer)
			}
			if len(f.Pix) != 0 || f.Surface == 0 || f.Buffer == 0 || f.W != tc.width || f.H != tc.height {
				t.Fatalf("not a native frame: %+v", f)
			}
			var surf paint.Surface
			surf.Set(f.Surface, image.Pt(f.W, f.H))
			var ops op.Ops
			paint.NewSurfaceOp(&surf).Add(&ops)
			paint.PaintOp{}.Add(&ops)
			if err := window.Frame(&ops); err != nil {
				t.Fatal(err)
			}
			img := image.NewRGBA(image.Rect(0, 0, tc.width, tc.height))
			if err := window.Screenshot(img); err != nil {
				t.Fatal(err)
			}
			checkNativeColor(t, img, tc.blue)
			status := d.Status()
			if !status.Hardware || !status.ZeroCopy || status.ReadbackFrames != 0 || status.NativeFrames == 0 || status.Error != "" {
				t.Fatalf("unexpected native status: %+v", status)
			}
			// On-demand screenshots must still work without enabling per-frame
			// readback or closing/reconfiguring the live decoder.
			snapshot := Snapshot(f.Buffer)
			if snapshot == nil {
				t.Fatal("native frame screenshot failed")
			}
			checkNativeColor(t, snapshot, tc.blue)
			t.Logf("%+v", d.Status())
		})
	}
}

func checkNativeColor(t *testing.T, img *image.RGBA, blue bool) {
	t.Helper()
	c := img.RGBAAt(img.Bounds().Dx()/2, img.Bounds().Dy()/2)
	if blue {
		c.R, c.B = c.B, c.R
	}
	if c.R < 200 || c.G > 40 || c.B > 40 || c.A != 255 {
		t.Fatalf("wrong native pixel color: %+v (blue=%v)", c, blue)
	}
}

func TestGStreamerZeroCopyDisabledKeepsCPUOutput(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("enable the Linux GPU test")
	}
	t.Setenv("MIRAGE_LINUX_ZERO_COPY", "0")
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	frames := make(chan bool, 8)
	d, err := NewDecoder(HEVC, func(f Frame) { frames <- len(f.Pix) > 0 && f.Surface == 0 && f.Buffer == 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetCPUFrames(false)
	if d.native {
		t.Fatal("explicit fallback still selects DMA-BUF")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "red.hevc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(SplitAnnexB(data)); err != nil {
		t.Fatal(err)
	}
	select {
	case cpu := <-frames:
		if !cpu {
			t.Fatal("CPU fallback did not produce pixels")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no CPU fallback frame: %+v", d.Status())
	}
	if status := d.Status(); status.ZeroCopy || status.ReadbackFrames == 0 {
		t.Fatalf("incorrect fallback diagnostics: %+v", status)
	}
}

func TestDMABufImportFailureReportsError(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("enable the Linux GPU test")
	}
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	failures := make(chan error, 1)
	h, err := paint.RegisterDMABuf(paint.DMABuf{Width: 64, Height: 64, FourCC: 0x3231564e, Planes: []paint.DMABufPlane{{FD: 9999999, Pitch: 64}, {FD: 9999999, Pitch: 64, Offset: 4096}}, OnError: func(err error) { failures <- err }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer paint.ReleaseDMABuf(h)
	var surf paint.Surface
	surf.Set(h, image.Pt(64, 64))
	var ops op.Ops
	paint.NewSurfaceOp(&surf).Add(&ops)
	paint.PaintOp{}.Add(&ops)
	if err := window.Frame(&ops); err != nil {
		t.Fatal(err)
	}
	select {
	case <-failures:
	default:
		t.Fatal("failed GPU import was silently ignored")
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	if err := window.Screenshot(img); err != nil {
		t.Fatal(err)
	}
	if img.RGBAAt(32, 32).A != 0 {
		t.Fatal("unsupported surface drew uninitialized pixels")
	}
}

func TestGStreamerZeroCopyOpacity(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("enable the Linux GPU test")
	}
	window, err := headless.NewWindow(320, 242)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	frames := make(chan Frame, 1)
	d, err := NewDecoder(HEVC, func(f Frame) {
		RetainBuffer(f.Buffer)
		select {
		case frames <- f:
		default:
			ReleaseBuffer(f.Buffer)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetCPUFrames(false)
	stream, err := os.ReadFile(filepath.Join("testdata", "red.hevc"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(SplitAnnexB(stream)); err != nil {
		t.Fatal(err)
	}
	var f Frame
	select {
	case f = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("no native frame")
	}
	defer ReleaseBuffer(f.Buffer)
	d.Close()
	for len(frames) > 0 {
		ReleaseBuffer((<-frames).Buffer)
	}
	reference := Snapshot(f.Buffer)
	if reference == nil {
		t.Fatal("no CPU reference")
	}
	var surf paint.Surface
	surf.Set(f.Surface, image.Pt(f.W, f.H))
	render := func(native bool) *image.RGBA {
		var ops op.Ops
		paint.Fill(&ops, color.NRGBA{R: 82, G: 98, B: 115, A: 255})
		opacity := paint.PushOpacity(&ops, .45)
		area := clip.UniformRRect(image.Rect(20, 20, 300, 222), 12).Push(&ops)
		if native {
			paint.NewSurfaceOp(&surf).Add(&ops)
		} else {
			paint.NewImageOp(reference).Add(&ops)
		}
		paint.PaintOp{}.Add(&ops)
		paint.FillShape(&ops, color.NRGBA{G: 200, B: 120, A: 128}, clip.Rect(image.Rect(80, 80, 240, 180)).Op())
		area.Pop()
		opacity.Pop()
		if err := window.Frame(&ops); err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(image.Rect(0, 0, 320, 242))
		if err := window.Screenshot(img); err != nil {
			t.Fatal(err)
		}
		return img
	}
	native, uploaded := render(true), render(false)
	var sum int
	for i, b := range native.Pix {
		delta := int(b) - int(uploaded.Pix[i])
		sum += max(delta, -delta)
	}
	if average := float64(sum) / float64(len(native.Pix)); average > 2 {
		t.Fatalf("native opacity layer differs from uploaded reference: %.2f", average)
	}
}

func TestGStreamerZeroCopyColorAndClipping(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("enable the Linux GPU test")
	}
	stream, reference := encode(t, HEVC, 320, 240, 1)
	window, err := headless.NewWindow(320, 240)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	frames := make(chan Frame, 1)
	d, err := NewDecoder(HEVC, func(f Frame) { RetainBuffer(f.Buffer); frames <- f })
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetCPUFrames(false)
	if err := d.Decode(SplitAnnexB(stream)); err != nil {
		t.Fatal(err)
	}
	var f Frame
	select {
	case f = <-frames:
	case <-time.After(5 * time.Second):
		t.Fatalf("no native picture: %+v", d.Status())
	}
	defer ReleaseBuffer(f.Buffer)
	var surf paint.Surface
	surf.Set(f.Surface, image.Pt(f.W, f.H))
	for _, clipped := range []bool{false, true} {
		var ops op.Ops
		var area clip.Stack
		if clipped {
			area = clip.UniformRRect(image.Rect(20, 20, 300, 220), 12).Push(&ops)
		}
		paint.NewSurfaceOp(&surf).Add(&ops)
		paint.PaintOp{}.Add(&ops)
		if clipped {
			area.Pop()
		}
		if err := window.Frame(&ops); err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(image.Rect(0, 0, 320, 240))
		if err := window.Screenshot(img); err != nil {
			t.Fatal(err)
		}
		var sum, count int
		for y := 35; y < 205; y++ {
			for x := 35; x < 285; x++ {
				i := y*img.Stride + x*4
				for _, pair := range [][2]int{{0, 2}, {1, 1}, {2, 0}} {
					delta := int(img.Pix[i+pair[0]]) - int(reference[i+pair[1]])
					sum += max(delta, -delta)
					count++
				}
			}
		}
		if average := float64(sum) / float64(count); average > 6 {
			t.Fatalf("native colors differ from reference by %.2f (clipped=%v)", average, clipped)
		}
		if clipped && img.RGBAAt(0, 0).A != 0 {
			t.Fatal("native image escaped rounded clip")
		}
	}
}
