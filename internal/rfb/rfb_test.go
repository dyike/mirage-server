package rfb

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"image"
	"io"
	"math"
	"math/big"
	"math/bits"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/klauspost/compress/zlib"
)

// fakeServer drives the server side of one connection from a test script.
type fakeServer struct {
	t    *testing.T
	conn net.Conn
}

func (s *fakeServer) write(b ...[]byte) {
	s.t.Helper()
	for _, p := range b {
		if _, err := s.conn.Write(p); err != nil {
			s.t.Errorf("server write: %v", err)
		}
	}
}

func (s *fakeServer) read(n int) []byte {
	s.t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(s.conn, b); err != nil {
		s.t.Errorf("server read: %v", err)
	}
	return b
}

func u16(v int) []byte    { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// init sends ServerInit for a w×h desktop and consumes ClientInit, the
// client's SetPixelFormat and SetEncodings, checking the pixel format.
func (s *fakeServer) init(w, h int) {
	s.t.Helper()
	s.read(1) // ClientInit
	var si []byte
	si = append(si, u16(w)...)
	si = append(si, u16(h)...)
	pf := make([]byte, 16)
	PixelFormat{BPP: 32, Depth: 24, TrueColor: true, RedMax: 255, GreenMax: 255, BlueMax: 255, RedShift: 16, GreenShift: 8}.marshal(pf)
	si = append(si, pf...)
	si = append(si, u32(4)...)
	si = append(si, "test"...)
	s.write(si)

	spf := s.read(20)
	if spf[0] != 0 || parsePixelFormat(spf[4:]) != rgbaFormat {
		s.t.Errorf("SetPixelFormat = %v", spf)
	}
	se := s.read(4)
	s.read(4 * int(binary.BigEndian.Uint16(se[2:])))
}

// expectRequest consumes one FramebufferUpdateRequest.
func (s *fakeServer) expectRequest(incremental bool) {
	s.t.Helper()
	b := s.read(10)
	if b[0] != 3 || (b[1] != 0) != incremental {
		s.t.Errorf("FramebufferUpdateRequest = %v, want incremental=%v", b, incremental)
	}
}

func pipe(t *testing.T, cfg Config, server func(s *fakeServer)) (*Client, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		server(&fakeServer{t: t, conn: conn})
	}()
	t.Cleanup(func() { <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, ln.Addr().String(), cfg)
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func TestVNCAuth(t *testing.T) {
	challenge := make([]byte, 16)
	rand.Read(challenge)
	c, err := pipe(t, Config{Password: "secret12"}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		if v := s.read(12); string(v) != "RFB 003.008\n" {
			t.Errorf("client version %q", v)
		}
		s.write([]byte{2, SecNone, SecVNC})
		if b := s.read(1); b[0] != SecVNC {
			t.Errorf("chose security %d", b[0])
		}
		s.write(challenge)
		resp := s.read(16)
		// Decrypt with an independently derived key: bits of each byte reversed.
		key := []byte("secret12")
		for i := range key {
			key[i] = bits.Reverse8(key[i])
		}
		blk, _ := des.NewCipher(key)
		got := make([]byte, 16)
		blk.Decrypt(got[:8], resp[:8])
		blk.Decrypt(got[8:], resp[8:])
		if !bytes.Equal(got, challenge) {
			t.Errorf("VNC auth response does not decrypt to the challenge")
		}
		s.write(u32(0))
		s.init(8, 4)
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "test" || c.Width() != 8 || c.Height() != 4 {
		t.Errorf("ServerInit: name %q size %dx%d", c.Name, c.Width(), c.Height())
	}
}

func TestVNCAuthFailure(t *testing.T) {
	_, err := pipe(t, Config{Password: "x"}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecVNC})
		s.read(1)
		s.write(make([]byte, 16))
		s.read(16)
		s.write(u32(1), u32(5), []byte("nope!"))
	})
	ae, ok := err.(*AuthError)
	if !ok || ae.Reason != "nope!" {
		t.Fatalf("err = %v, want AuthError(nope!)", err)
	}
}

