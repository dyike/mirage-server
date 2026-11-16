package rfb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"
)

func compactTight(n int) []byte {
	b := []byte{byte(n & 127)}
	if n > 127 {
		b[0] |= 128
		b = append(b, byte(n>>7&127))
		if n > 16383 {
			b[1] |= 128
			b = append(b, byte(n>>14))
		}
	}
	return b
}
func tightClient(t *testing.T, w, h int) *Client {
	t.Helper()
	c := &Client{}
	c.fb.Resize(w, h)
	t.Cleanup(func() {
		for i := range c.tight {
			c.tight[i].stop()
		}
	})
	return c
}
func decodeTightBytes(c *Client, r image.Rectangle, data []byte) error {
	c.r = bufio.NewReader(bytes.NewReader(data))
	return c.decodeTight(r)
}
func rgbPixels(rgb []byte) []byte {
	out := make([]byte, 0, len(rgb)/3*4)
	for i := 0; i < len(rgb); i += 3 {
		out = append(out, rgb[i], rgb[i+1], rgb[i+2], 255)
	}
	return out
}

func TestTightCopyStreamsAndReset(t *testing.T) {
	c := tightClient(t, 4, 2)
	r := c.fb.Bounds()
	for stream := 0; stream < 4; stream++ {
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		for frame := 0; frame < 3; frame++ {
			rgb := make([]byte, 24)
			for i := range rgb {
				rgb[i] = byte(i*7 + frame*31 + stream)
			}
			compressed.Reset()
			if _, err := writer.Write(rgb); err != nil {
				t.Fatal(err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
			wire := []byte{byte(stream << 4)}
			wire = append(wire, compactTight(compressed.Len())...)
			wire = append(wire, compressed.Bytes()...)
			if err := decodeTightBytes(c, r, wire); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(c.fb.Pix, rgbPixels(rgb)) {
				t.Fatal("Tight stream reuse corrupted pixels")
			}
		}
		writer.Close()
	}
	// Even Fill resets all four streams before drawing its pixel.
	if err := decodeTightBytes(c, r, []byte{0x8f, 11, 22, 33}); err != nil {
		t.Fatal(err)
	}
	for i := range c.tight {
		if c.tight[i].src != nil {
			t.Fatal("Fill did not reset compression stream")
		}
	}
	if !bytes.Equal(c.fb.Pix, bytes.Repeat([]byte{11, 22, 33, 255}, 8)) {
		t.Fatal("Fill color mismatch")
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	rgb := bytes.Repeat([]byte{9, 8, 7}, 8)
	writer.Write(rgb)
	writer.Flush()
	defer writer.Close()
	wire := append([]byte{0}, compactTight(compressed.Len())...)
	wire = append(wire, compressed.Bytes()...)
	if err := decodeTightBytes(c, r, wire); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.fb.Pix, rgbPixels(rgb)) {
		t.Fatal("reset stream did not accept fresh zlib header")
	}
}
func TestTightFilters(t *testing.T) {
	t.Run("small-copy", func(t *testing.T) {
		c := tightClient(t, 2, 1)
		rgb := []byte{1, 2, 3, 4, 5, 6}
		if err := decodeTightBytes(c, c.fb.Bounds(), append([]byte{0}, rgb...)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.fb.Pix, rgbPixels(rgb)) {
			t.Fatal("uncompressed copy mismatch")
		}
	})
	t.Run("two-color-row-padding", func(t *testing.T) {
		c := tightClient(t, 3, 2)
		wire := []byte{0x40, 1, 1, 255, 0, 0, 0, 255, 0, 0xa0, 0x40}
		if err := decodeTightBytes(c, c.fb.Bounds(), wire); err != nil {
			t.Fatal(err)
		}
		want := []byte{0, 255, 0, 255, 0, 0, 0, 255, 0, 255, 0, 0, 0, 255, 0, 255, 0, 0}
		if !bytes.Equal(c.fb.Pix, rgbPixels(want)) {
			t.Fatal("palette bit packing or row padding incorrect")
		}
	})
	t.Run("three-color", func(t *testing.T) {
		c := tightClient(t, 3, 1)
		wire := []byte{0x40, 1, 2, 1, 2, 3, 4, 5, 6, 7, 8, 9, 2, 0, 1}
		if err := decodeTightBytes(c, c.fb.Bounds(), wire); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.fb.Pix, rgbPixels([]byte{7, 8, 9, 1, 2, 3, 4, 5, 6})) {
			t.Fatal("palette indices incorrect")
		}
	})
	t.Run("gradient-clamp-wrap", func(t *testing.T) {
		c := tightClient(t, 2, 2)
		rgb := []byte{250, 0, 80, 255, 1, 70, 240, 2, 60, 1, 255, 50}
		delta := make([]byte, len(rgb))
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				for ch := 0; ch < 3; ch++ {
					i := (y*2+x)*3 + ch
					l, u, ul := 0, 0, 0
					if x > 0 {
						l = int(rgb[i-3])
					}
					if y > 0 {
						u = int(rgb[i-6])
					}
					if x > 0 && y > 0 {
						ul = int(rgb[i-9])
					}
					delta[i] = rgb[i] - byte(max(0, min(255, l+u-ul)))
				}
			}
		}
		var compressed bytes.Buffer
		zw := zlib.NewWriter(&compressed)
		zw.Write(delta)
		zw.Flush()
		defer zw.Close()
		wire := append([]byte{0x40, 2}, compactTight(compressed.Len())...)
		wire = append(wire, compressed.Bytes()...)
		if err := decodeTightBytes(c, c.fb.Bounds(), wire); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.fb.Pix, rgbPixels(rgb)) {
			t.Fatal("gradient reconstruction mismatch")
		}
	})
}
func TestTightJPEGAndBounds(t *testing.T) {
	c := tightClient(t, 10, 10)
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:], []byte{210, 30, 10, 255})
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{0x90}, compactTight(data.Len())...)
	wire = append(wire, data.Bytes()...)
	rect := image.Rect(2, 3, 6, 7)
	if err := decodeTightBytes(c, rect, wire); err != nil {
		t.Fatal(err)
	}
	reference, err := jpeg.Decode(bytes.NewReader(data.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			i := (y*10 + x) * 4
			want := color.RGBA{}
			if image.Pt(x, y).In(rect) {
				want = color.RGBAModel.Convert(reference.At(x-2, y-3)).(color.RGBA)
			}
			if !bytes.Equal(c.fb.Pix[i:i+4], []byte{want.R, want.G, want.B, want.A}) {
				t.Fatal("JPEG changed pixels outside its rectangle or incorrect color")
			}
		}
	}
	if err := decodeTightBytes(c, image.Rect(0, 0, 3, 3), wire); err == nil {
		t.Fatal("JPEG dimensions mismatch accepted")
	}
}
func TestTightMalformedControls(t *testing.T) {
	for _, wire := range [][]byte{{0xf0}, {0x40, 3}, {0x40, 1, 0}, {0x40, 1, 2, 1, 2, 3, 4, 5, 6, 7, 8, 9, 3}, {0x80, 1, 2}, {0x90, 0x80}} {
		c := tightClient(t, 1, 1)
		if err := decodeTightBytes(c, c.fb.Bounds(), wire); err == nil {
			t.Fatalf("malformed Tight accepted: %x", wire)
		}
	}
}
func TestUnchangedDesktopSizeKeepsPixels(t *testing.T) {
	for _, encoding := range []int32{EncPseudoDesktopSize, EncPseudoExtendedDesktopSize} {
		c := tightClient(t, 2, 2)
		want := bytes.Repeat([]byte{1, 2, 3, 255}, 4)
		copy(c.fb.Pix, want)
		packet := append([]byte{0, 0, 1}, rectHeader(c.fb.Bounds(), encoding)...)
		if encoding == EncPseudoExtendedDesktopSize {
			packet = append(packet, 0, 0, 0, 0)
		}
		c.r = bufio.NewReader(bytes.NewReader(packet))
		if err := c.framebufferUpdate(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c.fb.Pix, want) {
			t.Fatal("unchanged desktop size cleared visible pixels")
		}
	}
}

func TestTightTruncatedInflateCancels(t *testing.T) {
	c := tightClient(t, 4, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.decodeContext = ctx
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	zw.Flush()
	defer zw.Close()
	wire := append([]byte{0}, compactTight(compressed.Len())...)
	wire = append(wire, compressed.Bytes()...)
	done := make(chan error, 1)
	go func() { done <- decodeTightBytes(c, c.fb.Bounds(), wire) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("truncated pixel data accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("closing did not cancel incomplete Tight inflate")
	}
}
