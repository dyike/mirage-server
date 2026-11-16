package rfb

import (
	"bytes"
	"encoding/binary"
	"image"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
)

// Apple RGB555 (encoding 0x3ea) is one of macOS Screen Sharing's adaptive
// quality tiers. Determined from captures of a macOS server:
//
//	u32  n             length of the compressed payload
//	[n]  zlib stream   level 1; inflates to w×h big-endian 16-bit pixels,
//	                   0RRRRRGG GGGBBBBB
//
// The community reverse-engineering notes call it RGB 5-6-5; the captured
// pixels are 5-5-5. The server spends far less time encoding it than ZRLE,
// at the cost of 15-bit colour.
//
// The connection carries one zlib stream: the first payload starts with
// the zlib header, and every payload ends at a flush point, so the next
// starts on a block boundary. Each payload is inflated to its end with a
// decompressor reset onto the last 32 KB of output as its dictionary.
// Continuing one decompressor across payloads instead broke when a block's
// end-of-block code was left unread at the end of a payload. A payload may
// also end its stream; the next then starts a new one with a header. The
// client
// inflates the raw deflate data and skips zlib's Adler-32 check: over 16 MB
// of pixels per 4K frame it cost a third of the decode time, and TCP
// already checksums.
type apple555State struct {
	src     bytes.Reader
	zr      io.ReadCloser
	started bool
	in      []byte
	out     []byte
	hist    []byte // last 32 KB of output, the next payload's dictionary
}

const zlibWindow = 32 << 10

// rgb555 maps a 15-bit pixel to the framebuffer's RGBA word: each 5-bit
// channel is widened by repeating its top bits, so 31 becomes 255.
var rgb555 = sync.OnceValue(func() *[1 << 15]uint32 {
	var t [1 << 15]uint32
	for v := range t {
		r, g, b := uint32(v>>10&31), uint32(v>>5&31), uint32(v&31)
		r, g, b = r<<3|r>>2, g<<3|g>>2, b<<3|b>>2
		t[v] = r | g<<8 | b<<16 | opaque
	}
	return &t
})

func isZlibHeader(b []byte) bool {
	return len(b) >= 2 && b[0]&0x0f == 8 && (uint16(b[0])<<8|uint16(b[1]))%31 == 0
}

func (c *Client) decodeApple555(r image.Rectangle) error {
	var lb [4]byte
	if _, err := io.ReadFull(c.r, lb[:]); err != nil {
		return err
	}
	n := be.Uint32(lb[:])
	if n > 64<<20 {
		return protoErr("Apple RGB555 payload of %d bytes", n)
	}
	a := &c.a555
	if cap(a.in) < int(n) {
		a.in = make([]byte, n)
	}
	a.in = a.in[:n]
	if _, err := io.ReadFull(c.r, a.in); err != nil { // network, no lock held
		return err
	}
	size := r.Dx() * r.Dy() * 2
	if cap(a.out) < size {
		a.out = make([]byte, size)
	}
	a.out = a.out[:size]
	in := a.in
	if !a.started {
		if !isZlibHeader(in) {
			return protoErr("Apple RGB555 stream does not start with a zlib header")
		}
		in, a.started, a.hist = in[2:], true, a.hist[:0]
	}
	a.src.Reset(in)
	if a.zr == nil {
		a.zr = flate.NewReaderDict(&a.src, a.hist)
	} else if err := a.zr.(flate.Resetter).Reset(&a.src, a.hist); err != nil {
		return err
	}
	if _, err := io.ReadFull(a.zr, a.out); err != nil {
		return protoErr("Apple RGB555 payload: %v", err)
	}
	// Consume the rest of the payload: the end of the block and the flush
	// point, or the stream's final block and Adler-32 trailer, after which
	// the next payload starts a new stream.
	var extra [1]byte
	switch k, err := a.zr.Read(extra[:]); {
	case k > 0:
		return protoErr("Apple RGB555 payload holds more than its %v rectangle", r)
	case err == io.EOF && a.src.Len() <= 4:
		a.started = false
	case err == io.ErrUnexpectedEOF && a.src.Len() == 0:
	default:
		return protoErr("Apple RGB555 payload ends badly: %v, %d bytes left", err, a.src.Len())
	}
	if size >= zlibWindow {
		a.hist = append(a.hist[:0], a.out[size-zlibWindow:]...)
	} else {
		keep := min(len(a.hist), zlibWindow-size)
		a.hist = append(a.hist[:copy(a.hist, a.hist[len(a.hist)-keep:])], a.out...)
	}

	c.lockFB()
	defer c.unlockFB()
	convert555(c.pix32(), c.fb.W, r, a.out)
	c.wrote(r)
	return nil
}

// convert555 writes rectangle r of 16-bit pixels into pix, splitting large
// rectangles across cores.
func convert555(pix []uint32, stride int, r image.Rectangle, src []byte) {
	lut := rgb555()
	w := r.Dx()
	rows := func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			dst := pix[(r.Min.Y+y)*stride+r.Min.X:][:w]
			row := src[y*w*2:][:w*2]
			for x := range dst {
				dst[x] = lut[binary.BigEndian.Uint16(row[2*x:])&0x7fff]
			}
		}
	}
	h := r.Dy()
	workers := min(runtime.GOMAXPROCS(0), h/64)
	if w*h < 1<<18 || workers < 2 {
		rows(0, h)
		return
	}
	var wg sync.WaitGroup
	for i := range workers {
		y0, y1 := h*i/workers, h*(i+1)/workers
		wg.Go(func() { rows(y0, y1) })
	}
	wg.Wait()
}