// TestARDAuth plays a macOS server: version 003.889 and security type 30.
func TestARDAuth(t *testing.T) {
	const user, pass = "dyike", "p@ss wörd"
	ardKey := make(chan [16]byte, 1)
	c, err := pipe(t, Config{Username: user, Password: pass}, func(s *fakeServer) {
		s.write([]byte("RFB 003.889\n"))
		if v := s.read(12); string(v) != "RFB 003.008\n" {
			t.Errorf("client version %q", v)
		}
		s.write([]byte{4, 30, 33, 36, 35})
		if b := s.read(1); b[0] != SecARD {
			t.Errorf("chose security %d", b[0])
		}
		// 128-byte group (small for test speed; macOS uses a 1024-bit prime).
		prime, _ := rand.Prime(rand.Reader, 1024)
		keyLen := 128
		priv, _ := rand.Int(rand.Reader, prime)
		pub := new(big.Int).Exp(big.NewInt(2), priv, prime)
		s.write(u16(2), u16(keyLen), prime.FillBytes(make([]byte, keyLen)), pub.FillBytes(make([]byte, keyLen)))

		msg := s.read(128 + keyLen)
		clientPub := new(big.Int).SetBytes(msg[128:])
		shared := new(big.Int).Exp(clientPub, priv, prime)
		key := md5.Sum(shared.FillBytes(make([]byte, keyLen)))
		ardKey <- key
		blk, _ := aes.NewCipher(key[:])
		plain := make([]byte, 128)
		for i := 0; i < 128; i += 16 {
			blk.Decrypt(plain[i:i+16], msg[i:i+16])
		}
		cstr := func(b []byte) string { return string(b[:bytes.IndexByte(b, 0)]) }
		if u, p := cstr(plain[:64]), cstr(plain[64:]); u != user || p != pass {
			t.Errorf("ARD credentials = %q/%q", u, p)
		}
		s.write(u32(0))
		s.init(4, 4)
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "003.008" {
		t.Errorf("Version = %s", c.Version)
	}
	// The Apple dialect wraps its record keys in this key.
	if k := <-ardKey; c.wrapKey != k {
		t.Errorf("wrap key %x, want %x", c.wrapKey, k)
	}
}

func TestARDNeedsUsername(t *testing.T) {
	_, err := pipe(t, Config{Password: "x"}, func(s *fakeServer) {
		s.write([]byte("RFB 003.889\n"))
		s.read(12)
		s.write([]byte{2, 30, 33})
	})
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("err = %v, want AuthError", err)
	}
}

func TestVersion33None(t *testing.T) {
	c, err := pipe(t, Config{}, func(s *fakeServer) {
		s.write([]byte("RFB 003.003\n"))
		s.read(12)
		s.write(u32(uint32(SecNone))) // 3.3: no SecurityResult after None
		s.init(2, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "003.003" {
		t.Errorf("Version = %s", c.Version)
	}
}

// --- framebuffer updates ---

// testImage is a deterministic picture with flat areas, gradients and noise,
// so every ZRLE subencoding gets exercised.
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	for y := range h {
		for x := range w {
			var r, g, b byte
			switch {
			case x < 64: // solid
				r, g, b = 10, 20, 30
			case x < 128: // two colours: packed 1-bit palette
				if (x+y)%3 == 0 {
					r = 255
				}
			case x < 192: // eight colours: packed 4-bit palette / palette RLE
				r, g, b = byte(x/4%8)*30, byte(y/8%8)*30, 7
			default: // noise: raw or plain RLE
				seed = seed*1664525 + 1013904223
				r, g, b = byte(seed>>24), byte(seed>>16), byte(seed>>8)
			}
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 255
		}
	}
	return img
}

// encodeZRLE is a reference ZRLE encoder: it picks the subencoding by tile
// content and cycles through the alternatives with mode so that each one
// appears in the stream.
func encodeZRLE(img *image.RGBA, r image.Rectangle, mode int) []byte {
	var out bytes.Buffer
	px := func(x, y int) [3]byte {
		i := img.PixOffset(x, y)
		return [3]byte{img.Pix[i], img.Pix[i+1], img.Pix[i+2]}
	}
	for ty := r.Min.Y; ty < r.Max.Y; ty += 64 {
		for tx := r.Min.X; tx < r.Max.X; tx += 64 {
			tw, th := min(64, r.Max.X-tx), min(64, r.Max.Y-ty)
			var pixels [][3]byte
			index := map[[3]byte]int{}
			var palette [][3]byte
			for y := range th {
				for x := range tw {
					p := px(tx+x, ty+y)
					pixels = append(pixels, p)
					if _, ok := index[p]; !ok {
						index[p] = len(palette)
						palette = append(palette, p)
					}
				}
			}
			mode++
			switch {
			case len(palette) == 1:
				out.WriteByte(1)
				out.Write(palette[0][:])
			case len(palette) <= 16 && mode%2 == 0: // packed palette
				out.WriteByte(byte(len(palette)))
				for _, p := range palette {
					out.Write(p[:])
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
						nb += bpp
						if nb == 8 {
							out.WriteByte(cur)
							cur, nb = 0, 0
						}
					}
					if nb > 0 {
						out.WriteByte(cur << (8 - nb))
					}
				}
			case len(palette) <= 127 && mode%2 == 1: // palette RLE
				out.WriteByte(byte(128 + len(palette)))
				for _, p := range palette {
					out.Write(p[:])
				}
				for i := 0; i < len(pixels); {
					n := 1
					for i+n < len(pixels) && pixels[i+n] == pixels[i] {
						n++
					}
					if n == 1 {
						out.WriteByte(byte(index[pixels[i]]))
					} else {
						out.WriteByte(byte(index[pixels[i]]) | 0x80)
						writeRun(&out, n)
					}
					i += n
				}
			case mode%2 == 0: // raw
				out.WriteByte(0)
				for _, p := range pixels {
					out.Write(p[:])
				}
			default: // plain RLE
				out.WriteByte(128)
				for i := 0; i < len(pixels); {
					n := 1
					for i+n < len(pixels) && pixels[i+n] == pixels[i] {
						n++
					}
					out.Write(pixels[i][:])
					writeRun(&out, n)
					i += n
				}
			}
		}
	}
	return out.Bytes()
}

func writeRun(out *bytes.Buffer, n int) {
	n--
	for n >= 255 {
		out.WriteByte(255)
		n -= 255
	}
	out.WriteByte(byte(n))
}

func rectHeader(r image.Rectangle, enc int32) []byte {
	var b []byte
	b = append(b, u16(r.Min.X)...)
	b = append(b, u16(r.Min.Y)...)
	b = append(b, u16(r.Dx())...)
	b = append(b, u16(r.Dy())...)
	return append(b, u32(uint32(enc))...)
}

func rawBytes(img *image.RGBA, r image.Rectangle) []byte {
	var b []byte
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := img.PixOffset(x, y)
			b = append(b, img.Pix[i], img.Pix[i+1], img.Pix[i+2], 0) // zero padding byte
		}
	}
	return b
}

