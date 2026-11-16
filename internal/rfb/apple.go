package rfb

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"hash"
	"image"
	"io"
	"math"
	"math/big"
	"time"

	"github.com/dyike/mirage-server/internal/displaymeta"
	"github.com/dyike/mirage-server/internal/inputtrace"
)

// macOS Screen Sharing speaks an extended dialect when the client answers
// its "RFB 003.889" greeting with the same version. The extensions this
// client uses:
//
//   - ServerInit carries a flag word and a bitmap of the client messages the
//     server accepts, before the desktop name.
//   - After the client asks for it, the server sends a Rekey pseudo-rectangle
//     with AES keys wrapped in the authentication key. From then on both
//     directions travel in encrypted, checksummed records.
//   - DisplayLayout describes the remote displays and the scale the server
//     applies, which is how pointer positions map to the desktop.
//   - AutoFramebufferUpdate (message 9) makes the server push updates at a
//     fixed interval instead of answering requests.
//
// Message layouts come from observing macOS 26 and 27 and from the
// open-screen-sharing project's notes on ScreenSharing.app. Confirmed
// against macOS on a Mac mini: SRP and type 30 logins, the record layer,
// DisplayLayout under ScaleFactor 0.5 (21 updates/s at 1920x1080 against
// 10 at 4K), and pushing (2.4 times the updates of answering requests).

// Apple security types besides SecARD.
const (
	SecAppleRSA uint8 = 33
	SecAppleSRP uint8 = 36
)

// Apple encodings and pseudo-encodings.
const (
	EncAppleMediaStream    int32 = 1010
	EncAppleMVS            int32 = 1011
	EncAppleCursorPosition int32 = 1100
	EncAppleDisplayInfo    int32 = 1101
	EncAppleUserInfo       int32 = 1102
	EncAppleRekey          int32 = 1103
	EncAppleCursorImage    int32 = 1104
	EncAppleDisplayLayout  int32 = 1105
	EncAppleVendorKeysyms  int32 = 1107
	EncAppleKeyboardSource int32 = 1109
	EncAppleDeviceInfo     int32 = 1110
)

// appleMeta are the pseudo-encodings requested in the Apple dialect.
var appleMeta = []int32{
	displaymeta.Encoding, dynamicResolutionEncoding, EncPseudoExtendedDesktopSize,
	EncAppleCursorImage, EncAppleCursorPosition, EncPseudoDesktopSize, EncAppleDisplayInfo, EncAppleDisplayLayout,
}

// ServerInit flag bits in the Apple dialect.
const (
	appleFlagSessionSelect uint32 = 0x04
)

// Client-to-server Apple messages.
const (
	msgAppleAutoUpdate     = 0x09
	msgAppleSetMode        = 0x0a
	msgAppleSetDisplay     = 0x0d
	msgApplePointer        = 0x10 // encrypted pointer event
	msgAppleEncryption     = 0x12
	msgAppleViewerInfo     = 0x21
	appleModeControl       = 1
	defaultAppleUpdateTick = time.Second / 60
)

// Display is one remote display in a DisplayLayout.
type Display struct {
	Name string
	ID   uint32
	// Scale is the factor the server applies before encoding (1 = none).
	Scale float64
	// Logical is the display's area of the desktop in points; Backing is
	// the same area in pixels.
	Logical, Backing image.Rectangle
	Main             bool
}

// DisplayLayout is the server's description of its displays.
type DisplayLayout struct {
	SingleDisplayOnly bool
	Desktop           image.Point // whole desktop, in pixels
	Framebuffer       image.Point // what the server sends, after scaling
	Current           uint32      // shown display; 0xFFFFFFFF = all
	Displays          []Display
}

// AllDisplays selects the whole desktop in SelectDisplay.
const AllDisplays uint32 = 0xFFFFFFFF

