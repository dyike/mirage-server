package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"net"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/rfb"
)

type testClipboard struct{ updates, sets chan string }

func (c *testClipboard) Set(s string) error     { c.sets <- s; return nil }
func (c *testClipboard) Updates() <-chan string { return c.updates }
func (c *testClipboard) Close() error           { return nil }
func TestExistingClientExtensions(t *testing.T) {
	for _, encoding := range []int32{rfb.EncZRLE, rfb.EncTight, rfb.EncRaw} {
		t.Run(map[int32]string{16: "ZRLE", 7: "Tight", 0: "Raw"}[encoding], func(t *testing.T) {
			cfg, _ := testTLS(t)
			sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			clipboard := &testClipboard{make(chan string, 2), make(chan string, 2)}
			// A non-solid image spanning multiple Tight rectangles/ZRLE tiles, repeated
			// over one zlib stream. Both alpha and all channels are checked by the client.
			const width, height = 1100, 130
			pixels := make([]byte, width*height*4)
			for i := 0; i < len(pixels); i += 4 {
				pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = byte(i/4), byte(i/100), 197, 255
			}
			s := RFBServer{TLS: cfg, Password: "test", Width: width, Height: height, EmbeddedCursor: true,
				OpenClipboard: func() (Clipboard, error) { return clipboard, nil },
				Source: func(ctx context.Context, _ string, send func([]byte) error) error {
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-tick.C:
							if err := send(pixels); err != nil {
								return err
							}
						}
					}
				},
				Audio: func(ctx context.Context, f AudioFormat, send func([]byte) error) error {
					if f != (AudioFormat{3, 2, 48000}) {
						t.Errorf("unexpected audio format %+v", f)
					}
					if err := send([]byte{1, 2, 3, 4}); err != nil {
						return err
					}
					<-ctx.Done()
					return ctx.Err()
				},
			}
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, l) }()
			defer func() { cancel(); <-done }()
			frames := make(chan bool, 8)
			audio := make(chan []byte, 2)
			texts := make(chan string, 2)
			layout := make(chan rfb.DesktopLayout, 2)
			cursor := make(chan bool, 2)
			c, err := rfb.Dial(ctx, l.Addr().String(), rfb.Config{Password: "test", ServerFingerprint: hex.EncodeToString(sum[:]), Encodings: []int32{encoding, rfb.EncPseudoContinuousUpdates, rfb.EncPseudoExtendedDesktopSize, rfb.EncPseudoCursor}, OnUpdate: func(f *frame.Framebuffer, _ []image.Rectangle) {
				ok := len(f.Pix) == len(pixels)
				if ok {
					for i := range pixels {
						if f.Pix[i] != pixels[i] {
							ok = false
							break
						}
					}
				}
				select {
				case frames <- ok:
				default:
				}
			}, OnAudio: func(_, _ int, b []byte) { audio <- b }, OnCutText: func(s string) { texts <- s }, OnDesktop: func(d rfb.DesktopLayout) { layout <- d }, OnCursor: func(i *image.RGBA, _ image.Point) { cursor <- i == nil || i.Bounds().Empty() }})
			if err != nil {
				t.Fatal(err)
			}
			run := make(chan error, 1)
			go func() { run <- c.Run() }()
			defer func() { c.Close(); <-run }()
			deadline := time.After(4 * time.Second)
			for i := 0; i < 3; i++ {
				select {
				case ok := <-frames:
					if !ok {
						t.Fatal("pixel corruption")
					}
				case <-deadline:
					t.Fatal("no continuous frames")
				}
			}
			select {
			case b := <-audio:
				if string(b) != string([]byte{1, 2, 3, 4}) {
					t.Fatal("audio corrupt")
				}
			case <-deadline:
				t.Fatal("no audio")
			}
			select {
			case d := <-layout:
				if len(d.Screens) != 1 || d.Size != image.Pt(width, height) {
					t.Fatalf("layout %+v", d)
				}
			case <-deadline:
				t.Fatal("no desktop layout")
			}
			select {
			case hidden := <-cursor:
				if !hidden {
					t.Fatal("embedded cursor not hidden")
				}
			case <-deadline:
				t.Fatal("no cursor update")
			}
			clipboard.updates <- "caf\u00e9"
			select {
			case text := <-texts:
				if text != "caf\u00e9" {
					t.Fatal(text)
				}
			case <-deadline:
				t.Fatal("no server clipboard")
			}
			if err = c.CutText("ol\u00e9"); err != nil {
				t.Fatal(err)
			}
			select {
			case text := <-clipboard.sets:
				if text != "ol\u00e9" {
					t.Fatal(text)
				}
			case <-deadline:
				t.Fatal("no client clipboard")
			}
		})
	}
}

func TestFullVideoRefreshRestartsStream(t *testing.T) {
	for _, enc := range []uint32{rfbH264, rfbHEVC, encTight} {
		for _, incremental := range []byte{0, 1} {
			serverConn, clientConn := net.Pipe()
			state := &rfbState{encoding: enc}
			requests := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() { done <- new(RFBServer).readRFB(serverConn, state, requests, nil) }()
			message := make([]byte, 10)
			message[0], message[1] = 3, incremental
			if _, err := clientConn.Write(message); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requests:
			case <-time.After(time.Second):
				t.Fatal("refresh not queued")
			}
			state.Lock()
			revision := state.revision
			state.Unlock()
			clientConn.Close()
			serverConn.Close()
			<-done
			want := uint64(0)
			if incremental == 0 && enc != encTight {
				want = 1
			}
			if revision != want {
				t.Fatalf("encoding=%d incremental=%d revision=%d want=%d", enc, incremental, revision, want)
			}
		}
	}
}

func TestNegotiatedClientFeedbackReducesFrameRate(t *testing.T) {
	cfg, _ := testTLS(t)
	sum := sha256.Sum256(cfg.Certificates[0].Certificate[0])
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan int, 8)
	s := RFBServer{TLS: cfg, Password: "test", Width: 64, Height: 64, VideoFPS: 30,
		DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
			r.BindFPS(func(fps int) { changed <- fps })
			defer r.BindFPS(nil)
			tick := time.NewTicker(time.Second / 30)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
					if err := send(make([]byte, 64*64*4)); err != nil {
						return err
					}
				}
			}
		}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() { cancel(); l.Close(); <-done }()
	var c *rfb.Client
	frames := 0
	c, err = rfb.Dial(ctx, l.Addr().String(), rfb.Config{Password: "test", ServerFingerprint: hex.EncodeToString(sum[:]), Encodings: []int32{rfb.EncRaw}, OnUpdate: func(_ *frame.Framebuffer, _ []image.Rectangle) {
		frames++
		// Simulate a renderer that only draws every third submitted picture.
		if frames%3 == 0 {
			c.FramesPresented(c.SubmittedFrames())
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan error, 1)
	go func() { clientDone <- c.Run() }()
	defer func() { c.Close(); <-clientDone }()
	select {
	case fps := <-changed:
		if fps != 22 {
			t.Fatalf("unexpected first adaptive rate %d", fps)
		}
	case <-ctx.Done():
		t.Fatal("negotiated feedback never adjusted FPS")
	}
}
