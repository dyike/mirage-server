package rfb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dyike/mirage-server/internal/audio"
	"github.com/dyike/mirage-server/internal/clip"
	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/inputtrace"
	"github.com/dyike/mirage-server/internal/video"
	"github.com/dyike/mirage-server/internal/vncfeedback"
)

var be = binary.BigEndian

// Config controls a connection. Callbacks run on the reader goroutine
// started by Run, or for the media stream's pictures on the decoder's
// thread, never two at once; they must return quickly and must not call
// Run.
type Config struct {
	// OnAudio receives negotiated S16LE system audio. The buffer is owned by the callback.
	OnAudio func(rate, channels int, pcm []byte)
	// ServerName is used for TLS certificate verification (filled by Dial).
	ServerName string
	// ServerFingerprint pins the SHA-256 certificate or RSA public-key wire
	// encoding. A mismatch requires explicit OnServerIdentity approval.
	ServerFingerprint string
	// OnServerIdentity runs synchronously during the handshake, before any
	// credentials are sent. It must return when its context is canceled.
	OnServerIdentity func(context.Context, string) bool

	// Username is used by ARD, RSA-AES account auth and VeNCrypt Plain.
	// Classic VNC challenge-response and password-only RSA-AES ignore it.
	Username string
	Password string
	// Exclusive asks the server to disconnect other viewers.
	Exclusive bool
	// Encodings in preference order. Nil uses DefaultEncodings.
	Encodings []int32

	// OnUpdate receives the framebuffer and the rectangles changed by one
	// FramebufferUpdate. fb is only valid during the call.
	OnUpdate func(fb *frame.Framebuffer, dirty []image.Rectangle)
	// OnResize runs after the remote desktop changed size, before the
	// OnUpdate that repaints it.
	OnResize  func(w, h int)
	OnCutText func(text string)
	// OnClipboard enables Apple text/image sharing only with MediaStream.
	OnClipboard func(clip.Content)
	// OnCursor receives the remote cursor shape when the server supports
	// the Cursor pseudo-encoding; the viewer then draws the cursor itself,
	// with no network round trip. img is premultiplied RGBA owned by the
	// callee; an empty img hides the cursor.
	OnCursor func(img *image.RGBA, hotspot image.Point)
	OnBell   func()

	// Trace, when set, receives one line per update rectangle and request,
	// for diagnosing what the server sends.
	Trace io.Writer

	// FramebufferLock, when set, is held while the client writes framebuffer
	// pixels or resizes it. Network reads happen outside it, so another
	// goroutine can copy pixels out between rectangles without waiting on
	// the connection.
	FramebufferLock sync.Locker
	// OnPixels, when set, runs on the reader goroutine with FramebufferLock
	// held, right after the client wrote the pixels in r and before it
	// releases the lock. A viewer that renders straight from the
	// framebuffer marks r changed here, one rectangle at a time.
	OnPixels func(fb *frame.Framebuffer, r image.Rectangle)

	// Record, when set, receives every byte the server sends, for replaying
	// a session offline (see cmd/replay). It contains the remote screen.
	Record io.Writer

	// InFlight is how many update requests the client keeps outstanding
	// (default 4), so the server can start the next update early. macOS
	// pipelines them: scrolling a 4K screen went from 15 fps with 2 to 27
	// with 4.
	InFlight int
	// AppleScale, when positive, sends macOS Screen Sharing's ScaleFactor
	// message (0x08) asking the server to scale before encoding.
	// In the Apple dialect the server reports the scale in DisplayLayout
	// and pointer positions follow it; otherwise the client infers it from
	// the resize that answers the request.
	AppleScale float64

	// Apple speaks macOS Screen Sharing's extended protocol (see apple.go)
	// when the server offers it and Username is set: encrypted transport,
	// display layout, server-pushed updates.
	Apple bool
	// Security overrides the preference order of security types, e.g.
	// []uint8{SecAppleSRP} to force SRP.
	Security []uint8
	// UpdateInterval is how often the server pushes updates in the Apple
	// dialect; 0 means 1/60 s, negative turns pushing off.
	UpdateInterval time.Duration
	// Network says how the server is reached: NetworkLocal or NetworkRemote,
	// or "" to tell from the interfaces. Remote turns push mode off and
	// caps the media stream's bit rate.
	Network string
	// OnDisplays receives the remote display layout (Apple dialect).
	OnDisplays func(DisplayLayout)
	// OnDesktop receives standard RFB screen geometry, independent of Apple display selection.
	OnDesktop func(DesktopLayout)
	// MediaStream asks macOS for its high-performance mode (HEVC over
	// SRTP) in the Apple dialect. Experimental: negotiation only.
	MediaStream bool
	// MediaRemoteDesktop selects RemoteDesktop video negotiation independently
	// of link classification. It does not impose a bitrate cap on a local link.
	MediaRemoteDesktop bool
	// MediaMaxBitrate limits the video offer in bit/s. A positive limit
	// uses RemoteDesktop mode even on a local link to bound video bursts.
	// Zero leaves local video uncapped and uses the normal remote cap.
	MediaMaxBitrate uint64
	// NoAudio leaves the media stream's sound alone; MuteAudio receives it
	// without playing it, for tests.
	NoAudio, MuteAudio bool
	// OnVideoFrame, when set, receives the media stream's pictures to draw
	// from their IOSurface without a copy (see video.Frame); the client
	// then leaves its framebuffer alone and calls no OnUpdate for them. It
	// runs on the decoder's thread.
	OnVideoFrame func(f video.Frame)
	// OnMediaAnswer receives the server's answer to the media offer.
	OnMediaAnswer func(body []byte)
	// Diagnostics enables metadata-only media logging and output-route queries.
	Diagnostics bool
	// ConnectionDiagnostics logs handshake stages and bounded picture recovery,
	// without enabling payload traces, media logs or output-route queries.
	ConnectionDiagnostics bool
	OnMediaDiagnostics    func(MediaDiagnostics)
	// PlainKeys sends KeyEvents in the Apple dialect too, instead of the
	// encrypted key messages macOS needs for password fields. For tests.
	PlainKeys bool
	// OnCursorPosition receives the pointer position the server reports,
	// in framebuffer pixels (Apple dialect).
	OnCursorPosition func(image.Point)
	// Dialer, when set, carries the connection and the media stream's UDP
	// legs instead of the system's network, e.g. through a WireGuard tunnel.
	Dialer Dialer
}

