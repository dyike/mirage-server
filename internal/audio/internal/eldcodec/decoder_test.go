//go:build (windows || linux) && !android && (amd64 || arm64)

package eldcodec

import "testing"

func TestNativeELDConfigurationAndIsolation(t *testing.T) {
	for range 3 {
		d, err := New([]byte{0xf8, 0xe6, 0x50, 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Decode(nil); err == nil {
			t.Fatal("empty packet accepted")
		}
		d.Close()
		d.Close()
		if _, err := d.Decode([]byte{0}); err == nil {
			t.Fatal("closed decoder accepted packet")
		}
	}
	if d, err := New([]byte{0}); err == nil {
		d.Close()
		t.Fatal("invalid configuration accepted")
	}
}