// framebufferRects returns the shown displays' areas in framebuffer
// pixels: the backing rectangles, relative to the shown desktop and scaled
// to the framebuffer.
func (l *DisplayLayout) framebufferRects() []image.Rectangle {
	var shown []Display
	for _, d := range l.Displays {
		if l.Current == AllDisplays || d.ID == l.Current {
			shown = append(shown, d)
		}
	}
	if len(shown) == 0 {
		shown = l.Displays
	}
	var desk image.Rectangle
	for _, d := range shown {
		desk = desk.Union(d.Backing)
	}
	if desk.Empty() || l.Framebuffer.X <= 0 || l.Framebuffer.Y <= 0 {
		return nil
	}
	sx, sy := float64(l.Framebuffer.X)/float64(desk.Dx()), float64(l.Framebuffer.Y)/float64(desk.Dy())
	at := func(v, origin int, s float64) int { return int(math.Round(float64(v-origin) * s)) }
	rects := make([]image.Rectangle, 0, len(shown))
	for _, d := range shown {
		b := d.Backing
		rects = append(rects, image.Rect(at(b.Min.X, desk.Min.X, sx), at(b.Min.Y, desk.Min.Y, sy), at(b.Max.X, desk.Min.X, sx), at(b.Max.Y, desk.Min.Y, sy)))
	}
	return rects
}

func parseDisplayLayout(p []byte) (*DisplayLayout, error) {
	const head, rec = 0x14, 0x38
	if len(p) < head {
		return nil, protoErr("display layout of %d bytes", len(p))
	}
	n := int(be.Uint16(p[0x12:]))
	if n > 32 || len(p) < head+n*rec {
		return nil, protoErr("display layout of %d bytes lists %d displays", len(p), n)
	}
	l := &DisplayLayout{
		Desktop:     image.Pt(int(be.Uint16(p[2:])), int(be.Uint16(p[4:]))),
		Framebuffer: image.Pt(int(be.Uint16(p[6:])), int(be.Uint16(p[8:]))),
		Current:     be.Uint32(p[0x0a:]),
	}
	edges := func(b []byte) image.Rectangle { // top, left, bottom, right
		s := func(i int) int { return int(int16(be.Uint16(b[i:]))) }
		return image.Rect(s(2), s(0), s(6), s(4))
	}
	for i := range n {
		r := p[head+i*rec:]
		d := Display{
			Scale:   math.Float64frombits(be.Uint64(r[0x08:])),
			ID:      be.Uint32(r[0x10:]),
			Logical: edges(r[0x14:]),
			Backing: edges(r[0x1c:]),
			Main:    be.Uint32(r[0x24:])&1 != 0,
		}
		if !(d.Scale > 0 && d.Scale <= 1) {
			d.Scale = 1
		}
		l.Displays = append(l.Displays, d)
	}
	m, err := displaymeta.Parse(p[head+n*rec:])
	if err != nil {
		return nil, protoErr("invalid Mirage display metadata: %v", err)
	}
	l.SingleDisplayOnly = m.SingleDisplayOnly
	for i := range l.Displays {
		l.Displays[i].Name = m.Names[l.Displays[i].ID]
	}
	return l, nil
}

// --- handshake ---

// appleServerInit splits the extension header off the ServerInit name.
func (c *Client) appleServerInit(name string) string {
	if len(name) < 22 || name[0] != 0 || name[1] != 0 {
		return name
	}
	c.appleFlags = be.Uint32([]byte(name[2:6]))
	copy(c.appleCmds[:], name[6:22])
	c.appleExt = true
	return name[22:]
}

// appleAccepts reports whether the server listed client message t in its
// ServerInit bitmap.
func (c *Client) appleAccepts(t uint8) bool {
	return c.appleExt && c.appleCmds[t/8]&(0x80>>(t%8)) != 0
}

// selectSession answers the server's question of which login session to
// show: the console user's, or a separate one for the authenticated user
// when the console is someone else's.
func (c *Client) selectSession() error {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return err
	}
	n := int(be.Uint16(hdr[:]))
	if n < 13 {
		return protoErr("session selection of %d bytes", n)
	}
	rest := make([]byte, n-2)
	if _, err := io.ReadFull(c.r, rest); err != nil {
		return err
	}
	flags := be.Uint32(append(hdr[2:], rest...)[2:])
	c.trace("session select flags=%#x", flags)
	var body []byte
	switch {
	case flags&3 != 0: // share the console
		body = make([]byte, 72)
		be.PutUint16(body, 1)
		if flags&2 != 0 { // without asking the console user
			body[6] = 1
		}
	case flags&0xc == 4: // a session of our own
		body = make([]byte, 200)
		be.PutUint16(body, 2)
		body[6] = 2
		creds := body[0x48 : 0x48+128]
		putCString(creds[:64], c.cfg.Username)
		putCString(creds[64:], c.cfg.Password)
		b, err := aes.NewCipher(c.wrapKey[:])
		if err != nil {
			return err
		}
		for i := 0; i < len(creds); i += 16 {
			b.Encrypt(creds[i:i+16], creds[i:i+16])
		}
	default:
		return &AuthError{Reason: "the server offers no session to share"}
	}
	copy(body[8:71], "Mirage")
	if _, err := c.conn.Write(append(be.AppendUint16(nil, uint16(len(body))), body...)); err != nil {
		return err
	}
	var rl [2]byte
	if _, err := io.ReadFull(c.r, rl[:]); err != nil {
		return err
	}
	res := make([]byte, be.Uint16(rl[:]))
	if _, err := io.ReadFull(c.r, res); err != nil {
		return err
	}
	switch {
	case len(res) < 3:
		return protoErr("session selection result of %d bytes", len(res))
	case res[2] == 0:
		return nil
	case res[2] == 1:
		return &AuthError{Reason: "the console user declined"}
	default:
		return protoErr("session selection failed: %d", res[2])
	}
}

