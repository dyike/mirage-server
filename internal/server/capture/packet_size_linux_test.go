package capture

import "testing"

func TestRawCapturePacketSize(t *testing.T) {
	n := rawFrameBytes(Options{Codec: "raw", Width: 4608, Height: 2592})
	if n <= 32<<20 || !validCapturePacketSize(uint64(n), n) {
		t.Fatal("valid large RGBA frame rejected")
	}
	if validCapturePacketSize(uint64(n), 0) {
		t.Fatal("oversized compressed packet accepted")
	}
	for _, size := range []uint64{0, uint64(n - 1), uint64(n + 1)} {
		if validCapturePacketSize(size, n) {
			t.Fatal("malformed raw frame accepted")
		}
	}
	if rawFrameBytes(Options{Codec: "raw", Width: 8193, Height: 1}) != 0 {
		t.Fatal("unbounded raw size")
	}
}