// Dialer opens connections on another network than the system's.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
	// DialUDP opens a connected UDP socket bound to local.
	DialUDP(local, remote netip.AddrPort) (net.Conn, error)
}

// DefaultEncodings is what the client asks for when Config.Encodings is nil.
var DefaultEncodings = []int32{
	EncZRLE, EncCopyRect, EncRaw,
	EncPseudoExtendedDesktopSize, EncPseudoDesktopSize, EncPseudoLastRect,
	EncPseudoCursor, EncPseudoContinuousUpdates,
}

// Client is one RFB connection. Input methods are safe for concurrent use.
type Client struct {
	audio            audioState
	feedback         videoFeedback
	hevcActive       bool
	hevcDisabled     bool
	videoRecovery    map[image.Rectangle]*videoRecovery
	h264Factory      h264Factory
	h264Contexts     map[image.Rectangle]*orderedH264
	h264Clock        uint64
	h264Requested    bool // immutable after construction
	h264Disabled     bool
	h264Picture      video.Frame
	h264PictureDirty bool
	h264Status       atomic.Pointer[video.DecoderStatus]
	h264Frames       atomic.Int64

	authContext   context.Context
	decodeContext context.Context
	stopDecode    context.CancelFunc

	conn net.Conn
	r    *bufio.Reader
	cfg  Config

	Version string // negotiated protocol, e.g. "003.008"
	Name    string // desktop name from ServerInit
	Server  PixelFormat

	dynamicResolution atomic.Bool
	resizeRequested   image.Point                   // wmu
	standardLayout    atomic.Pointer[DesktopLayout] // RFB ExtendedDesktopSize, separate from Apple layout

	wmu         sync.Mutex
	wbuf        [20]byte
	keybuf      []byte          // reused keyboard message batch; wmu
	lastButtons uint8           // last mask sent; wmu
	held        map[uint32]bool // keysyms pressed and not released (as sent); wmu
	scroll      scrollGesture

	// pictureMu makes the reader goroutine and the media decoder thread
	// take turns at the framebuffer, its bookkeeping and the picture
	// callbacks (OnUpdate, OnResize), which therefore never overlap.
	pictureMu sync.Mutex
	fb        frame.Framebuffer
	// received counts bytes read from the connection, for tracing.
	received          atomic.Int64
	encoding          atomic.Int64 // last decoded picture encoding + 1; zero is unknown
	clipboardActive   atomic.Bool
	clipboardSequence uint32 // wmu; each pasteboard fetch has a distinct echo ID
	clipboardSendMu   sync.Mutex
	clipboardMu       sync.Mutex
	clipboardOffer    *clipboardConfirmation
	// mediaReceived counts all incoming UDP media datagrams across restarts.
	mediaReceived atomic.Int64
	// fbHeld and fbBusy time how long decoding holds the framebuffer: the
	// CPU part of an update, apart from waiting on the network.
	fbHeld time.Time
	fbBusy time.Duration
	size   atomic.Uint64 // fb.W<<32 | fb.H, for Refresh from other goroutines
	dirty  []image.Rectangle
	zrle   zrleState
	tight  [4]zrleState
	a555   apple555State
	// continuous is true while the server pushes updates by itself
	// (ContinuousUpdates extension); the client then stops requesting.
	continuous      bool
	continuousKnown bool // the server announced support

	// cov tracks painted areas (reader goroutine); holes hands the
	// unpainted ones to a retry timer (see coverage.go).
	cov   coverage
	holes holes

	// native is the desktop size at ServerInit. With AppleScale, macOS
	// sends a scaled framebuffer but still takes pointer positions in
	// native pixels; ptrScaleX/Y (float64 bits, 0 = 1) convert them.
	native               atomic.Uint64 // w<<32 | h
	ptrScaleX, ptrScaleY atomic.Uint64
	tmp                  []byte

	// Apple dialect state (apple.go). sealer and wrapKey change under wmu
	// once, when the record layer starts.
	apple              bool // answered 003.889
	appleExt           bool // ServerInit carried the extension header
	appleFlags         uint32
	appleCmds          [16]byte
	wrapKey            [16]byte
	sealer             *recordSealer
	recordsOn          bool        // reader side; only the reader goroutine uses it
	pushing            atomic.Bool // the server pushes updates (message 9)
	setUp              bool        // the Apple setup messages went out
	layout             atomic.Pointer[DisplayLayout]
	layoutScale        atomic.Uint64 // float64 bits of the scale in DisplayLayout
	cursors            map[uint32]appleCursor
	media              atomic.Pointer[mediaState]
	mediaDiagnosticsMu sync.Mutex
	mediaDiagnostics   MediaDiagnostics
	appleEncs          []int32 // picture encodings sent in the Apple dialect
	pictureWatch       pictureWatch
	economy            rfbEconomy
	pictureFrames      atomic.Uint64
	closing            atomic.Bool
	closeOnce          sync.Once
	closeErr           error
}

