package rfb

import (
	"bufio"
	"bytes"
	"image"
	"testing"
)

func TestExtendedDesktopLayout(t *testing.T) {
	c := &Client{}
	payload := make([]byte, 36)
	payload[0] = 2
	for i := 0; i < 2; i++ {
		b := payload[4+i*16:]
		be.PutUint32(b, uint32(i+7))
		be.PutUint16(b[4:], uint16(i*1920))
		be.PutUint16(b[8:], 1920)
		be.PutUint16(b[10:], 1080)
		be.PutUint32(b[12:], 0x80000000)
	}
	read := func(reason, status int, data []byte) error {
		c.r = bufio.NewReader(bytes.NewReader(data))
		return c.readExtendedDesktopSize(image.Rect(reason, status, reason+3840, status+1080))
	}
	if err := read(0, 0, payload); err != nil {
		t.Fatal(err)
	}
	l := c.Desktop()
	if l.Size != image.Pt(3840, 1080) || len(l.Screens) != 2 || l.Screens[1].ID != 8 || l.Screens[1].Bounds != image.Rect(1920, 0, 3840, 1080) || l.Screens[1].Flags != 0x80000000 {
		t.Fatalf("layout: %+v", l)
	}
	if c.layout.Load() != nil {
		t.Fatal("standard layout leaked into Apple transforms")
	}
	l.Screens[0].ID = 99
	if c.Desktop().Screens[0].ID != 7 {
		t.Fatal("snapshot aliases stored layout")
	}
	if err := read(1, 1, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if len(c.Desktop().Screens) != 2 {
		t.Fatal("rejected request replaced layout")
	}
	if err := read(0, 0, payload[:20]); err == nil {
		t.Fatal("accepted truncated layout")
	}
	if len(c.Desktop().Screens) != 2 {
		t.Fatal("partial packet replaced layout")
	}
	if err := read(2, 9, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if len(c.Desktop().Screens) != 0 {
		t.Fatal("server-side change incorrectly interpreted status")
	}
}
