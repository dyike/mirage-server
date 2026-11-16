package server

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
)

func TestZlibRectanglesUseContinuousStreamAndFullPixels(t *testing.T) {
	const w, h = 530, 142
	e := new(pixelEncoder)
	defer e.close()
	var compressed, expected []byte
	for frame := 0; frame < 2; frame++ {
		rgba := make([]byte, w*h*4)
		for i := 0; i < len(rgba); i += 4 {
			rgba[i] = byte(i/4 + frame)
			rgba[i+1] = byte(i / w)
			rgba[i+2] = 193
			rgba[i+3] = 255
		}
		message, err := e.encode(encZlib, defaultRFBFormat, rgba, w, h)
		if err != nil {
			t.Fatal(err)
		}
		n := int(networkOrder.Uint16(message[2:]))
		if n != 6 {
			t.Fatalf("unbounded rectangles: %d", n)
		}
		remaining := message[4:]
		covered := make([]bool, w*h)
		for i := 0; i < n; i++ {
			if len(remaining) < 16 {
				t.Fatal("short header")
			}
			x, y := int(networkOrder.Uint16(remaining)), int(networkOrder.Uint16(remaining[2:]))
			rw, rh := int(networkOrder.Uint16(remaining[4:])), int(networkOrder.Uint16(remaining[6:]))
			if networkOrder.Uint32(remaining[8:]) != 6 || rw > 256 || rh > 128 || x+rw > w || y+rh > h {
				t.Fatal("invalid rectangle")
			}
			size := int(networkOrder.Uint32(remaining[12:]))
			if size > len(remaining)-16 {
				t.Fatal("bad compressed length")
			}
			compressed = append(compressed, remaining[16:16+size]...)
			remaining = remaining[16+size:]
			for yy := y; yy < y+rh; yy++ {
				for xx := x; xx < x+rw; xx++ {
					pos := yy*w + xx
					if covered[pos] {
						t.Fatal("overlap")
					}
					covered[pos] = true
					expected = append(expected, rgba[pos*4:pos*4+3]...)
					expected = append(expected, 0)
				}
			}
		}
		if len(remaining) != 0 {
			t.Fatal("trailing message bytes")
		}
		for _, v := range covered {
			if !v {
				t.Fatal("missing pixels")
			}
		}
	}
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := make([]byte, len(expected))
	if _, err = io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("pixel data mismatch or deflate stream reset")
	}
}
