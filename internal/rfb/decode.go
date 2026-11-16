package rfb

import (
	"bufio"
	"image"
	"io"
	"sync"
	"unsafe"

	"github.com/dyike/mirage-server/internal/inputtrace"
	"github.com/klauspost/compress/flate"
)

// All decoders write through pix32, a uint32 view of Framebuffer.Pix. With
// rgbaFormat on a little-endian host a pixel value is R | G<<8 | B<<16 |
// A<<24, which is image.RGBA's byte order. Every supported target (arm64,
// amd64, wasm) is little-endian.
const opaque = 0xFF000000

func (c *Client) pix32() []uint32 {
	p := c.fb.Pix
	if len(p) == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&p[0])), len(p)/4)
}

// decodeRaw reads rows in chunks of about 1 MB without the framebuffer lock,
// then copies each chunk in under it.
func (c *Client) decodeRaw(r image.Rectangle) error {
	w := r.Dx() * 4
	rows := max(1, (1<<20)/max(w, 1))
	for y0 := r.Min.Y; y0 < r.Max.Y; y0 += rows {
		n := min(rows, r.Max.Y-y0)
		buf := c.scratch(n * w)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return err
		}
		c.lockFB()
		pix := c.pix32()
		for y := range n {
			off := ((y0+y)*c.fb.W + r.Min.X) * 4
			copy(c.fb.Pix[off:off+w], buf[y*w:(y+1)*w])
			row := pix[off/4 : off/4+r.Dx()]
			for i := range row {
				row[i] |= opaque // the server sends a zero padding byte
			}
		}
		c.wrote(image.Rect(r.Min.X, y0, r.Max.X, y0+n))
		c.unlockFB()
	}
	return nil
}

func (c *Client) scratch(n int) []byte {
	if cap(c.tmp) < n {
		c.tmp = make([]byte, n)
	}
	return c.tmp[:n]
}

func (c *Client) decodeCopyRect(r image.Rectangle) error {
	var b [4]byte
	if _, err := io.ReadFull(c.r, b[:]); err != nil {
		return err
	}
	src := image.Pt(int(be.Uint16(b[0:])), int(be.Uint16(b[2:])))
	if !r.Sub(r.Min).Add(src).In(c.fb.Bounds()) {
		return protoErr("CopyRect source %v outside framebuffer", src)
	}
	c.lockFB()
	defer c.unlockFB()
	stride, w := c.fb.W*4, r.Dx()*4
	rowCopy := func(y int) {
		d := (r.Min.Y+y)*stride + r.Min.X*4
		s := (src.Y+y)*stride + src.X*4
		copy(c.fb.Pix[d:d+w], c.fb.Pix[s:s+w]) // copy handles overlap within a row
	}
	if src.Y < r.Min.Y { // moving down: copy bottom-up so sources survive
		for y := r.Dy() - 1; y >= 0; y-- {
			rowCopy(y)
		}
	} else {
		for y := 0; y < r.Dy(); y++ {
			rowCopy(y)
		}
	}
	c.wrote(r)
	return nil
}

