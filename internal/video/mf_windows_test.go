//go:build windows && (amd64 || arm64)

package video

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestMFNV12PaddedCrop(t *testing.T) {
	// Visible 2x2 image in a 4x4 texture. UV starts after all four padded
	// luma rows; using the visible height gives green instead of red.
	src := make([]byte, 4*6)
	for i := range src {
		src[i] = 81
	}
	for i := 16; i < len(src); i += 2 {
		src[i], src[i+1] = 90, 240
	}
	dst := make([]byte, 2*2*4)
	mfNV12ToBGRA(dst, src, 2, 2, 4, 4, 2, 2)
	for i := 0; i < len(dst); i += 4 {
		if dst[i] > 3 || dst[i+1] > 3 || dst[i+2] < 250 || dst[i+3] != 255 {
			t.Fatalf("BGRA pixel: %v", dst[i:i+4])
		}
	}
}

func TestMFNV12Ranges(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nominal      uint32
		black, white byte
	}{
		{"limited", 2, 16, 235}, {"full", 1, 0, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte{tc.black, tc.white, tc.black, tc.white, 128, 128}
			dst := make([]byte, 16)
			mfNV12ToBGRA(dst, src, 2, 2, 2, 2, 1, tc.nominal)
			if dst[0] != 0 || dst[1] != 0 || dst[2] != 0 || dst[4] != 255 || dst[5] != 255 || dst[6] != 255 {
				t.Fatal(dst)
			}
		})
	}
}

func BenchmarkMFNV12ToBGRA(b *testing.B) {
	for _, size := range []image.Point{image.Pt(1920, 1080), image.Pt(3840, 2160), image.Pt(5120, 2880)} {
		b.Run(fmt.Sprintf("%dx%d", size.X, size.Y), func(b *testing.B) {
			src := make([]byte, size.X*size.Y*3/2)
			dst := make([]byte, size.X*size.Y*4)
			b.SetBytes(int64(len(dst)))
			b.ResetTimer()
			for b.Loop() {
				mfNV12ToBGRA(dst, src, size.X, size.Y, size.X, size.Y, 1, 2)
			}
		})
	}
}

