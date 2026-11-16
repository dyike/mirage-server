package rfb

import (
	"bufio"
	"bytes"
	"image"
	"io"
	"net"
	"testing"
	"time"
)

func TestAppleSystemCursorClearsCustomShapeAndKeepsCache(t *testing.T) {
	bitmap := image.NewRGBA(image.Rect(0, 0, 16, 16))
	hot := image.Pt(2, 3)
	var shapes []*image.RGBA
	var hotspots []image.Point
	var wire bytes.Buffer
	// A cached bitmap, a system cursor (ID only), then the cached bitmap
	// again. The system cursor must neither retain the old shape nor consume
	// bytes belonging to the next message.
	for _, data := range [][]byte{u32(1001), u32(0), u32(0), u32(1001), u32(0)} {
		wire.Write(data)
	}
	c := &Client{
		r: bufio.NewReader(&wire),
		cfg: Config{OnCursor: func(img *image.RGBA, at image.Point) {
			shapes = append(shapes, img)
			hotspots = append(hotspots, at)
		}},
		cursors: map[uint32]appleCursor{1001: {img: bitmap, hot: hot}},
	}
	for range 3 {
		if err := c.appleCursor(image.Rect(0, 0, 16, 16)); err != nil {
			t.Fatal(err)
		}
	}
	if len(shapes) != 3 || shapes[0] != bitmap || shapes[1] == nil || !shapes[1].Bounds().Empty() || shapes[2] != bitmap {
		t.Fatalf("system cursor did not reset the image or damaged the cache: %v", shapes)
	}
	if hotspots[0] != hot || hotspots[1] != (image.Point{}) || hotspots[2] != hot {
		t.Fatalf("wrong cursor hotspots: %v", hotspots)
	}
}

func TestAppleSameSizeLayoutRearmsCursorUpdates(t *testing.T) {
	for _, push := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll", true: "push"}[push], func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			var wire bytes.Buffer
			wire.Write([]byte{0, 0, 1}) // FramebufferUpdate body, one rectangle
			wire.Write(rectHeader(image.Rectangle{}, EncAppleDisplayLayout))
			wire.Write(displayLayoutPayload(image.Pt(1280, 720), image.Pt(1280, 720), 1))
			c := &Client{conn: client, r: bufio.NewReader(&wire), apple: true, appleExt: true, setUp: true}
			c.appleCmds[msgAppleAutoUpdate/8] |= 0x80 >> (msgAppleAutoUpdate % 8)
			c.resizeFB(1280, 720)
			c.pushing.Store(push)
			done := make(chan error, 1)
			go func() { done <- c.framebufferUpdate() }()
			server.SetReadDeadline(time.Now().Add(time.Second))
			packet := make([]byte, 26)
			if _, err := io.ReadFull(server, packet); err != nil {
				t.Fatalf("same-size layout did not refresh and rearm cursor updates: %v", err)
			}
			if packet[0] != 3 || packet[1] != 0 || packet[10] != msgAppleAutoUpdate {
				t.Fatalf("unexpected layout recovery: %x", packet)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