type update struct {
	pix     []byte
	w, h    int
	dirty   []image.Rectangle
	written []image.Rectangle // OnPixels since the previous update
}

// heldLock is a framebuffer lock that reports whether it is held.
type heldLock struct {
	sync.Mutex
	held bool
}

func (l *heldLock) Lock()   { l.Mutex.Lock(); l.held = true }
func (l *heldLock) Unlock() { l.held = false; l.Mutex.Unlock() }

// covered reports whether every pixel of r lies in one of rs.
func covered(r image.Rectangle, rs []image.Rectangle) bool {
	for y := r.Min.Y; y < r.Max.Y; y++ {
	next:
		for x := r.Min.X; x < r.Max.X; x++ {
			for _, w := range rs {
				if image.Pt(x, y).In(w) {
					continue next
				}
			}
			return false
		}
	}
	return true
}

func runClient(t *testing.T, c *Client, updates chan update) {
	go func() {
		if err := c.Run(); err != nil && err != io.EOF {
			t.Errorf("Run: %v", err)
		}
		close(updates)
	}()
}

func comparePix(t *testing.T, got []byte, want *image.RGBA, r image.Rectangle) {
	t.Helper()
	stride := want.Bounds().Dx() * 4
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := y*stride + x*4
			if !bytes.Equal(got[i:i+4], want.Pix[i:i+4]) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got[i:i+4], want.Pix[i:i+4])
			}
		}
	}
}

