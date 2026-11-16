package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"image"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/server/input"
	"github.com/dyike/mirage-server/internal/video"
)

// Apple's existing receiver binds the server's advertised port on its own IP.
// Distinct loopback IPs model two hosts without changing client code.
type ardLoopbackDialer struct {
	dropUDP bool
	dropOne *atomic.Bool
}

func (d ardLoopbackDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext(ctx, network, address)
}
func (d ardLoopbackDialer) DialUDP(local, remote netip.AddrPort) (net.Conn, error) {
	c, err := net.DialUDP("udp", net.UDPAddrFromAddrPort(local), net.UDPAddrFromAddrPort(remote))
	if err != nil {
		return nil, err
	}
	if d.dropUDP {
		return &ardBlackhole{Conn: c}, nil
	}
	if d.dropOne != nil {
		return &ardLossConn{Conn: c, drop: d.dropOne}, nil
	}
	return c, nil
}

type ardLossConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c *ardLossConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil {
			return n, err
		}
		if n > 2 && (b[1] < 192 || b[1] > 223) && c.drop.CompareAndSwap(true, false) {
			continue
		}
		return n, nil
	}
}

type ardBlackhole struct{ net.Conn }

func (c *ardBlackhole) Write(b []byte) (int, error) { return len(b), nil }

