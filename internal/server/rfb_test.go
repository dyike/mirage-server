package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/input"
)

func TestExistingRFBClient(t *testing.T) {
	t.Run("ARDServerWithStandardClient", func(t *testing.T) { testExistingRFBClient(t, "mirage", "long-account-password", true) })
	t.Run("VNCPassword", func(t *testing.T) { testExistingRFBClient(t, "", "vnc-test") })
	t.Run("AccountPassword", func(t *testing.T) { testExistingRFBClient(t, "mirage", "long-account-password") })
}
func testExistingRFBClient(t *testing.T, username, password string, ard ...bool) {
	tlsConfig, _ := testTLS(t)
	sum := sha256.Sum256(tlsConfig.Certificates[0].Certificate[0])
	pin := hex.EncodeToString(sum[:])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controls := &testInput{events: make(chan input.Event, 16), closed: make(chan struct{})}
	var opened atomic.Int32
	s := RFBServer{ARD: len(ard) > 0 && ard[0], TLS: tlsConfig, Password: password, Username: username, Width: 64, Height: 64, OpenInput: func() (InputSession, error) { opened.Add(1); return controls, nil }, Source: func(ctx context.Context, codec string, send func([]byte) error) error {
		if codec != "raw" {
			return fmt.Errorf("unexpected codec %s", codec)
		}
		pixels := make([]byte, 64*64*4)
		for i := 0; i < len(pixels); i += 4 {
			pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = 17, 93, 201, 255
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := send(pixels); err != nil {
				return err
			}
		}
	}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	defer func() { cancel(); <-done }()
	cfg := rfb.Config{Username: username, Password: "wrong", ServerFingerprint: pin}
	if c, e := rfb.Dial(ctx, listener.Addr().String(), cfg); e == nil {
		c.Close()
		t.Fatal("bad password accepted")
	}
	if opened.Load() != 0 {
		t.Fatal("unauthenticated input created")
	}
	got := make(chan [4]byte, 1)
	cfg.Password = password
	cfg.OnUpdate = func(f *frame.Framebuffer, _ []image.Rectangle) {
		var p [4]byte
		copy(p[:], f.Pix[:4])
		select {
		case got <- p:
		default:
		}
	}
	c, err := rfb.Dial(ctx, listener.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := make(chan error, 1)
	go func() { run <- c.Run() }()
	defer func() { c.Close(); <-run }()
	select {
	case p := <-got:
		if p != [4]byte{17, 93, 201, 255} {
			t.Fatalf("pixels=%v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no framebuffer from unchanged RFB client")
	}
	if err = c.KeyEvent('a', true); err != nil {
		t.Fatal(err)
	}
	if err = c.PointerEvent(1, 32, 16); err != nil {
		t.Fatal(err)
	}
	for _, want := range []input.Event{{Type: "key", Symbol: 'a', Down: true}, {Type: "pointer", Buttons: 1, X: .5, Y: .25}} {
		select {
		case e := <-controls.events:
			if e != want {
				t.Fatalf("event=%+v want=%+v", e, want)
			}
		case <-time.After(time.Second):
			t.Fatal("input timeout")
		}
	}
	c.Close()
	select {
	case <-controls.closed:
	case <-time.After(time.Second):
		t.Fatal("input not released")
	}
}
func TestRFBPixelFormats(t *testing.T) {
	for _, test := range []struct {
		pf   rfbFormat
		want []byte
	}{
		{defaultRFBFormat, []byte{255, 128, 0, 0}},
		{rfbFormat{16, 16, 1, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0}, []byte{0xfc, 0x00}},
	} {
		if err := test.pf.validate(); err != nil {
			t.Fatal(err)
		}
		got, err := test.pf.pack([]byte{255, 128, 0, 255}, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(test.want) {
			t.Fatalf("pixels %x want %x", got, test.want)
		}
	}
	bad := defaultRFBFormat
	bad[10] = 8
	if bad.validate() == nil {
		t.Fatal("overlapping channels accepted")
	}
}

func TestSharedClientsAreIndependent(t *testing.T) {
	cfg, _ := testTLS(t)
	sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := RFBServer{TLS: cfg, Password: "test", Width: 64, Height: 64, MaxClients: 2, Source: func(ctx context.Context, _ string, send func([]byte) error) error {
		p := make([]byte, 64*64*4)
		tick := time.NewTicker(time.Millisecond * 10)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				if err := send(p); err != nil {
					return err
				}
			}
		}
	}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() { cancel(); <-done }()
	var clients []*rfb.Client
	var runs []chan error
	frames := []chan struct{}{make(chan struct{}, 4), make(chan struct{}, 4)}
	defer func() {
		for i, c := range clients {
			c.Close()
			<-runs[i]
		}
	}()
	for i := 0; i < 2; i++ {
		index := i
		c, e := rfb.Dial(ctx, l.Addr().String(), rfb.Config{Password: "test", ServerFingerprint: hex.EncodeToString(sum[:]), OnUpdate: func(*frame.Framebuffer, []image.Rectangle) {
			select {
			case frames[index] <- struct{}{}:
			default:
			}
		}})
		if e != nil {
			t.Fatal(e)
		}
		clients = append(clients, c)
		run := make(chan error, 1)
		runs = append(runs, run)
		go func() { run <- c.Run() }()
	}
	for _, f := range frames {
		select {
		case <-f:
		case <-time.After(time.Second):
			t.Fatal("shared client received no frame")
		}
	}
	clients[0].Close()
	if err := clients[1].Refresh(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames[1]:
	case <-time.After(time.Second):
		t.Fatal("closing one client stopped another")
	}
}