// appleStart asks for the encrypted record layer. The client must not send
// anything else until the Rekey arrives (see startRecords).
func (c *Client) appleStart() error {
	var b []byte
	// ViewerInfo: Apple's client describes itself with this fixed block.
	vi := make([]byte, 66)
	vi[0] = msgAppleViewerInfo
	be.PutUint16(vi[2:], 62)
	be.PutUint16(vi[4:], 1)
	for i, v := range []uint32{2, 6, 1, 0, 26, 6, 2} {
		be.PutUint32(vi[6+i*4:], v)
	}
	vi[34], vi[36], vi[37], vi[38], vi[44] = 0xb0, 0x0c, 0x03, 0x90, 0x40
	b = append(b, vi...)
	b = append(b, msgAppleSetMode, 0, 0, appleModeControl)
	if c.cfg.MediaStream && c.cfg.OnClipboard != nil && c.appleAccepts(msgAppleAutoClipboard) {
		b = append(b, msgAppleAutoClipboard, 0, 0, 1, 0, 0, 0, 0)
	}
	b = append(b, msgAppleEncryption, 0, 0, 1, 0, 1, 0, 1, 0, 0, 0, 1)
	b = append(b, msgAppleEncryption, 0, 0, 2, 0, 1, 0, 0)
	return c.send(b)
}

// appleFirstMessages runs once the record layer is up: the setup a
// standard client sends right after ServerInit.
func (c *Client) appleFirstMessages() error {
	if err := c.startClipboard(); err != nil {
		return err
	}
	if err := c.setPixelFormat(rgbaFormat); err != nil {
		return err
	}
	var encs []int32
	for _, e := range c.cfg.Encodings {
		if e >= 0 { // picture encodings; the pseudo ones are Apple's own here
			encs = append(encs, e)
		}
	}
	c.appleEncs = encs
	if c.cfg.MediaStream && c.appleAccepts(msgAppleMediaStream) {
		m := &mediaState{offer: c.firstMediaOffer()}
		c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.MaxBitrate = m.offer.maxBitrate })
		c.media.Store(m)
		c.watchNegotiation(m)
		encs = append([]int32{EncAppleMediaStream}, encs...)
	}
	if err := c.setEncodings(append(encs, appleMeta...)); err != nil {
		return err
	}
	if c.cfg.AppleScale > 0 {
		if err := c.appleScale(c.cfg.AppleScale); err != nil {
			return err
		}
	}
	if c.media.Load() == nil {
		c.economizeRFB()
	}
	if err := c.requestUpdate(false); err != nil {
		return err
	}
	return c.armAutoUpdate()
}

// armAutoUpdate selects push or polling mode explicitly. In push mode the
// server updates the whole framebuffer every UpdateInterval, and the client
// sends no incremental requests. A request ends pushing, so requestRect
// arms it again after each one (see appendArm).
func (c *Client) armAutoUpdate() error {
	if !c.appleAccepts(msgAppleAutoUpdate) {
		return nil
	}
	push := c.cfg.UpdateInterval >= 0 && c.pushSuits()
	c.pushing.Store(push)
	if !push {
		c.trace("requesting framebuffer updates instead of push")
	} else {
		c.trace("auto update every %v", c.updateTick())
	}
	return c.rearmAutoUpdate()
}