// Dial connects to addr ("host:port"; port 5900 if omitted), authenticates
// and completes initialisation. Call Run to start receiving updates.
func Dial(ctx context.Context, addr string, cfg Config) (*Client, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "5900")
	}
	var d Dialer = systemDialer{}
	if cfg.Dialer != nil {
		d = cfg.Dialer
	}
	progress := newHandshakeProgress(&Client{cfg: cfg}, "tcp-connect")
	conn, err := d.DialContext(ctx, "tcp", addr)
	err = progress.finish(err)
	if err != nil {
		return nil, err
	}
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(addr)
	}
	c, err := NewClient(ctx, conn, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// NewClient runs the handshake over an established connection.
func NewClient(ctx context.Context, conn net.Conn, cfg Config) (*Client, error) {
	if cfg.Encodings == nil {
		cfg.Encodings = DefaultEncodings
	}
	if cfg.OnAudio != nil {
		cfg.Encodings = append(append([]int32(nil), cfg.Encodings...), EncPseudoQEMUAudio)
	}
	cfg.Encodings = append(append([]int32(nil), cfg.Encodings...), vncfeedback.Encoding)
	decodeContext, stopDecode := context.WithCancel(context.Background())
	c := &Client{conn: conn, cfg: cfg, authContext: ctx, decodeContext: decodeContext, stopDecode: stopDecode}
	c.r = bufio.NewReaderSize(&countingReader{r: conn, n: &c.received, rec: cfg.Record}, 256<<10)
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	c.prepareH264()
	if err := c.handshake(); err != nil {
		stopDecode()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
		d.State, d.AudioState = "RFB", "Unavailable in RFB picture mode"
		if cfg.MediaStream {
			d.State, d.AudioState = "Waiting for media negotiation", "Waiting for server offer"
		}
	})
	if cfg.Diagnostics {
		output := audio.CurrentOutput()
		c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.Output = output })
	}
	c.publishMediaDiagnostics(true)
	return c, nil
}