func TestUpdates(t *testing.T) {
	const w, h = 300, 150
	img := testImage(w, h)
	updates := make(chan update, 8)
	lock := new(heldLock)
	var written []image.Rectangle
	cfg := Config{InFlight: 2, FramebufferLock: lock, OnPixels: func(fb *frame.Framebuffer, r image.Rectangle) {
		if !lock.held {
			t.Errorf("OnPixels(%v) without the framebuffer lock", r)
		}
		written = append(written, r)
	}, OnUpdate: func(fb *frame.Framebuffer, dirty []image.Rectangle) {
		updates <- update{pix: bytes.Clone(fb.Pix), w: fb.W, h: fb.H, dirty: append([]image.Rectangle(nil), dirty...), written: written}
		written = nil
	}}
	serverDone := make(chan struct{})
	c, err := pipe(t, cfg, func(s *fakeServer) {
		defer close(serverDone)
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(w, h)

		// 1: whole screen, top half Raw, bottom half in two ZRLE rects
		// that share one zlib stream.
		s.expectRequest(false)
		s.expectRequest(true) // the second request kept in flight
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		zchunk := func(r image.Rectangle, mode int) []byte {
			zbuf.Reset()
			zw.Write(encodeZRLE(img, r, mode))
			zw.Flush()
			return append(u32(uint32(zbuf.Len())), zbuf.Bytes()...)
		}
		top := image.Rect(0, 0, w, 70)
		bl, br := image.Rect(0, 70, 150, h), image.Rect(150, 70, w, h)
		s.write([]byte{0, 0}, u16(3),
			rectHeader(top, EncRaw), rawBytes(img, top),
			rectHeader(bl, EncZRLE), zchunk(bl, 0),
			rectHeader(br, EncZRLE), zchunk(br, 1))

		// 2: CopyRect, overlapping downward move, then a third ZRLE chunk
		// of the same stream, framed with LastRect.
		s.expectRequest(true)
		dst := image.Rect(10, 20, 110, 90)
		cr := append(rectHeader(dst, EncCopyRect), u16(5)...)
		cr = append(cr, u16(5)...)
		zr := image.Rect(200, 0, 300, 64)
		s.write([]byte{0, 0}, u16(0xFFFF), cr,
			rectHeader(zr, EncZRLE), zchunk(zr, 2),
			rectHeader(image.Rectangle{}, EncPseudoLastRect))

		// 3: DesktopSize shrinks the screen.
		s.expectRequest(true)
		s.write([]byte{0, 0}, u16(1), rectHeader(image.Rect(0, 0, 40, 30), EncPseudoDesktopSize))
		s.expectRequest(false) // full refresh after resize
		s.expectRequest(true)
		// Answer it: the client stops re-requesting the full screen.
		small := image.Rect(0, 0, 40, 30)
		s.write([]byte{0, 0}, u16(1), rectHeader(small, EncRaw), rawBytes(testImage(40, 30), small))
		s.expectRequest(true)

		// Client messages.
		if b := s.read(8); !bytes.Equal(b, []byte{4, 1, 0, 0, 0, 0, 0xff, 0x0d}) {
			t.Errorf("KeyEvent = %v", b)
		}
		if b := s.read(6); !bytes.Equal(b, []byte{5, 5, 0, 7, 1, 2}) {
			t.Errorf("PointerEvent = %v", b)
		}
		if b := s.read(11); !bytes.Equal(b, []byte{6, 0, 0, 0, 0, 0, 0, 3, 'h', 0xe9, '?'}) {
			t.Errorf("ClientCutText = %v", b)
		}
		s.write([]byte{3, 0, 0, 0}, u32(2), []byte{'o', 0xe9}, []byte{2})
	})
	if err != nil {
		t.Fatal(err)
	}
	cut := make(chan string, 1)
	bell := make(chan struct{}, 1)
	c.cfg.OnCutText = func(s string) { cut <- s }
	c.cfg.OnBell = func() { bell <- struct{}{} }
	runClient(t, c, updates)

	checkWritten := func(u update) {
		t.Helper()
		for _, d := range u.dirty {
			if !covered(d, u.written) {
				t.Errorf("dirty %v not reported to OnPixels %v", d, u.written)
			}
		}
	}
	u := <-updates
	comparePix(t, u.pix, img, img.Bounds())
	if len(u.dirty) != 3 {
		t.Errorf("dirty = %v", u.dirty)
	}
	checkWritten(u)

	// Apply the same CopyRect and ZRLE to the reference image.
	want := image.NewRGBA(img.Bounds())
	copy(want.Pix, img.Pix)
	src := image.NewRGBA(img.Bounds())
	copy(src.Pix, img.Pix)
	for y := range 70 {
		for x := range 100 {
			copy(want.Pix[want.PixOffset(10+x, 20+y):][:4], src.Pix[src.PixOffset(5+x, 5+y):][:4])
		}
	}
	u = <-updates
	comparePix(t, u.pix, want, want.Bounds())
	if len(u.dirty) != 2 {
		t.Errorf("dirty = %v", u.dirty)
	}
	checkWritten(u)

	u = <-updates
	if u.w != 40 || u.h != 30 || len(u.dirty) != 1 || u.dirty[0] != image.Rect(0, 0, 40, 30) {
		t.Errorf("after resize: %dx%d dirty %v", u.w, u.h, u.dirty)
	}
	checkWritten(u) // the resize clears every pixel

	<-updates // the full screen at the new size
	c.holes.mu.Lock()
	holes := len(c.holes.bands)
	c.holes.mu.Unlock()
	if holes != 0 {
		t.Errorf("%d unpainted bands after a full update", holes)
	}

	c.KeyEvent(XKReturn, true)
	c.PointerEvent(ButtonLeft|ButtonRight, 7, 258)
	c.CutText("hé€")
	if s := <-cut; s != "oé" {
		t.Errorf("cut text %q", s)
	}
	<-bell
	<-serverDone
}