// Even polling needs an explicit AutoFramebufferUpdate: interval 0xffffffff
// disables periodic pushes while returning the server to ordinary pictures.
// A full-frame request alone can leave a negotiated media session black.
func (c *Client) rearmAutoUpdate() error {
	if !c.appleAccepts(msgAppleAutoUpdate) {
		return nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(c.appendArm(nil))
}

func (c *Client) updateTick() time.Duration {
	if c.cfg.UpdateInterval == 0 {
		return defaultAppleUpdateTick
	}
	return c.cfg.UpdateInterval
}

// appendArm appends message 9 for the current framebuffer size.
func (c *Client) appendArm(b []byte) []byte {
	size := c.size.Load()
	interval := uint32(math.MaxUint32)
	if c.pushing.Load() {
		interval = uint32(c.updateTick() / time.Microsecond)
	}
	b = append(b, msgAppleAutoUpdate, 0, 0, 1)
	b = be.AppendUint32(b, interval)
	b = append(b, 0, 0, 0, 0)
	b = be.AppendUint16(b, uint16(size>>32))
	return be.AppendUint16(b, uint16(size))
}

// SetServerScale asks macOS to scale the picture before encoding it, by a
// factor in (0, 1]; 1 turns scaling off. The new size arrives as a
// DisplayLayout. Apple dialect only.
func (c *Client) SetServerScale(f float64) error {
	if !c.apple || !(f > 0 && f <= 1) {
		return protoErr("server scaling unavailable")
	}
	return c.appleScale(f)
}

// SelectDisplay shows one remote display (an ID from DisplayLayout), or
// all of them with AllDisplays. Apple dialect only.
func (c *Client) SelectDisplay(id uint32) error {
	c.economy.picked.Store(true)
	return c.selectDisplay(id)
}

func (c *Client) selectDisplay(id uint32) error {
	if l := c.layout.Load(); l != nil && l.SingleDisplayOnly && id == AllDisplays {
		return protoErr("server supports individual displays only")
	}
	if !c.appleAccepts(msgAppleSetDisplay) {
		return protoErr("server cannot select displays")
	}
	b := []byte{msgAppleSetDisplay, boolByte(id == 0), 0, 0}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(be.AppendUint32(b, id))
}

// --- authentication ---

// srpAuth runs Apple's SRP-6a login (security type 36), which current
// macOS prefers: SHA-512, a 3072-bit or larger group, and a password key
// stretched with PBKDF2. Its session key wraps the record-layer keys.
// For RFB 3.7+, selectType sends the security selector with the identity in
// one write. macOS can discard a selector received on its own and then read
// the identity's leading zero as another security type instead of an SRP frame.
func srpAuth(r io.Reader, w io.Writer, username, password string, selectType bool) (key []byte, err error) {
	id := be.AppendUint16(nil, 0)
	id = be.AppendUint16(id, uint16(len(username)))
	id = append(id, username...)
	id = append(id, 0, 0, 0)
	id = append(be.AppendUint32(nil, uint32(len(id))), id...)
	var request []byte
	if selectType {
		request = append(request, SecAppleSRP)
	}
	request = append(be.AppendUint32(request, uint32(len(id))), id...)
	if _, err := w.Write(request); err != nil {
		return nil, err
	}
	read := func() ([]byte, error) {
		var n [4]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return nil, err
		}
		size := be.Uint32(n[:])
		if size < 4 { // a SecurityResult in its place: rejected
			return nil, &AuthError{}
		}
		if size > 1<<16 {
			return nil, protoErr("SRP message of %d bytes", size)
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return b[4:], nil
	}
	msg, err := read()
	if err != nil {
		return nil, err
	}
	f := fields{b: msg}
	f.u8()
	nb := f.take(int(f.u16()))
	gb := f.take(int(f.u16()))
	salt := f.take(int(f.u8()))
	bb := f.take(int(f.u16()))
	iter := f.u64()
	opts := f.take(int(f.u16()))
	if f.bad {
		return nil, protoErr("short SRP challenge")
	}
	if len(nb) < 256 || iter == 0 || iter > 10_000_000 {
		return nil, protoErr("SRP group of %d bits, %d iterations", len(nb)*8, iter)
	}
	A, M1, K, M2, err := srpClient(nb, gb, salt, bb, int(iter), password)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	var out []byte
	out = be.AppendUint16(out, uint16(len(A)))
	out = append(out, A...)
	out = append(out, byte(len(M1)))
	out = append(out, M1...)
	out = be.AppendUint16(out, uint16(len(opts)))
	out = append(out, opts...)
	out = append(out, 16)
	out = append(out, nonce...)
	env := be.AppendUint32(nil, uint32(4+len(out)))
	env = be.AppendUint32(env, uint32(len(out)))
	if _, err := w.Write(append(env, out...)); err != nil {
		return nil, err
	}
	msg, err = read()
	if err != nil {
		return nil, err
	}
	f = fields{b: msg}
	proof := f.take(int(f.u8()))
	if f.bad || subtle.ConstantTimeCompare(proof, M2) != 1 {
		return nil, &AuthError{Reason: "the server's SRP proof does not match"}
	}
	sum := sha256.Sum256(K)
	return sum[:16], nil
}

// srpClient computes the client side of Apple's SRP-6a variant: the
// identity hashed into M1 is empty, and x = H(salt | H(":" | P)) where P is
// 128 bytes of PBKDF2-HMAC-SHA512 over the password.
func srpClient(nb, gb, salt, bb []byte, iter int, password string) (A, M1, K, M2 []byte, err error) {
	N := new(big.Int).SetBytes(nb)
	g := new(big.Int).SetBytes(gb)
	B := new(big.Int).SetBytes(bb)
	if new(big.Int).Mod(B, N).Sign() == 0 {
		return nil, nil, nil, nil, protoErr("invalid SRP server value")
	}
	pad := func(v *big.Int) []byte { return v.FillBytes(make([]byte, len(nb))) }
	H := func(parts ...[]byte) []byte {
		h := sha512.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}
	P, err := pbkdf2.Key(sha512.New, password, salt, iter, 128)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	x := new(big.Int).SetBytes(H(salt, H([]byte(":"), P)))
	k := new(big.Int).SetBytes(H(nb, pad(g)))
	ab := make([]byte, 64)
	rand.Read(ab)
	a := new(big.Int).SetBytes(ab)
	A = pad(new(big.Int).Exp(g, a, N))
	u := new(big.Int).SetBytes(H(A, pad(B)))
	// S = (B - k·g^x)^(a + u·x) mod N
	base := new(big.Int).Exp(g, x, N)
	base.Mul(base, k).Sub(B, base).Mod(base, N)
	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, a)
	K = H(pad(new(big.Int).Exp(base, exp, N)))
	hn, hg := H(nb), H(pad(g))
	for i := range hn {
		hn[i] ^= hg[i]
	}
	M1 = H(hn, H(nil), salt, A, pad(B), K)
	M2 = H(A, M1, K)
	return A, M1, K, M2, nil
}