func (c *Client) handshake() (err error) {
	progress := newHandshakeProgress(c, "server-greeting")
	defer func() { err = progress.finish(err) }()
	// Version. macOS answers "RFB 003.889"; treat anything ≥ 3.8 as 3.8.
	var ver [12]byte
	if _, err := io.ReadFull(c.r, ver[:]); err != nil {
		return err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return protoErr("unexpected greeting %q", ver[:])
	}
	switch {
	case minor == 889 && c.cfg.Apple && c.cfg.Username != "":
		c.apple = true
		minor = 8
	case minor >= 8:
		minor = 8
	case minor == 7:
	default:
		minor = 3
	}
	c.Version = fmt.Sprintf("003.%03d", minor)
	if c.apple {
		c.Version = "003.889"
	}
	progress.next("version-reply")
	if _, err := fmt.Fprintf(c.conn, "RFB %s\n", c.Version); err != nil {
		return err
	}

	// Security.
	progress.next("security-offer")
	var offered []uint8
	if minor == 3 {
		var t [4]byte
		if _, err := io.ReadFull(c.r, t[:]); err != nil {
			return err
		}
		offered = []uint8{uint8(be.Uint32(t[:]))}
	} else {
		n, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		offered = make([]uint8, n)
		if _, err := io.ReadFull(c.r, offered); err != nil {
			return err
		}
	}
	if len(offered) == 0 || offered[0] == SecInvalid {
		reason, _ := c.readReason()
		return protoErr("server refused connection: %s", reason)
	}
	sec, err := c.chooseSecurity(offered)
	if err != nil {
		return err
	}
	progress.next("security-selection")
	// SRP sends the selection and initial identity together. Some macOS
	// servers discard a standalone SRP selector without advancing auth state.
	if minor != 3 && sec != SecAppleSRP {
		if _, err := c.conn.Write([]byte{sec}); err != nil {
			return err
		}
	}
	rw := struct {
		io.Reader
		io.Writer
	}{c.r, c.conn}
	switch sec {
	case SecNone:
	case SecVNC:
		progress.next("vnc-authentication")
		if err := c.passwordAuth(); err != nil {
			return err
		}
	case SecVeNCrypt:
		progress.next("vencrypt-authentication")
		if err := c.vencryptAuth(); err != nil {
			return err
		}
	case SecRA2, SecRA256:
		progress.next("rsa-aes-authentication")
		if err := c.rsaAESAuth(sec == SecRA256); err != nil {
			return err
		}
	case SecARD:
		progress.next("ard-authentication")
		key, err := ardAuth(rw, c.cfg.Username, c.cfg.Password)
		if err != nil {
			return err
		}
		copy(c.wrapKey[:], key)
	case SecAppleSRP:
		progress.next("srp-authentication")
		key, err := srpAuth(c.r, c.conn, c.cfg.Username, c.cfg.Password, minor != 3)
		if err != nil {
			return err
		}
		copy(c.wrapKey[:], key)
	}
	c.trace("security type %d", sec)
	// SecurityResult: always after authentication, and after None from 3.8.
	if sec != SecNone || minor == 8 {
		progress.next("security-result")
		var res [4]byte
		if _, err := io.ReadFull(c.r, res[:]); err != nil {
			return err
		}
		if be.Uint32(res[:]) != 0 {
			reason := ""
			if minor == 8 {
				reason, _ = c.readReason()
			}
			return &AuthError{Reason: reason}
		}
	}

	// ClientInit / ServerInit. Apple's client adds 0x80 for the extended
	// ServerInit and 0x40 for session selection.
	ci := boolByte(!c.cfg.Exclusive)
	progress.next("client-init")
	if c.apple {
		ci |= 0xc0
	}
	if _, err := c.conn.Write([]byte{ci}); err != nil {
		return err
	}
	var si [24]byte
	progress.next("server-init")
	if _, err := io.ReadFull(c.r, si[:]); err != nil {
		return err
	}
	c.resizeFB(int(be.Uint16(si[0:])), int(be.Uint16(si[2:])))
	c.native.Store(c.size.Load())
	c.Server = parsePixelFormat(si[4:20])
	progress.next("desktop-name")
	name, err := c.readString(be.Uint32(si[20:]))
	if err != nil {
		return err
	}
	c.Name = name
	if c.apple {
		c.Name = c.appleServerInit(name)
		c.trace("apple server flags=%#x commands=% x ext=%v key=%v", c.appleFlags, c.appleCmds, c.appleExt, c.wrapKey != [16]byte{})
		if c.appleExt && c.wrapKey != [16]byte{} {
			if c.appleFlags&appleFlagSessionSelect != 0 {
				progress.next("session-selection")
				if err := c.selectSession(); err != nil {
					return err
				}
			}
			progress.next("encrypted-setup")
			return c.appleStart()
		}
		c.apple = false // plain RFB from here on
	}

	progress.next("pixel-format")
	if err := c.setPixelFormat(rgbaFormat); err != nil {
		return err
	}
	progress.next("encodings")
	return c.setEncodings(c.cfg.Encodings)
}

// chooseSecurity prefers encrypted authentication, preserving Apple SRP/ARD
// for Apple sessions. A saved identity prevents an unverified downgrade.
func (c *Client) chooseSecurity(offered []uint8) (uint8, error) {
	prefs := []uint8{SecRA256, SecRA2, SecVeNCrypt, SecVNC, SecNone}
	switch {
	case c.cfg.Security != nil:
		prefs = c.cfg.Security
	case c.apple:
		prefs = []uint8{SecAppleSRP, SecARD} // SRP: what current macOS prefers
	case c.cfg.Username != "":
		prefs = []uint8{SecRA256, SecRA2, SecVeNCrypt, SecARD, SecVNC, SecNone}
	}
	for _, p := range prefs {
		if c.cfg.ServerFingerprint != "" && p != SecRA256 && p != SecRA2 && p != SecVeNCrypt {
			continue
		}
		if slices.Contains(offered, p) {
			return p, nil
		}
	}
	if c.cfg.Username == "" && slices.Contains(offered, SecARD) {
		return 0, &AuthError{Reason: "server requires a username (macOS account)"}
	}
	return 0, protoErr("no supported security type in %v", offered)
}

func (c *Client) readReason() (string, error) {
	var n [4]byte
	if _, err := io.ReadFull(c.r, n[:]); err != nil {
		return "", err
	}
	return c.readString(be.Uint32(n[:]))
}