func BenchmarkZRLE(b *testing.B) {
	const w, h = 2560, 1600
	img := testImage(w, h)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(encodeZRLE(img, img.Bounds(), 0))
	zw.Flush()
	frame := append(u32(uint32(zbuf.Len())), zbuf.Bytes()...)
	// Each iteration needs a fresh stream: replay the first chunk into a
	// new client.
	b.SetBytes(int64(w * h * 4))
	b.ReportAllocs()
	c := &Client{}
	c.fb.Resize(w, h)
	for b.Loop() {
		c.zrle.stop()
		c.r = bufio.NewReader(bytes.NewReader(frame))
		if err := c.decodeZRLE(img.Bounds()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCursor(t *testing.T) {
	type cursor struct {
		img     *image.RGBA
		hotspot image.Point
	}
	got := make(chan cursor, 1)
	cfg := Config{OnCursor: func(img *image.RGBA, hot image.Point) { got <- cursor{img, hot} }}
	c, err := pipe(t, cfg, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(4, 4)
		s.expectRequest(false)
		// 10×2 cursor, hotspot (1, 1): the mask row spans two bytes.
		var pix []byte
		for i := range 20 {
			pix = append(pix, byte(i), 100, 200, 0)
		}
		mask := []byte{0b10000000, 0b01000000, 0b01111111, 0b11000000}
		s.write([]byte{0, 0}, u16(1), rectHeader(image.Rect(1, 1, 11, 3), EncPseudoCursor), pix, mask)
	})
	if err != nil {
		t.Fatal(err)
	}
	go c.Run()
	cur := <-got
	if cur.hotspot != image.Pt(1, 1) || cur.img.Bounds() != image.Rect(0, 0, 10, 2) {
		t.Fatalf("cursor %v hotspot %v", cur.img.Bounds(), cur.hotspot)
	}
	// Row 0: bits for x=0 and x=9. Row 1: x=1..7 in the first byte, 8..9 in the second.
	opaque := map[image.Point]bool{{0, 0}: true, {9, 0}: true}
	for y := range 2 {
		for x := range 10 {
			px := cur.img.Pix[cur.img.PixOffset(x, y):][:4]
			want := []byte{0, 0, 0, 0}
			if opaque[image.Pt(x, y)] || (y == 1 && x >= 1) {
				want = []byte{byte(y*10 + x), 100, 200, 255}
			}
			if !bytes.Equal(px, want) {
				t.Errorf("pixel (%d,%d) = %v, want %v", x, y, px, want)
			}
		}
	}
}

// TestContinuousUpdates: a server announcing ContinuousUpdates (message 150)
// gets EnableContinuousUpdates, after which the client stops requesting.
func TestContinuousUpdates(t *testing.T) {
	updates := make(chan struct{}, 4)
	cfg := Config{InFlight: 2, OnUpdate: func(*frame.Framebuffer, []image.Rectangle) { updates <- struct{}{} }}
	done := make(chan struct{})
	c, err := pipe(t, cfg, func(s *fakeServer) {
		defer close(done)
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(8, 4)
		s.expectRequest(false)
		s.expectRequest(true)
		s.write([]byte{150}) // supported
		if b := s.read(10); !bytes.Equal(b, []byte{150, 1, 0, 0, 0, 0, 0, 8, 0, 4}) {
			t.Errorf("EnableContinuousUpdates = %v", b)
		}
		// Pushed updates: the client must not answer with requests.
		r := image.Rect(0, 0, 1, 1)
		for range 2 {
			s.write([]byte{0, 0}, u16(1), rectHeader(r, EncRaw), []byte{1, 2, 3, 0})
		}
		s.write([]byte{150}) // the server stopped pushing
		s.expectRequest(true)
	})
	if err != nil {
		t.Fatal(err)
	}
	go c.Run()
	<-updates
	<-updates
	<-done
}

// encode555 packs img's rectangle r as big-endian 0RRRRRGGGGGBBBBB.
func encode555(img *image.RGBA, r image.Rectangle) []byte {
	var b []byte
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := img.PixOffset(x, y)
			v := uint16(img.Pix[i]>>3)<<10 | uint16(img.Pix[i+1]>>3)<<5 | uint16(img.Pix[i+2]>>3)
			b = append(b, byte(v>>8), byte(v))
		}
	}
	return b
}