// fields reads the big-endian length-prefixed fields of Apple's security
// messages; a short message sets bad.
type fields struct {
	b   []byte
	bad bool
}

func (f *fields) take(n int) []byte {
	if f.bad || n > len(f.b) {
		f.bad = true
		return nil
	}
	v := f.b[:n]
	f.b = f.b[n:]
	return v
}

func (f *fields) u8() uint64 {
	if b := f.take(1); b != nil {
		return uint64(b[0])
	}
	return 0
}

func (f *fields) u16() uint64 {
	if b := f.take(2); b != nil {
		return uint64(be.Uint16(b))
	}
	return 0
}

func (f *fields) u64() uint64 {
	if b := f.take(8); b != nil {
		return be.Uint64(b)
	}
	return 0
}

// --- record layer ---

// Each record is a 2-byte length followed by that many bytes of AES-128-CBC
// ciphertext (the chain continues across records). The plaintext holds a
// 2-byte body length, the body, zero padding and, in its last 20 bytes, a
// SHA-1 over a record sequence number and everything before the checksum.
const (
	recSum     = sha1.Size
	recMaxSize = 65520
	recMaxBody = recMaxSize - 2 - recSum
)

type recordKeys struct{ key, iv [16]byte }

// unwrapRekey decodes a Rekey body: a generation number, then key and IV
// encrypted under the current wrap key.
func unwrapRekey(wrap [16]byte, body []byte) (recordKeys, error) {
	var k recordKeys
	b, err := aes.NewCipher(wrap[:])
	if err != nil {
		return k, err
	}
	b.Decrypt(k.key[:], body[4:20])
	b.Decrypt(k.iv[:], body[20:36])
	return k, nil
}

type recordSealer struct {
	cbc cipher.BlockMode
	sum hash.Hash
	seq uint32
	buf []byte
}

