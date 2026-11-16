package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/des"
	"crypto/tls"
	"image"
	"io"
	"math/bits"
	"net"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/server/input"
)

type wireEvent struct {
	kind       byte
	enc        uint32
	x, y, w, h int
	data       []byte
}

func wireConnect(t *testing.T, address, password string, cfg *tls.Config) net.Conn {
	t.Helper()
	c, e := net.Dial("tcp", address)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	read := func(n int) []byte {
		b, e := readBytes(c, n)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	write := func(b []byte) {
		if e := writeBytes(c, b); e != nil {
			t.Fatal(e)
		}
	}
	read(12)
	write([]byte("RFB 003.008\n"))
	read(2)
	write([]byte{19})
	read(2)
	write([]byte{0, 2})
	read(6)
	write(u32(261))
	read(1)
	secured := tls.Client(c, cfg)
	if e = secured.Handshake(); e != nil {
		t.Fatal(e)
	}
	c = secured
	challenge := read(16)
	var key [8]byte
	copy(key[:], password)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	block, _ := des.NewCipher(key[:])
	response := make([]byte, 16)
	block.Encrypt(response[:8], challenge[:8])
	block.Encrypt(response[8:], challenge[8:])
	write(response)
	if networkOrder.Uint32(read(4)) != 0 {
		t.Fatal("authentication failed")
	}
	write([]byte{1})
	init := read(24)
	read(int(networkOrder.Uint32(init[20:])))
	return c
}
func readWire(t *testing.T, c net.Conn) []wireEvent {
	t.Helper()
	read := func(n int) []byte {
		b, e := readBytes(c, n)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	kind := read(1)[0]
	switch kind {
	case 0:
		header := read(3)
		count := int(networkOrder.Uint16(header[1:]))
		events := make([]wireEvent, 0, count)
		for i := 0; i < count; i++ {
			r := read(12)
			v := wireEvent{kind: 0, enc: networkOrder.Uint32(r[8:]), x: int(networkOrder.Uint16(r)), y: int(networkOrder.Uint16(r[2:])), w: int(networkOrder.Uint16(r[4:])), h: int(networkOrder.Uint16(r[6:]))}
			switch v.enc {
			case 0:
				v.data = read(v.w * v.h * 4)
			case 0xfffffecc:
				h := read(4)
				v.data = append(h, read(int(h[0])*16)...)
			case 0xfffffefb:
				v.data = read(1)
			case 0xfffffefe, 0xfffffec4:
			default:
				t.Fatalf("unknown rectangle %x", v.enc)
			}
			events = append(events, v)
		}
		return events
	case 3:
		h := read(7)
		n := int32(networkOrder.Uint32(h[3:]))
		if n < 0 {
			n = -n
		}
		return []wireEvent{{kind: 3, data: read(int(n))}}
	default:
		t.Fatalf("unknown message %d", kind)
		return nil
	}
}
func TestRFBResizeKeysExtendedClipboard(t *testing.T) {
	cfg, clientTLS := testTLS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	controls := &testInput{events: make(chan input.Event, 16), closed: make(chan struct{})}
	clipboard := &testClipboard{make(chan string, 2), make(chan string, 2)}
	s := RFBServer{TLS: cfg, Password: "test", Width: 64, Height: 64, OpenInput: func() (InputSession, error) { return controls, nil }, OpenClipboard: func() (Clipboard, error) { return clipboard, nil }, SetDesktop: func(context.Context, DesktopLayout) error { return nil }, DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
		pixels := make([]byte, r.Desktop.Size.X*r.Desktop.Size.Y*4)
		tick := time.NewTicker(time.Millisecond * 10)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				if e := send(pixels); e != nil {
					return e
				}
			}
		}
	}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() { cancel(); <-done }()
	c := wireConnect(t, l.Addr().String(), "test", clientTLS)
	write := func(b []byte) {
		t.Helper()
		if e := writeBytes(c, b); e != nil {
			t.Fatal(e)
		}
	}
	encs := []uint32{0, 0xfffffecc, 0xfffffefe, 0xfffffefb, 0xfffffec4, extClipboard}
	msg := []byte{2, 0, 0, byte(len(encs))}
	for _, enc := range encs {
		msg = append(msg, u32(enc)...)
	}
	write(msg)
	for i := 0; i < 5; i++ {
		readWire(t, c)
	} // clipboard caps + key, LED, mouse, layout acknowledgements
	// Physical right Ctrl (QEMU extended XT 0x9d) maps to evdev 97.
	msg = []byte{255, 0, 0, 1}
	msg = append(msg, u32(0xffe4)...)
	msg = append(msg, u32(0x9d)...)
	write(msg)
	select {
	case ev := <-controls.events:
		if ev.Type != "keycode" || ev.Code != 97 || !ev.Down {
			t.Fatalf("key %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no physical key")
	}
	readWire(t, c) // changed LED state
	write([]byte{5, 129, 0, 32, 0, 16, 2})
	select {
	case ev := <-controls.events:
		if ev.Buttons != 257 || ev.X != .5 || ev.Y != .25 {
			t.Fatalf("pointer %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no extended pointer")
	}
	clipmsg := func(flags uint32, payload []byte) {
		data := append(u32(flags), payload...)
		write(append(append([]byte{6, 0, 0, 0}, u32(uint32(-int32(len(data))))...), data...))
	}
	clipmsg(clipCaps|clipRequest|clipNotify|clipProvide|1, u32(1<<20))
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	text := []byte("中文剪贴板\x00")
	z.Write(u32(uint32(len(text))))
	z.Write(text)
	z.Close()
	clipmsg(clipProvide|1, compressed.Bytes())
	select {
	case text := <-clipboard.sets:
		if text != "中文剪贴板" {
			t.Fatal("UTF-8 lost")
		}
	case <-time.After(time.Second):
		t.Fatal("no extended clipboard")
	}
	clipboard.updates <- "远端文字"
	v := readWire(t, c)[0]
	if v.kind != 3 || networkOrder.Uint32(v.data) != clipProvide|1 {
		t.Fatal("missing clipboard provide")
	}
	r, e := zlib.NewReader(bytes.NewReader(v.data[4:]))
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(r)
	r.Close()
	if e != nil || string(data[4:]) != "远端文字\x00" {
		t.Fatal("clipboard data corrupt")
	}
	write([]byte{3, 0, 0, 0, 0, 0, 0, 64, 0, 64})
	if v := readWire(t, c)[0]; v.w != 64 || v.enc != 0 {
		t.Fatal("missing initial frame")
	}
	// Change the encoded desktop, preserving the screen ID.
	msg = []byte{251, 0, 0, 80, 0, 64, 1, 0}
	screen := make([]byte, 16)
	networkOrder.PutUint32(screen, 1)
	networkOrder.PutUint16(screen[8:], 80)
	networkOrder.PutUint16(screen[10:], 64)
	write(append(msg, screen...))
	v = readWire(t, c)[0]
	if v.enc != 0xfffffecc || v.x != 1 || v.y != 0 || v.w != 80 {
		t.Fatalf("resize acknowledgement %+v", v)
	}
	v = readWire(t, c)[0]
	if v.enc != 0 || v.w != 80 || v.h != 64 {
		t.Fatal("old-size frame after resize")
	}
	c.Close()
	select {
	case <-controls.closed:
	case <-time.After(time.Second):
		t.Fatal("held controls not released")
	}
}
func TestCursorScaleAndMask(t *testing.T) {
	v := input.CursorImage{Pixels: []byte{128, 0, 0, 128, 0, 0, 0, 0, 0, 255, 0, 255, 0, 0, 255, 255}, Width: 2, Height: 2, Hotspot: image.Pt(1, 1), SourceWidth: 100, SourceHeight: 100}
	v = scaleCursor(v, image.Pt(200, 200))
	if v.Width != 4 || v.Hotspot != image.Pt(2, 2) {
		t.Fatal("cursor scaling")
	}
	message, e := cursorMessage(defaultRFBFormat, v)
	if e != nil {
		t.Fatal(e)
	}
	if message[16] != 255 {
		t.Fatal("premultiplied alpha not corrected")
	}
	if message[len(message)-4] != 0xc0 {
		t.Fatalf("wrong cursor alpha mask %x", message[len(message)-4:])
	}
}