// want555 is what a 555 round trip makes of img.
func want555(img *image.RGBA) *image.RGBA {
	out := image.NewRGBA(img.Rect)
	for i := 0; i < len(img.Pix); i += 4 {
		for c := range 3 {
			v := img.Pix[i+c] >> 3
			out.Pix[i+c] = v<<3 | v>>2
		}
		out.Pix[i+3] = 255
	}
	return out
}

func TestAppleRGB555(t *testing.T) {
	const w, h = 300, 150
	img := testImage(w, h)
	img.Pix[0], img.Pix[1], img.Pix[2] = 255, 255, 255 // white stays white
	img.Pix[4], img.Pix[5], img.Pix[6] = 0, 0, 0
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "stream per rect", true: "shared stream"}[shared], func(t *testing.T) {
			updates := make(chan []byte, 2)
			cfg := Config{Encodings: []int32{EncAppleRGB555, EncRaw},
				OnUpdate: func(fb *frame.Framebuffer, _ []image.Rectangle) { updates <- bytes.Clone(fb.Pix) }}
			c, err := pipe(t, cfg, func(s *fakeServer) {
				s.write([]byte("RFB 003.008\n"))
				s.read(12)
				s.write([]byte{1, SecNone})
				s.read(1)
				s.write(u32(0))
				s.init(w, h)
				s.expectRequest(false)
				s.expectRequest(true)
				var zbuf bytes.Buffer
				zw, _ := zlib.NewWriterLevel(&zbuf, 1)
				payload := func(r image.Rectangle) []byte {
					if !shared {
						zbuf.Reset()
						zw.Reset(&zbuf)
						zw.Write(encode555(img, r))
						zw.Close()
					} else {
						zbuf.Reset()
						zw.Write(encode555(img, r))
						zw.Flush()
					}
					return append(u32(uint32(zbuf.Len())), zbuf.Bytes()...)
				}
				top, bottom := image.Rect(0, 0, w, 70), image.Rect(0, 70, w, h)
				s.write([]byte{0, 0}, u16(2),
					rectHeader(top, EncAppleRGB555), payload(top),
					rectHeader(bottom, EncAppleRGB555), payload(bottom))
			})
			if err != nil {
				t.Fatal(err)
			}
			go c.Run()
			comparePix(t, <-updates, want555(img), img.Bounds())
		})
	}
}