func newRecordSealer(k recordKeys) *recordSealer {
	b, _ := aes.NewCipher(k.key[:])
	return &recordSealer{cbc: cipher.NewCBCEncrypter(b, k.iv[:]), sum: sha1.New()}
}

// seal returns data as records, in a buffer reused by the next call.
func (s *recordSealer) seal(data []byte) []byte {
	out := s.buf[:0]
	for len(data) > 0 || len(out) == 0 {
		body := data[:min(len(data), recMaxBody)]
		data = data[len(body):]
		size := (2 + len(body) + recSum + 15) &^ 15
		start := len(out)
		out = append(out, make([]byte, 2+size)...)
		rec := out[start:]
		be.PutUint16(rec, uint16(size))
		pt := rec[2:]
		be.PutUint16(pt, uint16(len(body)))
		copy(pt[2:], body)
		end := size - recSum
		s.sum.Reset()
		s.sum.Write(be.AppendUint32(nil, s.seq))
		s.sum.Write(pt[:end])
		s.sum.Sum(pt[end:end])
		s.cbc.CryptBlocks(pt, pt)
		s.seq++
		if len(data) == 0 {
			break
		}
	}
	s.buf = out
	return out
}

// recordReader yields the plaintext of the records read from src.
type recordReader struct {
	src  io.Reader
	cbc  cipher.BlockMode
	sum  hash.Hash
	seq  uint32
	buf  []byte
	left []byte
}

func newRecordReader(src io.Reader, k recordKeys) *recordReader {
	b, _ := aes.NewCipher(k.key[:])
	return &recordReader{src: src, cbc: cipher.NewCBCDecrypter(b, k.iv[:]), sum: sha1.New(), buf: make([]byte, recMaxSize)}
}

func (r *recordReader) Read(p []byte) (int, error) {
	for len(r.left) == 0 {
		if err := r.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.left)
	r.left = r.left[n:]
	return n, nil
}

func (r *recordReader) next() error {
	var h [2]byte
	if _, err := io.ReadFull(r.src, h[:]); err != nil {
		return err
	}
	size := int(be.Uint16(h[:]))
	if size < 32 || size%16 != 0 || size > recMaxSize {
		return protoErr("record of %d bytes", size)
	}
	ct := r.buf[:size]
	if _, err := io.ReadFull(r.src, ct); err != nil {
		return err
	}
	r.cbc.CryptBlocks(ct, ct)
	end := size - recSum
	r.sum.Reset()
	r.sum.Write(be.AppendUint32(nil, r.seq))
	r.sum.Write(ct[:end])
	var sum [recSum]byte
	if subtle.ConstantTimeCompare(r.sum.Sum(sum[:0]), ct[end:size]) != 1 {
		return protoErr("checksum mismatch in record %d", r.seq)
	}
	n := int(be.Uint16(ct))
	if 2+n > end {
		return protoErr("record body of %d bytes in %d", n, size)
	}
	r.seq++
	r.left = ct[2 : 2+n]
	return nil
}

// startRecords switches both directions to the record layer. It runs on
// the reader goroutine between rectangles; bytes already buffered belong
// to the record stream.
func (c *Client) startRecords(body []byte) error {
	k, err := unwrapRekey(c.wrapKey, body)
	if err != nil {
		return err
	}
	c.trace("record layer on")
	c.r = bufio.NewReaderSize(newRecordReader(c.r, k), 256<<10)
	c.wmu.Lock()
	c.sealer = newRecordSealer(k)
	c.wrapKey = k.key // encrypted pointer events use the record key
	c.wmu.Unlock()
	c.recordsOn = true
	return nil
}

// --- server messages and pseudo-encodings ---

