package rfb

import (
	"bytes"
	"crypto/aes"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"image"
	"io"
	"math"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

func TestRecordLayer(t *testing.T) {
	var k recordKeys
	rand.Read(k.key[:])
	rand.Read(k.iv[:])
	s := newRecordSealer(k)
	var wire bytes.Buffer
	msgs := [][]byte{[]byte("hello"), bytes.Repeat([]byte{7}, recMaxBody+100), {}, []byte("x")}
	for _, m := range msgs {
		wire.Write(s.seal(m))
	}
	r := newRecordReader(&wire, k)
	for i, m := range msgs {
		got := make([]byte, len(m))
		if _, err := io.ReadFull(r, got); err != nil || !bytes.Equal(got, m) {
			t.Fatalf("message %d: %v", i, err)
		}
	}

	// A flipped bit fails the checksum.
	b := newRecordSealer(k).seal([]byte("tamper"))
	b[5] ^= 1
	if _, err := newRecordReader(bytes.NewReader(b), k).Read(make([]byte, 6)); err == nil {
		t.Fatal("tampered record accepted")
	}
}

// RFC 3526 group 14 (2048-bit MODP), generator 2.
var testN, _ = hex.DecodeString(strings.Join(strings.Fields(`
FFFFFFFF FFFFFFFF C90FDAA2 2168C234 C4C6628B 80DC1CD1 29024E08 8A67CC74
020BBEA6 3B139B22 514A0879 8E3404DD EF9519B3 CD3A431B 302B0A6D F25F1437
4FE1356D 6D51C245 E485B576 625E7EC6 F44C42E9 A637ED6B 0BFF5CB6 F406B7ED
EE386BFB 5A899FA5 AE9F2411 7C4B1FE6 49286651 ECE45B3D C2007CB8 A163BF05
98DA4836 1C55D39A 69163FA8 FD24CF5F 83655D23 DCA3AD96 1C62F356 208552BB
9ED52907 7096966D 670C354E 4ABC9804 F1746C08 CA18217C 32905E46 2E36CE3B
E39E772C 180E8603 9B2783A2 EC07A28F B5C55DF0 6F4C52C9 DE2BCBF6 95581718
3995497C EA956AE5 15D22618 98FA0510 15728E5A 8AACAA68 FFFFFFFF FFFFFFFF`), ""))

// srpServer plays screensharingd's side of type 36 for password pass and
// returns the wrap key it derives, or nil after rejecting the proof.
func srpServer(s *fakeServer, user, pass string) []byte {
	t := s.t
	idLen := binary.BigEndian.Uint32(s.read(4))
	id := s.read(int(idLen))[4:]
	if n := int(binary.BigEndian.Uint16(id[2:])); string(id[4:4+n]) != user {
		t.Errorf("SRP identity %q", id[4:4+n])
	}
	H := func(parts ...[]byte) []byte {
		h := sha512.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}
	N := new(big.Int).SetBytes(testN)
	g := big.NewInt(2)
	pad := func(v *big.Int) []byte { return v.FillBytes(make([]byte, len(testN))) }
	salt := []byte("0123456789abcdef")
	const iter = 1000
	P, _ := pbkdf2.Key(sha512.New, pass, salt, iter, 128)
	x := new(big.Int).SetBytes(H(salt, H([]byte(":"), P)))
	v := new(big.Int).Exp(g, x, N)
	k := new(big.Int).SetBytes(H(testN, pad(g)))
	b, _ := rand.Int(rand.Reader, N)
	B := new(big.Int).Exp(g, b, N)
	B.Add(B, new(big.Int).Mul(k, v)).Mod(B, N)

	var ch []byte
	ch = append(ch, 0)
	ch = append(ch, u16(len(testN))...)
	ch = append(ch, testN...)
	ch = append(ch, u16(1)...)
	ch = append(ch, 2)
	ch = append(ch, byte(len(salt)))
	ch = append(ch, salt...)
	ch = append(ch, u16(len(testN))...)
	ch = append(ch, pad(B)...)
	ch = binary.BigEndian.AppendUint64(ch, iter)
	opts := []byte("mda=SHA-512")
	ch = append(ch, u16(len(opts))...)
	ch = append(ch, opts...)
	s.write(u32(uint32(4+len(ch))), u32(uint32(len(ch))), ch)

	size := binary.BigEndian.Uint32(s.read(4))
	f := fields{b: s.read(int(size))[4:]}
	A := f.take(int(f.u16()))
	M1 := f.take(int(f.u8()))
	if got := f.take(int(f.u16())); !bytes.Equal(got, opts) {
		t.Errorf("SRP options echoed as %q", got)
	}
	u := new(big.Int).SetBytes(H(A, pad(B)))
	S := new(big.Int).Exp(v, u, N)
	S.Mul(S, new(big.Int).SetBytes(A)).Mod(S, N).Exp(S, b, N)
	K := H(pad(S))
	hn, hg := H(testN), H(pad(g))
	for i := range hn {
		hn[i] ^= hg[i]
	}
	if !bytes.Equal(M1, H(hn, H(nil), salt, A, pad(B), K)) {
		s.write(u32(1)) // SecurityResult in place of the proof
		return nil
	}
	M2 := H(A, M1, K)
	reply := append([]byte{byte(len(M2))}, M2...)
	s.write(u32(uint32(4+len(reply))), u32(uint32(len(reply))), reply)
	sum := sha256.Sum256(K)
	return sum[:16]
}

func TestSRPAuthFailure(t *testing.T) {
	_, err := pipe(t, Config{Apple: true, Username: "u", Password: "wrong"}, func(s *fakeServer) {
		s.write([]byte("RFB 003.889\n"))
		s.read(12)
		s.write([]byte{1, SecAppleSRP})
		s.read(1)
		if key := srpServer(s, "u", "right"); key != nil {
			t.Error("server accepted the wrong password")
		}
	})
	if _, ok := err.(*AuthError); !ok {
		t.Fatalf("err = %v, want AuthError", err)
	}
}

// appleServerInit sends a ServerInit with Apple's extension header,
// accepting client messages 9 and 0x10.
func appleServerInit(s *fakeServer, w, h int) {
	s.t.Helper()
	if b := s.read(1); b[0] != 0xc1 {
		s.t.Errorf("ClientInit = %#x", b[0])
	}
	var cmds [16]byte
	for _, m := range []uint8{msgAppleAutoUpdate, msgApplePointer, msgAppleSetDisplay} {
		cmds[m/8] |= 0x80 >> (m % 8)
	}
	name := append([]byte{0, 0}, u32(0)...)
	name = append(name, cmds[:]...)
	name = append(name, "Mac"...)
	var si []byte
	si = append(si, u16(w)...)
	si = append(si, u16(h)...)
	si = append(si, make([]byte, 16)...)
	si = append(si, u32(uint32(len(name)))...)
	s.write(si, name)
}

func displayLayoutPayload(desktop, fb image.Point, scale float64) []byte {
	p := make([]byte, 0x14+0x38)
	binary.BigEndian.PutUint16(p[2:], uint16(desktop.X))
	binary.BigEndian.PutUint16(p[4:], uint16(desktop.Y))
	binary.BigEndian.PutUint16(p[6:], uint16(fb.X))
	binary.BigEndian.PutUint16(p[8:], uint16(fb.Y))
	binary.BigEndian.PutUint16(p[0x12:], 1)
	r := p[0x14:]
	binary.BigEndian.PutUint64(r[0x00:], math.Float64bits(2))
	binary.BigEndian.PutUint64(r[0x08:], math.Float64bits(scale))
	binary.BigEndian.PutUint32(r[0x10:], 7)
	binary.BigEndian.PutUint16(r[0x18:], uint16(desktop.Y)) // backing bottom
	binary.BigEndian.PutUint16(r[0x1a:], uint16(desktop.X)) // backing right
	binary.BigEndian.PutUint32(r[0x24:], 1)
	return append(u16(len(p)), p...)
}

// TestAppleSession runs the Apple dialect end to end: SRP, the extended
// ServerInit, the switch to records, DisplayLayout with server scaling,
// pushed updates and the encrypted pointer event.
func TestAppleSession(t *testing.T) {
	const user, pass = "dyike", "p@ss wörd"
	img := testImage(4, 2)
	updates := make(chan update, 4)
	var layouts []DisplayLayout
	cfg := Config{Apple: true, Username: user, Password: pass,
		OnDisplays: func(l DisplayLayout) { layouts = append(layouts, l) },
		OnUpdate: func(fb *frame.Framebuffer, dirty []image.Rectangle) {
			updates <- update{pix: bytes.Clone(fb.Pix), w: fb.W, h: fb.H, dirty: append([]image.Rectangle(nil), dirty...)}
		}}
	pointer := make(chan []byte, 2)
	c, err := pipe(t, cfg, func(s *fakeServer) {
		s.write([]byte("RFB 003.889\n"))
		if v := s.read(12); string(v) != "RFB 003.889\n" {
			t.Errorf("client version %q", v)
		}
		s.write([]byte{2, SecAppleRSA, SecAppleSRP})
		if b := s.read(1); b[0] != SecAppleSRP {
			t.Errorf("chose security %d", b[0])
		}
		wrap := srpServer(s, user, pass)
		s.write(u32(0))
		appleServerInit(s, 8, 4)
		prelude := s.read(66 + 4 + 12 + 8)
		if prelude[0] != msgAppleViewerInfo || prelude[66] != msgAppleSetMode || prelude[70] != msgAppleEncryption {
			t.Errorf("prelude % x", prelude)
		}

		// Rekey in the clear, then everything sealed.
		var k recordKeys
		rand.Read(k.key[:])
		rand.Read(k.iv[:])
		blk, _ := aes.NewCipher(wrap)
		body := make([]byte, 36)
		blk.Encrypt(body[4:20], k.key[:])
		blk.Encrypt(body[20:36], k.iv[:])
		s.write([]byte{0, 0}, u16(2), rectHeader(image.Rect(0, 0, 0, 0), EncAppleRekey), body)
		seal := newRecordSealer(k)
		in := newRecordReader(s.conn, k)
		s.write(seal.seal(append(rectHeader(image.Rect(0, 0, 0, 0), EncAppleDisplayLayout),
			displayLayoutPayload(image.Pt(8, 4), image.Pt(4, 2), 0.5)...)))

		read := func(n int) []byte {
			b := make([]byte, n)
			if _, err := io.ReadFull(in, b); err != nil {
				t.Errorf("read records: %v", err)
			}
			return b
		}
		if b := read(20); b[0] != 0 {
			t.Errorf("SetPixelFormat = % x", b)
		}
		se := read(4)
		encs := read(4 * int(binary.BigEndian.Uint16(se[2:])))
		if !bytes.Contains(encs, u32(uint32(EncAppleDisplayLayout))) {
			t.Errorf("encodings % x lack DisplayLayout", encs)
		}
		if b := read(10); b[0] != 3 || b[1] != 0 {
			t.Errorf("first request % x", b)
		}
		if b := read(16); b[0] != msgAppleAutoUpdate || binary.BigEndian.Uint32(b[4:]) != 16666 ||
			binary.BigEndian.Uint16(b[12:]) != 4 || binary.BigEndian.Uint16(b[14:]) != 2 {
			t.Errorf("auto update % x", b)
		}
		s.write(seal.seal(append(append([]byte{0, 0}, u16(1)...),
			append(rectHeader(img.Rect, EncRaw), rawBytes(img, img.Rect)...)...)))
		for range 2 {
			t := read(1)
			switch t[0] {
			case msgApplePointer:
				pointer <- append(t, read(17)...)
			case 5:
				pointer <- append(t, read(5)...)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	go runClient(t, c, updates)
	u := <-updates
	if c.Width() != 4 || c.Height() != 2 || len(u.dirty) == 0 {
		t.Fatalf("after layout: %dx%d dirty %v", c.Width(), c.Height(), u.dirty)
	}
	u = <-updates // the first one only repaints after the resize
	comparePix(t, u.pix, img, img.Rect)
	if len(layouts) != 1 || layouts[0].Displays[0].Scale != 0.5 || layouts[0].Displays[0].ID != 7 {
		t.Errorf("layouts %+v", layouts)
	}

	c.PointerEvent(ButtonLeft, 3, 1) // button change: encrypted, in desktop pixels
	c.PointerEvent(ButtonLeft, 0, 0) // drag: same encrypted path as the press
	m := <-pointer
	if m[0] != msgApplePointer {
		t.Fatalf("first pointer message % x", m)
	}
	blk, _ := aes.NewCipher(c.wrapKey[:])
	blk.Decrypt(m[2:], m[2:])
	if m[12] != 0xff || m[13] != ButtonLeft || binary.BigEndian.Uint16(m[14:]) != 7 || binary.BigEndian.Uint16(m[16:]) != 3 {
		t.Errorf("encrypted pointer block % x", m[2:])
	}
	m = <-pointer
	if len(m) != 18 || m[0] != msgApplePointer {
		t.Fatalf("drag must use encrypted pointer message, got % x", m)
	}
	blk.Decrypt(m[2:], m[2:])
	if m[12] != 0xff || m[13] != ButtonLeft || binary.BigEndian.Uint16(m[14:]) != 1 || binary.BigEndian.Uint16(m[16:]) != 1 {
		t.Errorf("encrypted drag block % x", m[2:])
	}
}

// TestAppleModifierCodes: Option must not reach the Mac as Command. Alt is
// sent as Meta in the Apple dialect, and Meta is Option there.
func TestAppleModifierCodes(t *testing.T) {
	c := &Client{}
	rand.Read(c.wrapKey[:])
	for _, tc := range []struct {
		keysym uint32
		code   uint16
		name   string
	}{
		{appleKeysym(0xffe9), 0x3a, "Alt_L"},
		{appleKeysym(0xffea), 0x3d, "Alt_R"},
		{0xffeb, 0x37, "Super_L"},
		{0xffec, 0x36, "Super_R"},
		{0xffe3, 0x3b, "Control_L"},
		{0xffe1, 0x38, "Shift_L"},
	} {
		m, err := c.appleKeyMessage(tc.keysym, true)
		if err != nil {
			t.Fatal(err)
		}
		blk, _ := aes.NewCipher(c.wrapKey[:])
		blk.Decrypt(m[2:], m[2:])
		if got := binary.BigEndian.Uint16(m[16:]); got != tc.code || m[1] != keyFlagCodeValid|keyFlagPreferCode {
			t.Errorf("%s: key code %#x flags %d, want %#x", tc.name, got, m[1], tc.code)
		}
	}
}

// TestCloseReleasesKeys: a key still down when the connection closes is
// released first.
func TestCloseReleasesKeys(t *testing.T) {
	got := make(chan []byte, 4)
	c, err := pipe(t, Config{InFlight: 2}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(4, 4)
		for range 2 {
			got <- s.read(8)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	c.KeyEvent(0xffeb, true)
	c.Close()
	down, up := <-got, <-got
	if down[1] != 1 || up[1] != 0 || binary.BigEndian.Uint32(up[4:]) != 0xffeb {
		t.Errorf("key messages % x, % x", down, up)
	}
}

// TestApplePointerMapping covers server scaling and a video of another
// size than the framebuffer.
func TestApplePointerMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scale    float64
		layoutFB image.Point
		shown    image.Point
		in, want image.Point
	}{
		{"native", 1, image.Pt(3840, 2160), image.Pt(3840, 2160), image.Pt(100, 50), image.Pt(100, 50)},
		{"scale 0.5", 0.5, image.Pt(1920, 1080), image.Pt(1920, 1080), image.Pt(100, 50), image.Pt(201, 101)},
		{"half-size video", 1, image.Pt(3840, 2160), image.Pt(1920, 1080), image.Pt(100, 50), image.Pt(201, 101)},
		{"native video, scaled RFB", 0.5, image.Pt(1920, 1080), image.Pt(3840, 2160), image.Pt(100, 50), image.Pt(100, 50)},
	} {
		c := &Client{}
		c.layoutScale.Store(math.Float64bits(tc.scale))
		c.layout.Store(&DisplayLayout{Framebuffer: tc.layoutFB})
		c.size.Store(uint64(tc.shown.X)<<32 | uint64(tc.shown.Y))
		x, y, _ := c.applePointer(tc.in.X, tc.in.Y)
		if image.Pt(x, y) != tc.want {
			t.Errorf("%s: %v -> (%d,%d), want %v", tc.name, tc.in, x, y, tc.want)
		}
	}
}

func TestAppleMainDisplayVideoPointerMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		layoutFB image.Point
		scale    float64
		shown    image.Point
		in, want image.Point
	}{
		{"observed offset", image.Pt(9728, 2880), 1, image.Pt(5120, 2880), image.Pt(1228, 349), image.Pt(1228, 349)},
		{"main center", image.Pt(9728, 2880), 1, image.Pt(5120, 2880), image.Pt(2560, 1440), image.Pt(2560, 1440)},
		{"main right edge", image.Pt(9728, 2880), 1, image.Pt(5120, 2880), image.Pt(5119, 2879), image.Pt(5119, 2879)},
		{"half-size main video", image.Pt(9728, 2880), 1, image.Pt(2560, 1440), image.Pt(1228, 349), image.Pt(2457, 699)},
		{"native main video with scaled RFB", image.Pt(4864, 1440), 0.5, image.Pt(5120, 2880), image.Pt(1228, 349), image.Pt(1228, 349)},
		{"whole desktop", image.Pt(9728, 2880), 1, image.Pt(9728, 2880), image.Pt(6000, 500), image.Pt(6000, 500)},
		{"half-size whole desktop", image.Pt(4864, 1440), 0.5, image.Pt(4864, 1440), image.Pt(3000, 250), image.Pt(6001, 501)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			c.layout.Store(twoDisplays(tc.layoutFB))
			c.layoutScale.Store(math.Float64bits(tc.scale))
			c.size.Store(uint64(tc.shown.X)<<32 | uint64(tc.shown.Y))
			x, y, _ := c.applePointer(tc.in.X, tc.in.Y)
			if got := image.Pt(x, y); got != tc.want {
				t.Fatalf("%v -> %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAppleScroll: scrolling sends one native gesture per event, began then
// changed, and an ended phase once it stops; deltas become the Mac's
// points, positive upward.
func TestAppleScroll(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	var k recordKeys
	rand.Read(k.key[:])
	rand.Read(k.iv[:])
	c := &Client{conn: a, apple: true, appleExt: true, sealer: newRecordSealer(k)}
	c.appleCmds[msgAppleEvent/8] |= 0x80 >> (msgAppleEvent % 8)
	c.size.Store(3840<<32 | 2160)
	c.layout.Store(&DisplayLayout{Framebuffer: image.Pt(3840, 2160),
		Displays: []Display{{Scale: 1, Logical: image.Rect(0, 0, 1920, 1080)}}})
	in := newRecordReader(b, k)
	type ev struct {
		phase uint32
		dy    float32
	}
	got := make(chan ev, 8)
	go func() {
		for range 6 { // three scroll-wheel and gesture pairs
			h := make([]byte, 8)
			if _, err := io.ReadFull(in, h); err != nil {
				return
			}
			body := make([]byte, int(binary.BigEndian.Uint16(h[2:]))-4)
			io.ReadFull(in, body)
			if binary.BigEndian.Uint16(h[6:]) == eventGestureScroll {
				got <- ev{binary.BigEndian.Uint32(body[16:]), math.Float32frombits(binary.BigEndian.Uint32(body[4:]))}
			}
		}
	}()
	c.Scroll(100, 100, 0, 10) // 10 picture pixels down = 5 points
	c.Scroll(100, 100, 0, 4)
	want := []ev{{phaseBegan, -5}, {phaseChanged, -2}, {phaseEnded, 0}}
	for i, w := range want {
		select {
		case e := <-got:
			if e != w {
				t.Errorf("event %d = %+v, want %+v", i, e, w)
			}
		case <-time.After(time.Second):
			t.Fatalf("event %d missing", i)
		}
	}
}