func (c *Client) readString(n uint32) (string, error) {
	if n > 1<<20 {
		return "", protoErr("string of %d bytes", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(c.r, b)
	return string(b), err
}

// resizeFB changes the framebuffer size. Only the reader goroutine (or the
// handshake) calls it.
func (c *Client) resizeFB(w, h int) {
	c.clearH264Picture()
	c.closeH264Contexts()
	c.lockFB()
	defer c.unlockFB()
	c.fb.Resize(w, h)
	c.cov.reset(w, h)
	c.size.Store(uint64(w)<<32 | uint64(h))
	c.wrote(c.fb.Bounds()) // resize clears the pixels
}

// wrote reports pixels in r changed; the caller holds the framebuffer lock.
func (c *Client) wrote(r image.Rectangle) {
	if c.cfg.OnPixels != nil && !r.Empty() {
		c.cfg.OnPixels(&c.fb, r)
	}
}

func (c *Client) lockFB() {
	if c.cfg.FramebufferLock != nil {
		c.cfg.FramebufferLock.Lock()
	}
	if c.cfg.Trace != nil {
		c.fbHeld = time.Now()
	}
}

func (c *Client) unlockFB() {
	if c.cfg.Trace != nil {
		c.fbBusy += time.Since(c.fbHeld)
	}
	if c.cfg.FramebufferLock != nil {
		c.cfg.FramebufferLock.Unlock()
	}
}

func (c *Client) trace(format string, args ...any) {
	if c.cfg.Trace != nil {
		fmt.Fprintf(c.cfg.Trace, time.Now().Format("15:04:05.000 ")+format+"\n", args...)
	}
}

// Width and Height of the remote desktop as of the last processed update.
// Only meaningful from callbacks or before Run.
func (c *Client) Width() int  { return c.fb.W }
func (c *Client) Height() int { return c.fb.H }

// Close ends the connection; Run returns.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		if c.stopDecode != nil {
			c.stopDecode()
		}
		// Lifting modifiers is best effort, never an unbounded write that
		// keeps an old socket/media receiver alive while reconnecting.
		c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		c.stopMedia("closed", false)
		c.releaseKeys()
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// releaseKeys lifts every key still pressed, so closing the connection
// mid-chord leaves no modifier held on the server.
func (c *Client) releaseKeys() {
	c.wmu.Lock()
	held := make([]uint32, 0, len(c.held))
	for k := range c.held {
		held = append(held, k)
	}
	c.wmu.Unlock()
	for _, k := range held {
		c.KeyEvent(k, false)
	}
}

// Run requests updates and processes server messages until the connection
// fails or is closed. It returns nil after Close.
func (c *Client) Run() (err error) {
	started := time.Now()
	defer func() {
		stage, status := "session-run", "ended"
		if c.pictureFrames.Load() == 0 {
			stage = "first-picture"
		}
		if err != nil {
			status = "failed"
		}
		elapsed := time.Since(started)
		c.connectionLog(stage, status, elapsed, elapsed, err)
	}()
	defer c.Close() // also tear down media receivers after EOF/read failures
	defer c.zrle.stop()
	defer c.closeH264Contexts()
	defer c.clearH264Picture()
	defer func() {
		for i := range c.tight {
			c.tight[i].stop()
		}
	}()
	if c.cfg.OnResize != nil {
		c.cfg.OnResize(c.fb.W, c.fb.H)
	}
	startup := newHandshakeProgress(c, "stream-startup")
	if err := startup.finish(c.start()); err != nil {
		return c.runErr(err)
	}
	stopWatch := c.startPictureWatch()
	defer stopWatch()
	for {
		t, err := c.readMessage()
		if err == nil && t == 0 && !c.continuous && !c.pushing.Load() {
			err = c.requestUpdate(true)
		}
		if err != nil {
			return c.runErr(err)
		}
	}
}

// start sends the first update requests. In the Apple dialect it first
// waits for the record layer, which the server sets up in an update.
func (c *Client) start() error {
	if c.apple {
		for !c.recordsOn {
			if _, err := c.readMessage(); err != nil {
				return err
			}
		}
		if err := c.appleFirstMessages(); err != nil {
			return err
		}
		c.setUp = true
	} else {
		if c.cfg.AppleScale > 0 {
			if err := c.appleScale(c.cfg.AppleScale); err != nil {
				return err
			}
		}
		if err := c.requestUpdate(false); err != nil {
			return err
		}
	}
	if c.pushing.Load() {
		return nil
	}
	// Keep more requests in flight, so the server can start the next
	// update without waiting a round trip for our request.
	inFlight := c.cfg.InFlight
	if inFlight <= 0 {
		inFlight = 4
	}
	for range inFlight - 1 {
		if err := c.requestUpdate(true); err != nil {
			return err
		}
	}
	return nil
}

// readMessage reads and handles one server message, returning its type.
func (c *Client) readMessage() (byte, error) {
	t, err := c.r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch t {
	case 0:
		err = c.framebufferUpdate()
	case 1:
		err = c.colourMapEntries()
	case 2:
		if c.cfg.OnBell != nil {
			c.cfg.OnBell()
		}
	case 3:
		err = c.serverCutText()
	case 255:
		err = c.readAudio()
	case 150:
		err = c.endOfContinuousUpdates()
	default:
		if c.apple {
			var ok bool
			if ok, err = c.appleMessage(t); ok {
				break
			}
		}
		if b, _ := c.r.Peek(48); len(b) > 0 {
			c.trace("unknown server message %d, next bytes % x", t, b)
		}
		err = protoErr("unknown server message %d", t)
	}
	return t, err
}

func (c *Client) runErr(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (c *Client) framebufferUpdate() error {
	c.pictureMu.Lock()
	defer c.pictureMu.Unlock()
	var hdr [3]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return err
	}
	n := int(be.Uint16(hdr[1:]))
	start, rx := time.Now(), c.received.Load()
	c.fbBusy = 0
	c.dirty = c.dirty[:0]
	c.h264PictureDirty = false
	resized := false
	layoutChanged := false
	for i := 0; n == 0xFFFF || i < n; i++ {
		var rh [12]byte
		if _, err := io.ReadFull(c.r, rh[:]); err != nil {
			return err
		}
		r := image.Rect(0, 0, int(be.Uint16(rh[4:])), int(be.Uint16(rh[6:]))).
			Add(image.Pt(int(be.Uint16(rh[0:])), int(be.Uint16(rh[2:]))))
		enc := int32(be.Uint32(rh[8:]))
		layoutChanged = layoutChanged || c.apple && enc == EncAppleDisplayLayout
		c.trace("rect enc=%d %v", enc, r)
		if c.apple {
			ok, rs, err := c.applePseudo(enc, r)
			if err != nil {
				return err
			}
			if ok {
				resized = resized || rs
				continue
			}
		}
		switch enc {
		case dynamicResolutionEncoding:
			c.dynamicResolution.Store(true)
			continue
		case vncfeedback.Encoding:
			c.feedback.enabled = true
			continue
		case EncPseudoQEMUAudio:
			if err := c.enableAudio(); err != nil {
				return err
			}
			continue
		case EncPseudoCursor:
			// x, y is the hotspot, not a framebuffer position.
			if err := c.decodeCursor(r.Size(), r.Min); err != nil {
				return err
			}
			continue
		case EncPseudoLastRect:
			n = 0
			continue
		case EncPseudoDesktopSize:
			if r.Size() != c.fb.Bounds().Size() {
				c.resizeFB(r.Dx(), r.Dy())
				resized = true
			}
			continue
		case EncPseudoExtendedDesktopSize:
			if err := c.readExtendedDesktopSize(r); err != nil {
				return err
			}
			// Only reason 1 defines a result code; other reasons ignore y.
			if (r.Min.X != 1 || r.Min.Y == 0) && r.Size() != c.fb.Bounds().Size() {
				c.resizeFB(r.Dx(), r.Dy())
				resized = true
			}
			continue
		}
		if !r.In(c.fb.Bounds()) {
			return protoErr("rectangle %v outside %dx%d framebuffer", r, c.fb.W, c.fb.H)
		}
		if enc == EncH264 || enc == EncMirageHEVC {
			if c.hevcActive != (enc == EncMirageHEVC) {
				c.closeH264Contexts()
				c.hevcActive = enc == EncMirageHEVC
			}
			changed, err := c.decodeH264(r)
			if err != nil {
				return err
			}
			if changed {
				c.encoding.Store(int64(enc) + 1)
				c.dirty = append(c.dirty, r)
				c.cov.mark(r)
			}
			continue
		}
		if err := c.materializeH264(); err != nil {
			return err
		}
		var err error
		switch enc {
		case EncRaw:
			err = c.decodeRaw(r)
		case EncCopyRect:
			err = c.decodeCopyRect(r)
		case EncZRLE:
			err = c.decodeZRLE(r)
		case EncTight:
			err = c.decodeTight(r)
		case EncAppleRGB555:
			err = c.decodeApple555(r)
		default:
			err = protoErr("unsupported encoding %d", enc)
		}
		if err != nil {
			return err
		}
		if !r.Empty() {
			if enc != EncCopyRect {
				c.encoding.Store(int64(enc) + 1)
			}
			c.dirty = append(c.dirty, r)
			c.cov.mark(r)
		}
	}
	if resized {
		if c.cfg.OnResize != nil {
			c.cfg.OnResize(c.fb.W, c.fb.H)
		}
		c.updatePointerScale()
		// A new size invalidates everything the viewer holds.
		c.dirty = append(c.dirty[:0], c.fb.Bounds())
	}
	// Before the Apple setup is sent, the requests it makes cover the new
	// size already.
	// A Mac agent/display transition disarms cursor updates even when the
	// framebuffer dimensions stay the same. Polling mode also needs an
	// explicit AutoFramebufferUpdate to restore the cursor SELECT stream.
	if (resized || layoutChanged) && (!c.apple || c.setUp) {
		if err := c.requestUpdate(false); err != nil {
			return err
		}
		if layoutChanged && !c.pushing.Load() {
			if err := c.rearmAutoUpdate(); err != nil {
				return err
			}
		}
		if c.continuous {
			if err := c.enableContinuous(true); err != nil {
				return err
			}
		}
	}
	if c.cfg.Trace != nil {
		area := 0
		for _, r := range c.dirty {
			area += r.Dx() * r.Dy()
		}
		// Bytes are counted as the buffered reader pulls them from the
		// socket, so a single update can be off by up to its buffer size.
		c.trace("update rects=%d area=%d bytes=%d took=%v decode=%v", len(c.dirty), area,
			c.received.Load()-rx, time.Since(start).Round(time.Microsecond), c.fbBusy.Round(time.Microsecond))
	}
	c.noteCoverage(resized)
	if err := c.checkPushPace(time.Since(start)); err != nil {
		return err
	}
	if c.h264PictureDirty && c.cfg.OnVideoFrame != nil {
		c.cfg.OnVideoFrame(c.h264Picture)
	} else if len(c.dirty) > 0 && c.cfg.OnUpdate != nil {
		c.fb.Timing = frame.Timing{}
		c.cfg.OnUpdate(&c.fb, c.dirty)
	}
	if len(c.dirty) > 0 {
		c.pictureFrames.Add(1)
		c.feedback.submitted.Add(1)
		c.pictureWatch.frame(time.Now())
	}
	return c.reportVideoFeedback(time.Now())
}

// endOfContinuousUpdates handles message 150. The first one announces that
// the server supports ContinuousUpdates, so the client turns them on; a
// later one means the server stopped, so the client requests updates again.
func (c *Client) endOfContinuousUpdates() error {
	if !c.continuousKnown {
		c.continuousKnown = true
		c.trace("server supports continuous updates")
		c.continuous = true
		return c.enableContinuous(true)
	}
	c.trace("continuous updates ended")
	c.continuous = false
	return c.requestUpdate(true)
}

// updatePointerScale sets the factors from framebuffer to native pixels
// after a resize. Only server-side scaling makes them differ from 1: a
// resize the server makes by itself changes the native size too.
func (c *Client) updatePointerScale() {
	if c.cfg.AppleScale <= 0 || c.fb.W == 0 || c.fb.H == 0 {
		c.native.Store(c.size.Load())
		c.ptrScaleX.Store(0)
		c.ptrScaleY.Store(0)
		return
	}
	native := c.native.Load()
	sx, sy := float64(native>>32)/float64(c.fb.W), float64(native&0xFFFFFFFF)/float64(c.fb.H)
	c.trace("pointer scale %.3f x %.3f (native %dx%d)", sx, sy, native>>32, native&0xFFFFFFFF)
	c.ptrScaleX.Store(math.Float64bits(sx))
	c.ptrScaleY.Store(math.Float64bits(sy))
}

// appleScale sends ScaleFactor: type 0x08, one pad byte, big-endian f64.
func (c *Client) appleScale(scale float64) error {
	c.trace("apple scale %v", scale)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	b := c.wbuf[:10]
	b[0], b[1] = 0x08, 0
	be.PutUint64(b[2:], math.Float64bits(scale))
	return c.send(b)
}

func (c *Client) enableContinuous(on bool) error {
	size := c.size.Load()
	c.wmu.Lock()
	defer c.wmu.Unlock()
	b := c.wbuf[:10]
	b[0], b[1] = 150, boolByte(on)
	be.PutUint32(b[2:], 0)
	be.PutUint16(b[6:], uint16(size>>32))
	be.PutUint16(b[8:], uint16(size))
	return c.send(b)
}

func (c *Client) colourMapEntries() error {
	var h [5]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	_, err := c.r.Discard(int(be.Uint16(h[3:])) * 6)
	return err
}

func (c *Client) serverCutText() error {
	var h [7]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	n := be.Uint32(h[3:])
	if int32(n) < 0 { // Extended clipboard; not negotiated, skip it.
		_, err := c.r.Discard(int(-int32(n)))
		return err
	}
	text, err := c.readString(n)
	if err != nil {
		return err
	}
	if c.cfg.OnCutText != nil && !c.RichClipboard() {
		c.cfg.OnCutText(latin1ToUTF8(text))
	}
	return nil
}

func latin1ToUTF8(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			var b bytes.Buffer
			for j := 0; j < len(s); j++ {
				b.WriteRune(rune(s[j]))
			}
			return b.String()
		}
	}
	return s
}