// applePseudo handles an Apple pseudo-rectangle; ok is false for other
// encodings.
func (c *Client) applePseudo(enc int32, r image.Rectangle) (ok bool, resized bool, err error) {
	switch enc {
	case EncAppleRekey:
		body := make([]byte, 36)
		if _, err := io.ReadFull(c.r, body); err != nil {
			return true, false, err
		}
		if c.recordsOn {
			return true, false, protoErr("rekey during the session")
		}
		return true, false, c.startRecords(body)
	case EncAppleDisplayLayout:
		p, err := c.readU16Block()
		if err != nil {
			return true, false, err
		}
		l, err := parseDisplayLayout(p)
		if err != nil {
			return true, false, err
		}
		c.layout.Store(l)
		scale := 1.0
		if len(l.Displays) > 0 {
			scale = l.Displays[0].Scale
		}
		c.layoutScale.Store(math.Float64bits(scale))
		c.trace("display layout desktop=%v fb=%v current=%#x displays=%d scale=%v",
			l.Desktop, l.Framebuffer, l.Current, len(l.Displays), scale)
		if c.cfg.OnDisplays != nil {
			c.cfg.OnDisplays(*l)
		}
		resized := false
		if fb := l.Framebuffer; fb.X > 0 && fb.Y > 0 && (fb.X != c.fb.W || fb.Y != c.fb.H) {
			c.resizeFB(fb.X, fb.Y)
			resized = true
		}
		c.cov.setDisplays(l.framebufferRects())
		c.showMainDisplay(l)
		return true, resized, nil
	case EncAppleDisplayInfo:
		var h [10]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return true, false, err
		}
		if _, err := c.r.Discard(int(be.Uint16(h[8:])) * 0x1c); err != nil {
			return true, false, err
		}
		if r.Dx() > 0 && r.Dy() > 0 && (r.Dx() != c.fb.W || r.Dy() != c.fb.H) {
			c.resizeFB(r.Dx(), r.Dy())
			return true, true, nil
		}
		return true, false, nil
	case EncAppleCursorImage:
		return true, false, c.appleCursor(r)
	case EncAppleMediaStream:
		return true, false, c.mediaReply()
	case EncAppleCursorPosition:
		// The viewer draws the pointer where the user puts it; the
		// server's idea of it is only traced.
		c.trace("cursor at %d,%d", r.Min.X, r.Min.Y)
		inputtrace.Printf("mac cursor position=%v", r.Min)
		if c.cfg.OnCursorPosition != nil {
			c.cfg.OnCursorPosition(r.Min)
		}
		return true, false, nil
	case EncAppleUserInfo, EncAppleVendorKeysyms, EncAppleKeyboardSource, EncAppleDeviceInfo:
		_, err := c.readU16Block()
		return true, false, err
	}
	return false, false, nil
}

func (c *Client) readU16Block() ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(c.r, n[:]); err != nil {
		return nil, err
	}
	b := c.scratch(int(be.Uint16(n[:])))
	_, err := io.ReadFull(c.r, b)
	return b, err
}

// appleCursor decodes CursorImage: an ID (below 1000 a system cursor with
// no image), then a zlib block of w·h pixels in the client's pixel format
// followed by w·h alpha bytes. A zero-length block reselects a cached ID.
// x, y is the hotspot.
func (c *Client) appleCursor(r image.Rectangle) error {
	var h [4]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	id := be.Uint32(h[:])
	c.trace("cursor image id %d", id)
	inputtrace.Printf("mac cursor id=%d hotspot=%v size=%v", id, r.Min, r.Size())
	if id <= 999 {
		// A system cursor has no bitmap. Clear the previous custom shape so
		// the viewer can use its default pointer instead of a stale image.
		if c.cfg.OnCursor != nil {
			c.cfg.OnCursor(image.NewRGBA(image.Rectangle{}), image.Point{})
		}
		return nil
	}
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	n := be.Uint32(h[:])
	if n > 1<<20 {
		return protoErr("cursor image of %d bytes", n)
	}
	if n == 0 {
		if cur, ok := c.cursors[id]; ok && c.cfg.OnCursor != nil {
			c.cfg.OnCursor(cur.img, cur.hot)
		}
		return nil
	}
	z := make([]byte, n)
	if _, err := io.ReadFull(c.r, z); err != nil {
		return err
	}
	w, ht := r.Dx(), r.Dy()
	if w <= 0 || ht <= 0 || w > 512 || ht > 512 || len(z) < 2 {
		return nil
	}
	px := w * ht
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(z[2:])), int64(px*5)))
	if err != nil && err != io.ErrUnexpectedEOF || len(raw) != px*5 {
		c.trace("cursor %d does not inflate: %v", id, err)
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, ht))
	alpha := raw[px*4:]
	for i := range px {
		a := uint32(alpha[i])
		for j := range 3 {
			img.Pix[i*4+j] = uint8(uint32(raw[i*4+j]) * a / 255)
		}
		img.Pix[i*4+3] = uint8(a)
	}
	if c.cursors == nil {
		c.cursors = map[uint32]appleCursor{}
	}
	cur := appleCursor{img: img, hot: r.Min}
	c.cursors[id] = cur
	if c.cfg.OnCursor != nil {
		c.cfg.OnCursor(img, cur.hot)
	}
	return nil
}