func BenchmarkMFGPUConversion(b *testing.B) {
	if os.Getenv("MIRAGE_TEST_WINDOWS_VIDEO") != "1" {
		b.Skip("requires Windows GPU")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := mfProc(coInitialize, 0, 0); err != nil {
		b.Fatal(err)
	}
	defer coUninitialize.Call()
	if err := mfProc(mfStartup, 0x20070, 0); err != nil {
		b.Fatal(err)
	}
	defer mfShutdown.Call()
	n := &mfNative{d: &Decoder{codec: HEVC}}
	if err := n.init(); err != nil {
		b.Fatal(err)
	}
	defer n.close()
	for _, size := range []image.Point{image.Pt(1920, 1080), image.Pt(3840, 2160), image.Pt(5120, 2880)} {
		b.Run(fmt.Sprintf("%dx%d", size.X, size.Y), func(b *testing.B) {
			n.w, n.h = size.X, size.Y
			src := make([]byte, size.X*size.Y*3/2)
			for i := range src {
				src[i] = 128
			}
			desc := mfTextureDesc{Width: uint32(size.X), Height: uint32(size.Y), MipLevels: 1, ArraySize: 1, Format: 103, SampleCount: 1, BindFlags: 0x200}
			data := struct {
				Data              *byte
				Pitch, SlicePitch uint32
			}{&src[0], uint32(size.X), uint32(len(src))}
			var texture *mfCOM
			if err := mfCheck("test CreateTexture2D", n.device.call(5, uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&texture)))); err != nil {
				b.Fatal(err)
			}
			defer texture.release()
			p := &mfVideoProcessor{owner: n}
			defer p.close()
			emit := func(f Frame) { runtime.KeepAlive(f.Pix) }
			if err := p.frame(texture, desc, 0, emit); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(size.X * size.Y * 4))
			b.ResetTimer()
			for b.Loop() {
				if err := p.frame(texture, desc, 0, emit); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Explicit opt-in: GPU/driver availability must not turn CI into a software
// decoding test or a misleading pass. This test requires actual DXGI output.
func TestMFHardwareDecode(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_WINDOWS_VIDEO") != "1" {
		t.Skip("set MIRAGE_TEST_WINDOWS_VIDEO=1 on a Windows GPU host")
	}
	for _, tc := range []struct {
		codec Codec
		file  string
	}{{HEVC, "red.hevc"}, {H264, "red.h264"}} {
		t.Run(tc.codec.String(), func(t *testing.T) {
			stream, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var picture Frame
			arrived := make(chan struct{}, 1)
			d, err := NewDecoder(tc.codec, func(f Frame) {
				mu.Lock()
				picture = f
				picture.Pix = append([]byte(nil), f.Pix...)
				mu.Unlock()
				select {
				case arrived <- struct{}{}:
				default:
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Decode(nil); err != ErrNoParameterSets {
				t.Fatalf("before SPS: %v", err)
			}
			nals := SplitAnnexB(stream)
			// Split test streams at AUDs, preserving the original access units.
			aud := 9
			if tc.codec == HEVC {
				aud = 35
			}
			var units [][][]byte
			for _, nal := range nals {
				if len(units) == 0 || NALType(tc.codec, nal) == aud {
					units = append(units, nil)
				}
				units[len(units)-1] = append(units[len(units)-1], nal)
			}
			for _, unit := range units {
				if err := d.Decode(unit); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatalf("no hardware picture: %+v", d.Status())
			}
			status := d.Status()
			if !status.HardwareKnown || !status.Hardware || status.Error != "" {
				t.Fatalf("not hardware: %+v", status)
			}
			mu.Lock()
			got := picture
			mu.Unlock()
			if got.W != 320 || got.H < 240 {
				t.Fatalf("unexpected dimensions: %+v", image.Pt(got.W, got.H))
			}
			i := ((got.H/2)*got.W + got.W/2) * 4
			if p := got.Pix[i : i+4]; p[2] < 200 || p[0] > 40 || p[1] > 40 {
				t.Fatalf("not red: %v", p)
			}
			if tc.codec == HEVC {
				// Reconfigure twice on the same decoder, exercising padded output,
				// retired GPU resources, and a final still image without new input.
				for _, file := range []string{"blue.hevc", "red.hevc"} {
					stream, err := os.ReadFile(filepath.Join("testdata", file))
					if err != nil {
						t.Fatal(err)
					}
					for len(arrived) > 0 {
						<-arrived
					}
					for _, unit := range accessUnits(HEVC, SplitAnnexB(stream)) {
						if err := d.Decode(unit); err != nil {
							t.Fatal(err)
						}
					}
					select {
					case <-arrived:
					case <-time.After(5 * time.Second):
						t.Fatalf("no resized picture: %+v", d.Status())
					}
					mu.Lock()
					got = picture
					mu.Unlock()
					wantSize := image.Pt(320, 242)
					channel := 2
					if file == "blue.hevc" {
						wantSize, channel = image.Pt(640, 360), 0
					}
					if image.Pt(got.W, got.H) != wantSize {
						t.Fatalf("resized to %dx%d, want %v", got.W, got.H, wantSize)
					}
					i := ((got.H/2)*got.W + got.W/2) * 4
					if got.Pix[i+channel] < 200 || got.Pix[i+2-channel] > 40 {
						t.Fatalf("wrong resized color: %v", got.Pix[i:i+4])
					}
				}
			}
			d.Close()
			d.Close() // idempotent shutdown
			if err := d.Decode(nil); err == nil {
				t.Fatal("Decode after Close succeeded")
			}
			t.Logf("%+v", status)
		})
	}
}

func TestMFHardwareBurstKeepsFinalStill(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_WINDOWS_VIDEO") != "1" {
		t.Skip("requires Windows GPU")
	}
	stream, err := os.ReadFile(filepath.Join("testdata", "red_blue.hevc"))
	if err != nil {
		t.Fatal(err)
	}
	blue := make(chan struct{}, 1)
	d, err := NewDecoder(HEVC, func(f Frame) {
		i := (f.H/2)*f.Stride + (f.W/2)*4
		if f.Pix[i] > 200 && f.Pix[i+2] < 40 {
			select {
			case blue <- struct{}{}:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	units := accessUnits(HEVC, SplitAnnexB(stream))
	for range 10 {
		for _, unit := range units {
			if err := d.Decode(unit); err != nil {
				t.Fatal(err)
			}
		}
	}
	select {
	case <-blue:
	case <-time.After(5 * time.Second):
		t.Fatalf("last still picture was lost: %+v", d.Status())
	}
}