// TestFullUpdateRetry: after a resize, a server that answers the full
// request with only a small change gets asked again for the unpainted
// area, like macOS after ScaleFactor.
func TestFullUpdateRetry(t *testing.T) {
	fulls := make(chan struct{}, 8)
	c, err := pipe(t, Config{InFlight: 2}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(8, 4)
		s.expectRequest(false)
		s.expectRequest(true)
		s.write([]byte{0, 0}, u16(1), rectHeader(image.Rect(0, 0, 4, 2), EncPseudoDesktopSize))
		// Ignore the full request; send only a corner, then wait for asks.
		small := image.Rect(0, 0, 1, 1)
		s.write([]byte{0, 0}, u16(1), rectHeader(small, EncRaw), []byte{1, 2, 3, 0})
		for n := 0; n < 2; {
			if b := s.read(10); b[0] == 3 && b[1] == 0 {
				fulls <- struct{}{}
				n++
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	go c.Run()
	deadline := time.After(3 * time.Second)
	for got := 0; got < 2; got++ { // the resize's own request and a retry
		select {
		case <-fulls:
		case <-deadline:
			t.Fatal("no full-screen request repeated after the resize")
		}
	}
}

// TestCoverageBands reproduces macOS's adaptive full-screen answer, which
// leaves out the menu bar and Dock bands.
func TestCoverageBands(t *testing.T) {
	var cv coverage
	cv.reset(3840, 2160)
	cv.mark(image.Rect(0, 60, 3840, 2058))
	got := cv.missing()
	want := []image.Rectangle{image.Rect(0, 0, 3840, 64), image.Rect(0, 2048, 3840, 2160)}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	// The menu bar arrives in pieces; one icon-sized hole remains.
	cv.mark(image.Rect(0, 0, 2000, 64))
	cv.mark(image.Rect(2100, 0, 3840, 64))
	cv.mark(image.Rect(0, 2048, 3840, 2160))
	if got := cv.missing(); len(got) != 1 || got[0] != image.Rect(2000, 0, 2112, 64) {
		t.Fatalf("missing = %v, want the hole at x 2000..2112", got)
	}
	cv.mark(image.Rect(1990, 0, 2120, 70))
	if got := cv.missing(); got != nil {
		t.Fatalf("missing = %v after painting everything", got)
	}
	// Edges that are not multiples of the cell size count once reached.
	cv.reset(100, 50)
	cv.mark(image.Rect(0, 0, 100, 50))
	if got := cv.missing(); got != nil {
		t.Fatalf("odd-sized full paint left %v", got)
	}
}

// TestPointerScaling: after ScaleFactor, macOS sends a half-size
// framebuffer but wants pointer positions in native pixels.
func TestPointerScaling(t *testing.T) {
	resized := make(chan struct{}, 1)
	cfg := Config{InFlight: 2, AppleScale: 0.5, OnResize: func(w, h int) {
		if w == 4 {
			resized <- struct{}{}
		}
	}}
	got := make(chan []byte, 4)
	c, err := pipe(t, cfg, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(8, 4)
		if b := s.read(10); b[0] != 0x08 || math.Float64frombits(binary.BigEndian.Uint64(b[2:])) != 0.5 {
			t.Errorf("ScaleFactor = %v", b)
		}
		s.expectRequest(false)
		s.expectRequest(true)
		s.write([]byte{0, 0}, u16(1), rectHeader(image.Rect(0, 0, 4, 2), EncPseudoDesktopSize))
		for n := 0; n < 2; {
			switch b := s.read(1); b[0] {
			case 3: // update requests, including retries for unpainted bands
				s.read(9)
			case 5:
				got <- append(b, s.read(5)...)
				n++
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	go c.Run()
	<-resized
	c.PointerEvent(ButtonLeft, 3, 1) // last column, second row of the 4×2 framebuffer
	c.PointerEvent(0, 0, 0)
	for _, want := range [][]byte{{5, 1, 0, 7, 0, 3}, {5, 0, 0, 1, 0, 1}} {
		if b := <-got; !bytes.Equal(b, want) {
			t.Errorf("PointerEvent = %v, want %v", b, want)
		}
	}
}