type appleCursor struct {
	img *image.RGBA
	hot image.Point
}

// appleMessage skips the Apple server messages this client does not use
// (status, accessibility, drop destination, pasteboard, drag); ok is false
// for message types it does not know.
func (c *Client) appleMessage(t byte) (ok bool, err error) {
	if t == msgAppleClipboard {
		return true, c.receiveClipboard()
	}
	var hdr []byte
	var size func([]byte) int
	switch t {
	case 0x14: // status
		hdr, size = make([]byte, 3), func(h []byte) int { return int(be.Uint16(h[1:])) }
	case 0x15: // accessibility
		hdr, size = make([]byte, 14), func(h []byte) int { return int(binary.LittleEndian.Uint16(h[12:])) }
	case 0x1e: // drop destination
		hdr, size = make([]byte, 11), func(h []byte) int { return int(be.Uint32(h[7:])) }
	case 0x1f, 0x20: // pasteboard, drag
		hdr, size = make([]byte, 15), func(h []byte) int { return int(be.Uint32(h[11:])) }
	default:
		return false, nil
	}
	if _, err := io.ReadFull(c.r, hdr); err != nil {
		return true, err
	}
	n := size(hdr)
	if n > 100<<20 {
		return true, protoErr("message %#x of %d bytes", t, n)
	}
	if t == 0x14 {
		body := make([]byte, n)
		if _, err := io.ReadFull(c.r, body); err != nil {
			return true, err
		}
		if len(body) >= 4 {
			c.trace("apple status %d", be.Uint16(body[2:]))
			if be.Uint16(body[2:]) == 2 {
				return true, c.fetchClipboard()
			}
		}
		return true, nil
	}
	c.trace("apple message %#x, %d bytes", t, n)
	_, err = c.r.Discard(n)
	return true, err
}

// applePointer converts a position in the shown picture to the server's
// desktop pixels. macOS scales the RFB framebuffer (DisplayLayout's scale)
// but not pointer positions, and the media stream's video may differ in
// size from the framebuffer DisplayLayout describes.
func (c *Client) applePointer(x, y int) (int, int, bool) {
	s := math.Float64frombits(c.layoutScale.Load())
	if !(s > 0 && s <= 1) {
		s = 1
	}
	fx, fy := 1/s, 1/s
	if l := c.layout.Load(); l != nil && l.Framebuffer.X > 0 && l.Framebuffer.Y > 0 {
		size := c.size.Load()
		if w, h := float64(size>>32), float64(size&0xFFFFFFFF); w > 0 && h > 0 {
			// macOS can describe all displays in DisplayLayout while its
			// video stream contains only the main display. A picture with
			// the main display's aspect ratio, rather than the desktop's,
			// must map to that display instead of stretching across both.
			if l.Current == AllDisplays && len(l.Displays) > 1 && float64(l.Framebuffer.X)*h != float64(l.Framebuffer.Y)*w {
				for _, d := range l.Displays {
					b := d.Backing
					if d.Main && !b.Empty() && float64(b.Dx())*h == float64(b.Dy())*w {
						mapPixel := func(v int, origin int, factor float64) int {
							if factor == 1 {
								return origin + v
							}
							return origin + int((float64(v)+0.5)*factor)
						}
						return mapPixel(x, b.Min.X, float64(b.Dx())/w), mapPixel(y, b.Min.Y, float64(b.Dy())/h), true
					}
				}
			}
			fx *= float64(l.Framebuffer.X) / w
			fy *= float64(l.Framebuffer.Y) / h
		}
	}
	if fx == 1 && fy == 1 {
		return x, y, false
	}
	return int((float64(x) + 0.5) * fx), int((float64(y) + 0.5) * fy), true
}

// encryptedPointer builds message 0x10 for mouse motion and button
// changes: the pointer event in one AES block under the record key.
func (c *Client) encryptedPointer(buttons uint8, x, y int) ([]byte, error) {
	m := make([]byte, 18)
	m[0] = msgApplePointer
	blk := m[2:]
	blk[10], blk[11] = 0xff, buttons
	be.PutUint16(blk[12:], uint16(x))
	be.PutUint16(blk[14:], uint16(y))
	b, err := aes.NewCipher(c.wrapKey[:])
	if err != nil {
		return nil, err
	}
	b.Encrypt(blk, blk)
	return m, nil
}
