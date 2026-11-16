package video

import (
	"os"
	"testing"
)

func TestSPSDimensions(t *testing.T) {
	for _, test := range []struct {
		file  string
		codec Codec
		w, h  int
	}{
		{"testdata/red.h264", H264, 320, 242},
		{"testdata/red.hevc", HEVC, 320, 242},
		{"testdata/blue.hevc", HEVC, 640, 360},
	} {
		t.Run(test.file, func(t *testing.T) {
			stream, err := os.ReadFile(test.file)
			if err != nil {
				t.Fatal(err)
			}
			sps := 7
			if test.codec == HEVC {
				sps = 33
			}
			found := false
			for _, nal := range SplitAnnexB(stream) {
				if NALType(test.codec, nal) != sps {
					continue
				}
				found = true
				w, h, err := spsDimensions(test.codec, nal)
				if err != nil || w != test.w || h != test.h {
					t.Fatalf("got %dx%d, %v; want %dx%d", w, h, err, test.w, test.h)
				}
				for n := range min(4, len(nal)) {
					if _, _, err := spsDimensions(test.codec, nal[:n]); err == nil {
						t.Fatalf("accepted truncated SPS of %d bytes", n)
					}
				}
			}
			if !found {
				t.Fatal("fixture has no SPS")
			}
		})
	}
}

func FuzzSPSDimensions(f *testing.F) {
	for _, file := range []string{"testdata/red.h264", "testdata/red.hevc"} {
		stream, _ := os.ReadFile(file)
		for _, nal := range SplitAnnexB(stream) {
			f.Add(nal)
		}
	}
	f.Fuzz(func(t *testing.T, nal []byte) {
		for _, codec := range []Codec{H264, HEVC} {
			w, h, err := spsDimensions(codec, nal)
			if err == nil && (w <= 0 || h <= 0 || w > 16384 || h > 16384) {
				t.Fatalf("accepted invalid dimensions %dx%d", w, h)
			}
		}
	})
}