// decodeCursor reads a Cursor pseudo-encoding rectangle: size.X*size.Y
// pixels in the client pixel format, then a 1-bit opacity mask with each row
// padded to a byte, most significant bit first.
func (c *Client) decodeCursor(size, hotspot image.Point) error {
	w, h := size.X, size.Y
	if w > 512 || h > 512 {
		return protoErr("cursor of %dx%d", w, h)
	}
	maskStride := (w + 7) / 8
	buf := c.scratch(w*h*4 + maskStride*h)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	if c.cfg.OnCursor == nil {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pix, mask := buf[:w*h*4], buf[w*h*4:]
	for y := range h {
		for x := range w {
			if mask[y*maskStride+x/8]&(0x80>>(x%8)) == 0 {
				continue // transparent: zero is premultiplied transparent
			}
			i := (y*w + x) * 4
			copy(img.Pix[i:i+3], pix[i:i+3])
			img.Pix[i+3] = 0xFF
		}
	}
	inputtrace.Cursor("rfb", img, hotspot, buf)
	c.cfg.OnCursor(img, hotspot)
	return nil
}

// zrleState is the connection-wide zlib stream ZRLE uses (RFC 6143 §7.7.6).
//
// Each rectangle carries the next chunk of one continuous zlib stream, ending
// at a sync flush. Inflating is about 60% of decoding, so it runs on its own
// goroutine, ahead of the tile decoder: chunks go into src, inflated bytes
// come out of out. src blocks instead of reporting EOF when it runs dry, so
// the inflater never sees the stream end between rectangles; it simply waits
// at the sync point for the next chunk.
type zrleState struct {
	src     *bytePipe // compressed chunks, unbounded (each chunk is bounded)
	out     *bytePipe // inflated bytes, bounded so the inflater cannot run away
	br      *bufio.Reader
	scratch []byte
	palette [128]uint32
}

// start launches the inflater on first use.
func (z *zrleState) start() {
	z.src, z.out = newPipe(0), newPipe(4<<20)
	src, out := z.src, z.out
	go func() {
		// Inflate the raw deflate data after the 2-byte zlib header: the
		// Adler-32 trailer only comes at the end of the connection, and
		// checksumming every byte costs time TCP already spends.
		br := bufio.NewReaderSize(src, 64<<10)
		var hdr [2]byte
		_, err := io.ReadFull(br, hdr[:])
		if err == nil && !isZlibHeader(hdr[:]) {
			err = protoErr("ZRLE stream without zlib header")
		}
		if err == nil {
			_, err = io.Copy(out, flate.NewReader(br))
		}
		if err == nil {
			err = io.ErrUnexpectedEOF // a ZRLE stream never ends cleanly
		}
		out.closeWithError(err)
	}()
	z.br = bufio.NewReaderSize(out, 64<<10)
}

// stop ends the inflater; the state can start again afterwards.
func (z *zrleState) stop() {
	if z.src != nil {
		z.src.closeWithError(io.EOF)
		z.out.closeWithError(io.EOF) // unblock an inflater waiting for room
	}
	z.src, z.out, z.br = nil, nil, nil
}

func (c *Client) decodeZRLE(r image.Rectangle) error {
	var lb [4]byte
	if _, err := io.ReadFull(c.r, lb[:]); err != nil {
		return err
	}
	n := be.Uint32(lb[:])
	if n > 64<<20 {
		return protoErr("ZRLE chunk of %d bytes", n)
	}
	z := &c.zrle
	if z.src == nil {
		z.start()
	}
	// Feed the whole chunk before decoding, so holding the framebuffer lock
	// below waits on the inflater at most, never on the network.
	if _, err := io.CopyN(z.src, c.r, int64(n)); err != nil {
		return err
	}
	// The whole chunk is in memory: decoding is CPU work only, so holding
	// the framebuffer lock never waits on the network.
	c.lockFB()
	defer c.unlockFB()
	pix := c.pix32()
	for ty := r.Min.Y; ty < r.Max.Y; ty += 64 {
		th := min(64, r.Max.Y-ty)
		for tx := r.Min.X; tx < r.Max.X; tx += 64 {
			tw := min(64, r.Max.X-tx)
			t := tile{pix: pix, stride: c.fb.W, x: tx, y: ty, w: tw, h: th}
			if err := z.tile(&t); err != nil {
				return err
			}
		}
	}
	c.wrote(r)
	return nil
}

type tile struct {
	pix        []uint32
	stride     int
	x, y, w, h int
}

// fill paints count pixels starting at tile-local index i, wrapping rows.
func (t *tile) fill(i, count int, v uint32) {
	for count > 0 {
		y, x := i/t.w, i%t.w
		n := min(count, t.w-x)
		off := (t.y+y)*t.stride + t.x + x
		fillRow(t.pix[off:off+n], v)
		i += n
		count -= n
	}
}

// fillRow sets every pixel of row to v. Long rows double a filled prefix
// with copy, which runs as wide memmoves; short runs, the common case in
// RLE tiles, keep the simple loop.
func fillRow(row []uint32, v uint32) {
	if len(row) < 16 {
		for j := range row {
			row[j] = v
		}
		return
	}
	row[0] = v
	for k := 1; k < len(row); k *= 2 {
		copy(row[k:], row[:k])
	}
}

// fillTile paints the whole tile v: one row, then copies of it.
func (t *tile) fillTile(v uint32) {
	first := t.pix[t.y*t.stride+t.x:][:t.w]
	fillRow(first, v)
	for y := 1; y < t.h; y++ {
		copy(t.pix[(t.y+y)*t.stride+t.x:][:t.w], first)
	}
}

func (z *zrleState) cpixel() (uint32, error) {
	// Peek avoids io.ReadFull, whose interface call would move a stack
	// buffer to the heap on every pixel.
	b, err := z.br.Peek(3)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | opaque
	z.br.Discard(3)
	return v, nil
}

func (z *zrleState) runLength() (int, error) {
	n := 1
	for {
		b, err := z.br.ReadByte()
		if err != nil {
			return 0, err
		}
		n += int(b)
		if b != 255 {
			return n, nil
		}
		if n > 64*64 {
			return 0, protoErr("ZRLE run too long")
		}
	}
}

func (z *zrleState) need(n int) []byte {
	if cap(z.scratch) < n {
		z.scratch = make([]byte, n)
	}
	return z.scratch[:n]
}

func (z *zrleState) tile(t *tile) error {
	sub, err := z.br.ReadByte()
	if err != nil {
		return err
	}
	area := t.w * t.h
	switch {
	case sub == 0: // raw CPIXELs
		buf := z.need(area * 3)
		if _, err := io.ReadFull(z.br, buf); err != nil {
			return err
		}
		for y := 0; y < t.h; y++ {
			off := (t.y+y)*t.stride + t.x
			row := t.pix[off : off+t.w]
			src := buf[y*t.w*3:]
			for x := range row {
				row[x] = uint32(src[3*x]) | uint32(src[3*x+1])<<8 | uint32(src[3*x+2])<<16 | opaque
			}
		}

	case sub == 1: // solid
		v, err := z.cpixel()
		if err != nil {
			return err
		}
		t.fillTile(v)

	case sub <= 16: // packed palette
		size := int(sub)
		for i := range size {
			if z.palette[i], err = z.cpixel(); err != nil {
				return err
			}
		}
		bpp := 4
		switch {
		case size == 2:
			bpp = 1
		case size <= 4:
			bpp = 2
		}
		rowBytes := (t.w*bpp + 7) / 8
		buf := z.need(rowBytes * t.h)
		if _, err := io.ReadFull(z.br, buf); err != nil {
			return err
		}
		mask := byte(1<<bpp - 1)
		for y := 0; y < t.h; y++ {
			off := (t.y+y)*t.stride + t.x
			row := t.pix[off : off+t.w]
			src := buf[y*rowBytes:]
			for x := range row {
				bit := x * bpp
				shift := 8 - bpp - bit%8
				row[x] = z.palette[(src[bit/8]>>shift)&mask]
			}
		}

	case sub == 128: // plain RLE
		for i := 0; i < area; {
			v, err := z.cpixel()
			if err != nil {
				return err
			}
			n, err := z.runLength()
			if err != nil {
				return err
			}
			if i+n > area {
				return protoErr("ZRLE run past tile end")
			}
			t.fill(i, n, v)
			i += n
		}

	case sub >= 130: // palette RLE
		size := int(sub) - 128
		for i := range size {
			if z.palette[i], err = z.cpixel(); err != nil {
				return err
			}
		}
		for i := 0; i < area; {
			idx, err := z.br.ReadByte()
			if err != nil {
				return err
			}
			n := 1
			if idx&0x80 != 0 {
				idx &= 0x7F
				if n, err = z.runLength(); err != nil {
					return err
				}
			}
			if int(idx) >= size || i+n > area {
				return protoErr("bad ZRLE palette run")
			}
			t.fill(i, n, z.palette[idx])
			i += n
		}

	default:
		return protoErr("ZRLE subencoding %d", sub)
	}
	return nil
}

// bytePipe is an in-memory byte queue between goroutines. Reads block while it
// is empty; writes block while it holds limit bytes (0: no limit). After
// closeWithError, reads drain what is left and then return the error.
type bytePipe struct {
	mu    sync.Mutex
	cond  sync.Cond
	buf   []byte
	off   int
	limit int
	err   error
}

func newPipe(limit int) *bytePipe {
	p := &bytePipe{limit: limit}
	p.cond.L = &p.mu
	return p
}

func (p *bytePipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	written := 0
	for len(b) > 0 {
		for p.err == nil && p.limit > 0 && len(p.buf)-p.off >= p.limit {
			p.cond.Wait()
		}
		if p.err != nil {
			return written, p.err
		}
		n := len(b)
		if p.limit > 0 {
			n = min(n, p.limit-(len(p.buf)-p.off))
		}
		if p.off > 0 && p.off >= len(p.buf)/2 { // reclaim consumed space
			p.buf = p.buf[:copy(p.buf, p.buf[p.off:])]
			p.off = 0
		}
		p.buf = append(p.buf, b[:n]...)
		b, written = b[n:], written+n
		p.cond.Broadcast()
	}
	return written, nil
}

func (p *bytePipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.off == len(p.buf) && p.err == nil {
		p.cond.Wait()
	}
	if p.off == len(p.buf) {
		return 0, p.err
	}
	n := copy(b, p.buf[p.off:])
	p.off += n
	if p.off == len(p.buf) {
		p.buf, p.off = p.buf[:0], 0
	}
	p.cond.Broadcast()
	return n, nil
}

func (p *bytePipe) closeWithError(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.cond.Broadcast()
	p.mu.Unlock()
}
