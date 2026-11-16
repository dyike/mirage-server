//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && amd64)

package video

import (
	"github.com/dyike/mirage-server/internal/frame"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

func TestGStreamerMappingABI(t *testing.T) {
	if unsafe.Sizeof(gstBufferHeader{}) != 112 || unsafe.Offsetof(gstBufferHeader{}.PTS) != 72 {
		t.Fatal("GstBuffer ABI mismatch")
	}
	var info gstMapInfo
	if unsafe.Sizeof(info) != 104 || unsafe.Offsetof(info.Data) != 16 || unsafe.Offsetof(info.Size) != 24 {
		t.Fatal("GstMapInfo ABI mismatch")
	}
	if unsafe.Sizeof(gstError{}) != 16 {
		t.Fatal("GError ABI mismatch")
	}
}

func TestGStreamerHardwareDecode(t *testing.T) {
	flag := "MIRAGE_TEST_LINUX_VIDEO"
	if runtime.GOOS == "darwin" {
		flag = "MIRAGE_TEST_GSTREAMER_VIDEO"
	}
	if os.Getenv(flag) != "1" {
		t.Skip("enable the platform GPU test on a configured GStreamer host")
	}
	for _, tc := range []struct {
		name, file    string
		codec         Codec
		width, height int
	}{
		{"H264", "red.h264", H264, 320, 242},
		{"HEVC", "red.hevc", HEVC, 320, 242},
		{"HEVC4K", "red_4k.hevc", HEVC, 3840, 2160},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timing := frame.Timing{Source: time.Now().Add(-50 * time.Millisecond), ClockSynced: true, ClockError: time.Millisecond}
			arrived := make(chan struct{}, 1)
			d, err := NewDecoder(tc.codec, func(f Frame) {
				if f.Timing != timing {
					t.Errorf("decode lost frame timing: got=%+v want=%+v", f.Timing, timing)
				}
				if f.W != tc.width || f.H != tc.height {
					t.Errorf("unexpected output: %dx%d", f.W, f.H)
					return
				}
				i := f.Stride*(f.H/2) + (f.W/2)*4
				if f.Pix[i+2] < 200 || f.Pix[i] > 40 || f.Pix[i+1] > 40 {
					t.Error("hardware decoder did not produce red")
				}
				select {
				case arrived <- struct{}{}:
				default:
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			stream, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.DecodeTimed(SplitAnnexB(stream), timing); err != nil {
				t.Fatal(err)
			}
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatalf("no GPU output: %+v", d.Status())
			}
			if status := d.Status(); !status.HardwareKnown || !status.Hardware || status.Error != "" {
				t.Fatal(status)
			} else {
				t.Logf("%+v", status)
			}
		})
	}
}
