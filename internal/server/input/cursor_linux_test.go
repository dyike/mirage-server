package input

import (
	"os"
	"testing"
	"time"
)

func TestLiveCursor(t *testing.T) {
	name := os.Getenv("MIRAGE_TEST_CURSOR_OUTPUT")
	if name == "" {
		t.Skip("opt-in cursor capture")
	}
	c, err := OpenCursor(name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case v, ok := <-c.Updates():
		if !ok {
			t.Fatal("cursor capture disconnected")
		}
		if v.Width < 1 || v.Height < 1 || len(v.Pixels) != v.Width*v.Height*4 {
			t.Fatalf("invalid cursor %dx%d", v.Width, v.Height)
		}
		t.Logf("native cursor %dx%d hotspot=%v", v.Width, v.Height, v.Hotspot)
	case <-time.After(3 * time.Second):
		t.Fatal("no cursor captured")
	}
}