func TestARDExistingClient(t *testing.T) {
	for _, media := range []bool{false, true} {
		t.Run(map[bool]string{false: "records-input", true: "UDP-blocked-fallback"}[media], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cfg, _ := testTLS(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			controls := &testInput{events: make(chan input.Event, 32), closed: make(chan struct{})}
			var opened atomic.Int32
			srv := RFBServer{TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 64, Height: 64, OpenInput: func() (InputSession, error) { opened.Add(1); return controls, nil }, Source: func(ctx context.Context, codec string, send func([]byte) error) error {
				pixels := bytes.Repeat([]byte{17, 93, 201, 255}, 64*64)
				for {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := send(pixels); err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(10 * time.Millisecond):
					}
				}
			}}
			serverDone := make(chan error, 1)
			go func() { serverDone <- srv.Serve(ctx, listener) }()
			defer func() { cancel(); <-serverDone }()
			config := rfb.Config{Apple: true, Username: "test", Password: "wrong", Encodings: []int32{rfb.EncRaw}, Dialer: ardLoopbackDialer{dropUDP: media}, MediaStream: media, NoAudio: true, Network: rfb.NetworkLocal}
			if c, err := rfb.Dial(ctx, listener.Addr().String(), config); err == nil {
				c.Close()
				t.Fatal("bad ARD password accepted")
			}
			if opened.Load() != 0 {
				t.Fatal("input opened before authentication")
			}
			got := make(chan [4]byte, 1)
			config.Password = "test-password"
			config.OnUpdate = func(f *frame.Framebuffer, _ []image.Rectangle) {
				var pixel [4]byte
				copy(pixel[:], f.Pix)
				select {
				case got <- pixel:
				default:
				}
			}
			c, err := rfb.Dial(ctx, listener.Addr().String(), config)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Run() }()
			defer func() { c.Close(); <-done }()
			select {
			case pixel := <-got:
				if pixel != [4]byte{17, 93, 201, 255} {
					t.Fatal(pixel)
				}
			case <-ctx.Done():
				t.Fatalf("no ARD picture: %+v", c.MediaDiagnostics())
			}
			if media && c.MediaDiagnostics().Fallback == "" {
				t.Fatal("UDP blackhole did not fall back")
			}
			if err = c.KeyEvent('a', true); err != nil {
				t.Fatal(err)
			}
			if err = c.PointerEvent(1, 32, 16); err != nil {
				t.Fatal(err)
			}
			for _, want := range []input.Event{{Type: "key", Symbol: 'a', Down: true}, {Type: "pointer", Buttons: 1, X: .5, Y: .25}} {
				select {
				case got := <-controls.events:
					if got != want {
						t.Fatalf("got %+v want %+v", got, want)
					}
				case <-ctx.Done():
					t.Fatal("input timeout")
				}
			}
		})
	}
}
func TestARDRecords(t *testing.T) {
	block, _ := aes.NewCipher(make([]byte, 16))
	var wire bytes.Buffer
	writer := ardRecordWriter{target: &wire, cbc: cipher.NewCBCEncrypter(block, make([]byte, 16))}
	original := bytes.Repeat([]byte("record-boundary"), 20000)
	if _, err := writer.Write(original); err != nil {
		t.Fatal(err)
	}
	reader := ardRecordReader{source: &wire, cbc: cipher.NewCBCDecrypter(block, make([]byte, 16))}
	got := make([]byte, len(original))
	if _, err := readFullARD(&reader, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("record roundtrip mismatch")
	}
	var bad bytes.Buffer
	writer = ardRecordWriter{target: &bad, cbc: cipher.NewCBCEncrypter(block, make([]byte, 16))}
	writer.Write([]byte("secret"))
	bad.Bytes()[10] ^= 1
	reader = ardRecordReader{source: &bad, cbc: cipher.NewCBCDecrypter(block, make([]byte, 16))}
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("tampered record accepted")
	}
}
func readFullARD(r *ardRecordReader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := r.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestARDHardwareInterop(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_RFB_GPU") != "1" {
		t.Skip("opt-in hardware capture and decode")
	}
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	for _, codec := range []string{"h264", "hevc"} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cfg, _ := testTLS(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			errs := make(chan error, 8)
			var starts atomic.Int32
			var dropOne atomic.Bool
			srv := RFBServer{TLS: cfg, ARD: true, Username: "test", Password: "test-password", Width: 640, Height: 360, PreferHEVC: codec == "hevc", OnError: func(err error) {
				select {
				case errs <- err:
				default:
				}
			}, Source: func(ctx context.Context, selected string, send func([]byte) error) error {
				starts.Add(1)
				return capture.Run(ctx, capture.Options{Test: true, Codec: selected, Width: 640, Height: 360, FPS: 30, Bitrate: 2000}, func(f capture.Frame) error { return send(f.Data) })
			}}
			serverDone := make(chan error, 1)
			go func() { serverDone <- srv.Serve(ctx, listener) }()
			defer func() { cancel(); <-serverDone }()
			frames := make(chan video.Frame, 64)
			c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, MediaStream: true, Username: "test", Password: "test-password", NoAudio: true, Network: rfb.NetworkLocal, Dialer: ardLoopbackDialer{dropOne: &dropOne}, OnVideoFrame: func(f video.Frame) {
				select {
				case frames <- f:
				default:
				}
			}})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Run() }()
			defer func() { c.Close(); <-done }()
			for i := 0; i < 12; i++ {
				select {
				case f := <-frames:
					if f.W != 640 || f.H != 360 || f.Surface == 0 {
						t.Fatalf("not native 640x360: %+v", f)
					}
				case err := <-errs:
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatalf("SRTP timeout %+v", c.MediaDiagnostics())
				}
			}
			before := starts.Load()
			dropOne.Store(true)
			for recovered := 0; recovered < 12; {
				select {
				case <-frames:
					if starts.Load() > before {
						recovered++
					}
				case err := <-errs:
					t.Fatal(err)
				case <-ctx.Done():
					t.Fatalf("keyframe recovery timed out: starts=%d diagnostics=%+v", starts.Load(), c.MediaDiagnostics())
				}
			}
			if dropOne.Load() {
				t.Fatal("loss injection did not run")
			}
			diagnostics := c.MediaDiagnostics()
			if diagnostics.Fallback != "" || !diagnostics.Video.Hardware || !diagnostics.Video.ZeroCopy {
				t.Fatalf("unexpected TCP fallback: %+v", diagnostics)
			}
			t.Logf("unchanged Mac client received %s over SRTP: %+v", codec, diagnostics)
		})
	}
}