// --- client-to-server messages ---

// send writes one or more messages, as records once the Apple record
// layer is on. Callers hold wmu, except during the handshake.
func (c *Client) send(b []byte) error {
	if c.sealer != nil {
		b = c.sealer.seal(b)
	}
	_, err := c.conn.Write(b)
	return err
}

// inputBlocked reports whether input must wait: in the Apple dialect
// nothing goes out between the handshake and the record layer. wmu held.
func (c *Client) inputBlocked() bool { return c.apple && c.sealer == nil }

func (c *Client) setPixelFormat(pf PixelFormat) error {
	var b [20]byte
	pf.marshal(b[4:])
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(b[:])
}

func (c *Client) setEncodings(encs []int32) error {
	b := make([]byte, 4+4*len(encs))
	b[0] = 2
	be.PutUint16(b[2:], uint16(len(encs)))
	for i, e := range encs {
		be.PutUint32(b[4+4*i:], uint32(e))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(b)
}

// Refresh asks the server to resend the whole screen. Safe from any
// goroutine.
func (c *Client) Refresh() error {
	if err := c.requestUpdate(false); err != nil {
		return err
	}
	if m := c.media.Load(); m != nil {
		if receiver := m.refreshVideo.Load(); receiver != nil {
			receiver.requestKeyframe()
		}
	}
	return nil
}

// requestUpdate asks for the whole screen; incremental asks only for
// changes.
func (c *Client) requestUpdate(incremental bool) error {
	size := c.size.Load()
	return c.requestRect(incremental, image.Rect(0, 0, int(size>>32), int(size&0xFFFFFFFF)))
}

func (c *Client) requestRect(incremental bool, r image.Rectangle) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.trace("request incremental=%v %v", incremental, r)
	b := c.wbuf[:10]
	b[0], b[1] = 3, boolByte(incremental)
	be.PutUint16(b[2:], uint16(r.Min.X))
	be.PutUint16(b[4:], uint16(r.Min.Y))
	be.PutUint16(b[6:], uint16(r.Dx()))
	be.PutUint16(b[8:], uint16(r.Dy()))
	if c.pushing.Load() {
		return c.send(c.appendArm(b))
	}
	return c.send(b)
}

