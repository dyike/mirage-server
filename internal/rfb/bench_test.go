package rfb

import (
	"bufio"
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"sync"
	"testing"

	"github.com/klauspost/compress/zlib"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// desktopPage renders a tall web-page-like image: a toolbar, a sidebar,
// antialiased text paragraphs, buttons and photo-like blocks. Scrolling it
// gives frames whose ZRLE mix resembles a real 4K Retina desktop.
func desktopPage(w, h int) *image.RGBA {
	page := image.NewRGBA(image.Rect(0, 0, w, h))
	fill := func(r image.Rectangle, c color.RGBA) {
		draw.Draw(page, r, &image.Uniform{c}, image.Point{}, draw.Src)
	}
	fill(page.Bounds(), color.RGBA{255, 255, 255, 255})
	fill(image.Rect(0, 0, 520, h), color.RGBA{243, 244, 246, 255}) // sidebar

	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		panic(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 26, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	text := []string{
		"Mirage connects to macOS Screen Sharing and VNC servers with a native renderer.",
		"Scrolling a long page repaints most of the screen, which is what this benchmark measures.",
		"The quick brown fox jumps over the lazy dog, 0123456789, Pack my box with five dozen liquor jugs.",
		"Remote framebuffer updates arrive as ZRLE rectangles: zlib-compressed tiles of 64 by 64 pixels.",
	}
	d := &font.Drawer{Dst: page, Src: image.NewUniform(color.RGBA{30, 30, 34, 255}), Face: face}
	seed := uint32(7)
	for y := 120; y < h-40; {
		switch (y / 40) % 9 {
		case 3: // photo block: smooth gradient plus grain
			r := image.Rect(620, y, 620+1400, y+600)
			for py := r.Min.Y; py < min(r.Max.Y, h); py++ {
				for px := r.Min.X; px < r.Max.X; px++ {
					seed = seed*1664525 + 1013904223
					n := int(seed>>28) - 8
					i := page.PixOffset(px, py)
					page.Pix[i] = byte(clamp(80 + (px-r.Min.X)/8 + n))
					page.Pix[i+1] = byte(clamp(120 + (py-r.Min.Y)/5 + n))
					page.Pix[i+2] = byte(clamp(170 - (px-r.Min.X)/12 + n))
				}
			}
			y += 640
		case 6: // a row of buttons
			for i := range 5 {
				fill(image.Rect(620+i*360, y, 620+i*360+320, y+72), color.RGBA{37, 99, 235, 255})
			}
			y += 110
		default: // a paragraph
			for l := range 6 {
				d.Dot = fixed.P(620, y+l*44)
				d.DrawString(text[(y/44+l)%len(text)])
				d.Dot = fixed.P(40, y+l*44)
				if l == 0 {
					d.DrawString("Sidebar item")
				}
			}
			y += 6*44 + 40
		}
	}
	fill(image.Rect(0, 0, w, 96), color.RGBA{249, 250, 251, 255}) // toolbar
	return page
}

func clamp(v int) int { return max(0, min(255, v)) }

// encodeZRLEBest encodes like a real server: each tile takes the smallest
// of the subencodings that can represent it.
func encodeZRLEBest(img *image.RGBA, r image.Rectangle) []byte {
	var out, cand bytes.Buffer
	pixels := make([][3]byte, 0, 64*64)
	for ty := r.Min.Y; ty < r.Max.Y; ty += 64 {
		for tx := r.Min.X; tx < r.Max.X; tx += 64 {
			tw, th := min(64, r.Max.X-tx), min(64, r.Max.Y-ty)
			pixels = pixels[:0]
			index := map[[3]byte]int{}
			var palette [][3]byte
			for y := range th {
				for x := range tw {
					i := img.PixOffset(tx+x, ty+y)
					p := [3]byte{img.Pix[i], img.Pix[i+1], img.Pix[i+2]}
					pixels = append(pixels, p)
					if _, ok := index[p]; !ok && len(palette) < 128 {
						index[p] = len(palette)
						palette = append(palette, p)
					}
				}
			}
			if len(index) == 1 {
				out.WriteByte(1)
				out.Write(palette[0][:])
				continue
			}
			var best []byte
			try := func(enc func(*bytes.Buffer)) {
				cand.Reset()
				enc(&cand)
				if best == nil || cand.Len() < len(best) {
					best = append(best[:0], cand.Bytes()...)
				}
			}
			try(func(b *bytes.Buffer) { // raw
				b.WriteByte(0)
				for _, p := range pixels {
					b.Write(p[:])
				}
			})
			try(func(b *bytes.Buffer) { // plain RLE
				b.WriteByte(128)
				for i := 0; i < len(pixels); {
					n := 1
					for i+n < len(pixels) && pixels[i+n] == pixels[i] {
						n++
					}
					b.Write(pixels[i][:])
					writeRun(b, n)
					i += n
				}
			})
			complete := len(index) < 128 // every pixel is in the palette
			if complete && len(palette) <= 16 {
				try(func(b *bytes.Buffer) { // packed palette
					b.WriteByte(byte(len(palette)))
					for _, p := range palette {
						b.Write(p[:])
					}
					bpp := 4
					if len(palette) == 2 {
						bpp = 1
					} else if len(palette) <= 4 {
						bpp = 2
					}
					for y := range th {
						var cur byte
						nb := 0
						for x := range tw {
							cur = cur<<bpp | byte(index[pixels[y*tw+x]])
							if nb += bpp; nb == 8 {
								b.WriteByte(cur)
								cur, nb = 0, 0
							}
						}
						if nb > 0 {
							b.WriteByte(cur << (8 - nb))
						}
					}
				})
			}
			if complete {
				try(func(b *bytes.Buffer) { // palette RLE
					b.WriteByte(byte(128 + len(palette)))
					for _, p := range palette {
						b.Write(p[:])
					}
					for i := 0; i < len(pixels); {
						n := 1
						for i+n < len(pixels) && pixels[i+n] == pixels[i] {
							n++
						}
						if n == 1 {
							b.WriteByte(byte(index[pixels[i]]))
						} else {
							b.WriteByte(byte(index[pixels[i]]) | 0x80)
							writeRun(b, n)
						}
						i += n
					}
				})
			}
			out.Write(best)
		}
	}
	return out.Bytes()
}

// scrollStream is a server byte stream of full-screen ZRLE updates showing
// the page scrolled further each frame, as in a real 4K scroll trace.
var scrollStream = sync.OnceValues(func() ([]byte, int) {
	const w, h, frames, step = 3840, 2160, 6, 240
	page := desktopPage(w, h+frames*step)
	var zbuf, stream bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zbuf, zlib.DefaultCompression)
	frame := image.NewRGBA(image.Rect(0, 0, w, h))
	for f := range frames {
		draw.Draw(frame, frame.Bounds(), page, image.Pt(0, f*step), draw.Src)
		zbuf.Reset()
		zw.Write(encodeZRLEBest(frame, frame.Bounds()))
		zw.Flush()
		stream.Write([]byte{0, 0, 0, 1})
		stream.Write(rectHeader(frame.Bounds(), EncZRLE))
		stream.Write(u32(uint32(zbuf.Len())))
		stream.Write(zbuf.Bytes())
	}
	return stream.Bytes(), frames
})

// BenchmarkScroll4K decodes full-screen 4K ZRLE scroll frames: the client's
// share of the time per frame in a real scrolling session.
func BenchmarkScroll4K(b *testing.B) {
	stream, frames := scrollStream()
	c := &Client{}
	c.resizeFB(3840, 2160)
	for b.Loop() {
		// Restart the zlib stream but keep buffers, as a long connection would.
		c.zrle.stop()
		c.r = bufio.NewReaderSize(bytes.NewReader(stream), 256<<10)
		for range frames {
			if t, _ := c.r.ReadByte(); t != 0 {
				b.Fatal("not an update")
			}
			if err := c.framebufferUpdate(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Microseconds())/1e3/float64(b.N*frames), "ms/frame")
	b.ReportMetric(float64(len(stream))/float64(frames)/1e3, "KB/frame")
}

// BenchmarkApple555Scroll4K decodes the same scroll frames as
// BenchmarkScroll4K, sent as Apple RGB555 (one zlib stream per rectangle,
// level 1, as macOS sends them).
func BenchmarkApple555Scroll4K(b *testing.B) {
	const w, h, frames, step = 3840, 2160, 6, 240
	page := desktopPage(w, h+frames*step)
	frame := image.NewRGBA(image.Rect(0, 0, w, h))
	var stream bytes.Buffer
	for f := range frames {
		draw.Draw(frame, frame.Bounds(), page, image.Pt(0, f*step), draw.Src)
		var zbuf bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zbuf, 1)
		zw.Write(encode555(frame, frame.Bounds()))
		zw.Close()
		stream.Write([]byte{0, 0, 0, 1})
		stream.Write(rectHeader(frame.Bounds(), EncAppleRGB555))
		stream.Write(u32(uint32(zbuf.Len())))
		stream.Write(zbuf.Bytes())
	}
	c := &Client{}
	c.resizeFB(w, h)
	for b.Loop() {
		c.r = bufio.NewReaderSize(bytes.NewReader(stream.Bytes()), 256<<10)
		for range frames {
			c.r.ReadByte()
			if err := c.framebufferUpdate(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(b.Elapsed().Microseconds())/1e3/float64(b.N*frames), "ms/frame")
	b.ReportMetric(float64(stream.Len())/float64(frames)/1e3, "KB/frame")
}
