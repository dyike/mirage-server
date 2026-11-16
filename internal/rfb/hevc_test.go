package rfb

import (
	"os"
	"testing"
)

func TestHEVCAccessUnits(t *testing.T) {
	data, err := os.ReadFile("../video/testdata/red_blue.hevc")
	if err != nil {
		t.Fatal(err)
	}
	units, err := hevcAccessUnits(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(units) < 2 {
		t.Fatalf("expected multiple pictures, got %d", len(units))
	}
	for _, unit := range units {
		pictures := 0
		for _, nal := range unit {
			if (nal[0]>>1)&63 <= 31 && nal[2]&128 != 0 {
				pictures++
			}
		}
		if pictures != 1 {
			t.Fatalf("access unit has %d first slices", pictures)
		}
	}
	for _, data := range [][]byte{{0, 0, 1, 0x26}, {0, 0, 1, 0x26, 0}, {0, 0, 1, 0x26, 1}} {
		if _, err := hevcAccessUnits(data); err == nil {
			t.Fatalf("accepted malformed NAL %x", data)
		}
	}
}