// Key is one ordered press or release in a keyboard input sequence.
type Key struct {
	Keysym uint32
	Down   bool
}

// KeyEvent presses or releases an X11 keysym (see keysym.go).
func (c *Client) KeyEvent(keysym uint32, down bool) error {
	return c.KeyEvents([]Key{{Keysym: keysym, Down: down}})
}

// KeyEvents sends an already available sequence without a batching delay.
// Bound each write so a long paste cannot monopolize the connection lock.
func (c *Client) KeyEvents(keys []Key) error {
	for len(keys) > 0 {
		n := min(len(keys), 128)
		// macOS Screen Sharing can ignore a record containing multiple
		// keyboard messages. Preserve one message per record/write, as in
		// KeyEvent; batching remains safe for standard VNC's byte stream.
		if c.apple {
			n = 1
		}
		if err := c.sendKeys(keys[:n]); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

func (c *Client) sendKeys(keys []Key) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() {
		return nil
	}
	if c.held == nil {
		c.held = map[uint32]bool{}
	}
	c.keybuf = slices.Grow(c.keybuf[:0], len(keys)*18)
	message := c.keybuf
	released := false
	for _, key := range keys {
		sym := key.Keysym
		if c.apple {
			sym = appleKeysym(sym)
		}
		if key.Down {
			c.held[sym] = true
		} else {
			delete(c.held, sym)
			released = true
		}
		if c.apple && c.appleAccepts(msgApplePointer) && !c.cfg.PlainKeys {
			m, err := c.appleKeyMessage(sym, key.Down)
			if err != nil {
				return err
			}
			message = append(message, m...)
		} else {
			message = append(message, 4, boolByte(key.Down), 0, 0)
			message = be.AppendUint32(message, sym)
		}
	}
	err := c.send(message)
	if err == nil && released {
		c.pictureWatch.input(time.Now())
	}
	return err
}

// PointerEvent moves the pointer to (x, y) with the given button mask.
// Coordinates are framebuffer pixels; under AppleScale they are converted
// to the server's native pixels, aiming at the middle of the pixel.
func (c *Client) PointerEvent(buttons uint8, x, y int) error {
	picturePos := image.Pt(x, y)
	const wheel = ButtonWheelUp | ButtonWheelDown | ButtonWheelLeft | ButtonWheelRight
	if ax, ay, ok := c.applePointer(x, y); ok {
		x, y = ax, ay
	} else {
		native := c.native.Load()
		if sx := math.Float64frombits(c.ptrScaleX.Load()); sx > 0 {
			x = min(int((float64(x)+0.5)*sx), int(native>>32)-1)
		}
		if sy := math.Float64frombits(c.ptrScaleY.Load()); sy > 0 {
			y = min(int((float64(y)+0.5)*sy), int(native&0xFFFFFFFF)-1)
		}
	}
	if c.apple {
		// macOS reads mask bits as CGMouseButton numbers: right is 1,
		// middle 2. It scrolls only for a mask of exactly one wheel bit.
		b := buttons &^ (ButtonMiddle | ButtonRight)
		if buttons&ButtonMiddle != 0 {
			b |= ButtonRight
		}
		if buttons&ButtonRight != 0 {
			b |= ButtonMiddle
		}
		if buttons&wheel != 0 {
			b = buttons & wheel
		}
		buttons = b
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() {
		return nil
	}
	if buttons != c.lastButtons {
		size := c.size.Load()
		inputtrace.Printf("rfb pointer picture=%v desktop=%v buttons=%#x apple=%v encrypted=%v pictureSize=%dx%d layoutScale=%v layout=%+v", picturePos, image.Pt(x, y), buttons, c.apple, c.apple && c.appleAccepts(msgApplePointer), size>>32, size&0xffffffff, math.Float64frombits(c.layoutScale.Load()), c.layout.Load())
	}
	if buttons&wheel != 0 {
		c.trace("wheel %#x at %d,%d", buttons, x, y)
	}
	// Keep motion, dragging and button transitions on Apple's encrypted
	// input path. Mixing plain motion with encrypted clicks bypasses the
	// server's cursor tracking for the motion events.
	if c.apple && c.appleAccepts(msgApplePointer) {
		c.lastButtons = buttons
		m, err := c.encryptedPointer(buttons, x, y)
		if err != nil {
			return err
		}
		return c.send(m)
	}
	c.lastButtons = buttons
	b := c.wbuf[:6]
	b[0], b[1] = 5, buttons
	be.PutUint16(b[2:], uint16(max(x, 0)))
	be.PutUint16(b[4:], uint16(max(y, 0)))
	return c.send(b)
}

// CutText sends clipboard text. Classic RFB carries Latin-1 only; other
// characters become '?'.
func (c *Client) CutText(text string) error {
	if c.RichClipboard() {
		return c.SetClipboard(clip.Content{MIME: clip.Text, Data: []byte(text)})
	}
	b := make([]byte, 8, 8+len(text))
	b[0] = 6
	for _, r := range text {
		if r > 0xFF {
			r = '?'
		}
		b = append(b, byte(r))
	}
	be.PutUint32(b[4:], uint32(len(b)-8))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() {
		return nil
	}
	return c.send(b)
}

// appleKeysym maps keys for macOS, which reads Meta as Option.
func appleKeysym(k uint32) uint32 {
	switch k {
	case 0xffe9: // Alt_L
		return 0xffe7 // Meta_L
	case 0xffea: // Alt_R
		return 0xffe8 // Meta_R
	}
	return k
}

type countingReader struct {
	r   io.Reader
	n   *atomic.Int64
	rec io.Writer
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	if c.rec != nil && n > 0 {
		c.rec.Write(p[:n]) // recording is best effort
	}
	return n, err
}
