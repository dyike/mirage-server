package video

import (
	"bytes"
	"os"
	"testing"
)

func TestH264LowLatencySPS(t *testing.T) {
	data, err := os.ReadFile("testdata/red.h264")
	if err != nil {
		t.Fatal(err)
	}
	for _, nal := range SplitAnnexB(data) {
		original := append([]byte(nil), nal...)
		out := h264LowLatencySPS(nal)
		if !bytes.Equal(nal, original) {
			t.Fatal("mutated input")
		}
		if nal[0]&31 != 7 {
			if !bytes.Equal(out, nal) {
				t.Fatal("changed non-SPS")
			}
			continue
		}
		if bytes.Equal(out, nal) {
			t.Fatal("baseline DPB was not expanded")
		}
		w, h, err := spsDimensions(H264, out)
		if err != nil || w != 320 || h != 242 {
			t.Fatal(w, h, err)
		}
		if !bytes.Equal(out, h264LowLatencySPS(out)) {
			t.Fatal("workaround not idempotent")
		}
		for i := 0; i < len(nal); i++ {
			h264LowLatencySPS(nal[:i])
		}
	}
}
func FuzzH264LowLatencySPS(f *testing.F) {
	f.Add([]byte{0x67, 66, 0, 30, 0x80})
	f.Fuzz(func(t *testing.T, nal []byte) { h264LowLatencySPS(nal) })
}
