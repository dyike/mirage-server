package server

import (
	"context"
	"crypto/des"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"io"
	"log"
	"math/bits"
	"net"
	"sync"
	"time"

	"github.com/dyike/mirage-server/internal/server/input"
)

var networkOrder = binary.BigEndian

const rfbH264 uint32 = 50
const rfbHEVC uint32 = 0x4d485643

type InputSession interface {
	Apply(input.Event) error
	LEDs() uint32
	Close() error
}

// RFBServer serves standard VNC over VeNCrypt TLS and, when explicitly enabled,
// the existing Mirage Mac client over ARD authentication and Apple records.
type RFBServer struct {
	TLS *tls.Config
	// ARD enables the existing Mirage Mac client dialect and SRTP video.
	ARD                    bool
	SRPCredential          func() (*SRPCredential, error) // When set, require type 36; never offer legacy DH.
	ARDUDPPort             int                            // first video port; zero allocates per-session ephemeral ports
	Password               string
	Authenticate           func(context.Context, string, string) error
	Username               string
	Width, Height          int
	PreferHEVC             bool
	VideoBitrate, VideoFPS int
	MaxClients             int
	RefreshInputOnResize   bool
	OpenInput              func() (InputSession, error)
	OpenClipboard          func() (Clipboard, error)
	OpenCursor             func() (Cursor, error)
	ARDAudio               func(context.Context, func([]byte) error) error
	Audio                  func(context.Context, AudioFormat, func([]byte) error) error
	EmbeddedCursor         bool
	Screens                []DesktopScreen
	// AvailableScreens lists selectable native outputs; Screens is the initial capture.
	AvailableScreens []DesktopScreen
	DisplayNames     map[uint32]string
	DesktopSource    func(context.Context, CaptureRequest, func([]byte) error) error
	SetDesktop       func(context.Context, DesktopLayout) error
	// Source must restart with an independent picture on each invocation.
	// raw frames are tightly packed RGBA; video frames are Annex-B access units.
	Source  func(context.Context, string, func([]byte) error) error
	OnError func(error)
}

func (s *RFBServer) Serve(ctx context.Context, l net.Listener) error {
	defer l.Close()
	if s.SRPCredential != nil && !s.ARD {
		return errors.New("SRP requires Apple dialect")
	}
	if s.ARD && (s.ARDUDPPort < 0 || s.ARDUDPPort > 65471 || (s.SRPCredential == nil && s.Authenticate == nil && (s.Username == "" || len(s.Username) > 63 || len(s.Password) > 63))) {
		return errors.New("ARD requires an account (credentials <=63 bytes) and UDP base port 0..65471")
	}
	if s.TLS == nil || len(s.TLS.Certificates) == 0 || (s.SRPCredential == nil && s.Authenticate == nil && (len(s.Password) < 1 || (s.Username == "" && len(s.Password) > 8) || len(s.Password) > 256 || len(s.Username) > 256)) || (s.Source == nil && s.DesktopSource == nil) || s.Width < 1 || s.Height < 1 || s.Width > 8192 || s.Height > 8192 {
		return errors.New("RFB requires certificate, password (1–8 bytes for VNC, up to 256 with username), dimensions and source")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	go func() { <-ctx.Done(); l.Close() }()
	limit := s.MaxClients
	if limit == 0 {
		limit = 4
	}
	if limit < 1 || limit > 64 {
		return errors.New("max clients must be 1..64")
	}
	slots := make(chan struct{}, limit+4)
	active := &rfbClients{limit: limit, clients: map[net.Conn]func(){}}
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cancel()
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer c.Close()
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-ctx.Done():
					c.Close()
				case <-done:
				}
			}()
			if e := s.handleRFB(ctx, c, active); e != nil && ctx.Err() == nil && s.OnError != nil {
				s.OnError(e)
			}
		}()
	}
}
func writeBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func readBytes(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	_, e := io.ReadFull(r, b)
	return b, e
}
func u32(v uint32) []byte { b := make([]byte, 4); networkOrder.PutUint32(b, v); return b }
func (s *RFBServer) authenticate(ctx context.Context, c net.Conn) (net.Conn, error) {
	c.SetDeadline(time.Now().Add(15 * time.Second))
	greeting := "RFB 003.008\n"
	if s.ARD {
		greeting = "RFB 003.889\n"
	}
	if e := writeBytes(c, []byte(greeting)); e != nil {
		return nil, e
	}
	b, e := readBytes(c, 12)
	if e != nil {
		return nil, e
	}
	if s.ARD && s.SRPCredential != nil && (string(b) == "RFB 003.889\n" || string(b) == "RFB 003.008\n") {
		return s.authenticateSRP(c)
	}
	if s.ARD && string(b) == "RFB 003.889\n" {
		return s.authenticateARD(ctx, c)
	}
	if string(b) != "RFB 003.008\n" {
		return nil, errors.New("RFB 3.8 required")
	}
	if e = writeBytes(c, []byte{1, 19}); e != nil {
		return nil, e
	}
	b, e = readBytes(c, 1)
	if e != nil {
		return nil, e
	}
	if b[0] != 19 {
		return nil, errors.New("VeNCrypt required")
	}
	if e = writeBytes(c, []byte{0, 2}); e != nil {
		return nil, e
	}
	b, e = readBytes(c, 2)
	if e != nil {
		return nil, e
	}
	if b[0] != 0 || b[1] != 2 {
		return nil, errors.New("VeNCrypt 0.2 required")
	}
	subtype := uint32(261)
	if s.Username != "" || s.Authenticate != nil {
		subtype = 262
	}
	if e = writeBytes(c, append([]byte{0, 1}, u32(subtype)...)); e != nil {
		return nil, e
	}
	b, e = readBytes(c, 4)
	if e != nil {
		return nil, e
	}
	if networkOrder.Uint32(b) != subtype {
		return nil, errors.New("selected VeNCrypt subtype was not offered")
	}
	if e = writeBytes(c, []byte{1}); e != nil {
		return nil, e
	}
	cfg := s.TLS.Clone()
	cfg.MinVersion = max(cfg.MinVersion, tls.VersionTLS12)
	secured := tls.Server(c, cfg)
	if e = secured.HandshakeContext(ctx); e != nil {
		return nil, e
	}
	valid := false
	if subtype == 262 {
		lengths, err := readBytes(secured, 8)
		if err != nil {
			return nil, err
		}
		un, pn := networkOrder.Uint32(lengths), networkOrder.Uint32(lengths[4:])
		if un > 256 || pn > 256 {
			return nil, errors.New("credentials too long")
		}
		credentials, err := readBytes(secured, int(un+pn))
		if err != nil {
			return nil, err
		}
		if s.Authenticate != nil {
			valid = s.Authenticate(ctx, string(credentials[:un]), string(credentials[un:])) == nil
			clear(credentials)
		} else {
			gotUser, wantUser := sha256.Sum256(credentials[:un]), sha256.Sum256([]byte(s.Username))
			gotPassword, wantPassword := sha256.Sum256(credentials[un:]), sha256.Sum256([]byte(s.Password))
			clear(credentials)
			valid = subtle.ConstantTimeCompare(gotUser[:], wantUser[:])&subtle.ConstantTimeCompare(gotPassword[:], wantPassword[:]) == 1
		}
	} else {
		challenge := make([]byte, 16)
		if _, e = rand.Read(challenge); e != nil {
			return nil, e
		}
		if e = writeBytes(secured, challenge); e != nil {
			return nil, e
		}
		response, e := readBytes(secured, 16)
		if e != nil {
			return nil, e
		}
		var key [8]byte
		copy(key[:], s.Password)
		for i := range key {
			key[i] = bits.Reverse8(key[i])
		}
		block, e := des.NewCipher(key[:])
		clear(key[:])
		if e != nil {
			return nil, e
		}
		expected := make([]byte, 16)
		block.Encrypt(expected[:8], challenge[:8])
		block.Encrypt(expected[8:], challenge[8:])
		valid = subtle.ConstantTimeCompare(response, expected) == 1
		clear(response)
		clear(expected)

	}
	if !valid {
		reason := []byte("authentication failed")
		writeBytes(secured, append(append(u32(1), u32(uint32(len(reason)))...), reason...))
		return nil, errors.New("authentication failed")
	}
	if e = writeBytes(secured, u32(0)); e != nil {
		return nil, e
	}
	return secured, nil
}

type rfbFormat [16]byte

var defaultRFBFormat = rfbFormat{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16}

func (p rfbFormat) validate() error {
	if (p[0] != 8 && p[0] != 16 && p[0] != 32) || p[1] == 0 || p[1] > p[0] || p[2] > 1 || p[3] != 1 {
		return errors.New("unsupported RFB pixel format")
	}
	var used uint64
	for i := 0; i < 3; i++ {
		m := uint64(networkOrder.Uint16(p[4+2*i:]))
		shift := p[10+i]
		mask := m << shift
		if m == 0 || m&(m+1) != 0 || shift >= p[0] || mask>>p[0] != 0 || used&mask != 0 {
			return errors.New("invalid RFB color mask")
		}
		used |= mask
	}
	if bits.OnesCount64(used) > int(p[1]) {
		return errors.New("pixel depth smaller than color masks")
	}
	return nil
}
func (p rfbFormat) pack(rgba []byte, w, h int) ([]byte, error) {
	if len(rgba) != w*h*4 {
		return nil, errors.New("invalid raw capture size")
	}
	n := int(p[0]) / 8
	out := make([]byte, w*h*n)
	for i := 0; i < w*h; i++ {
		var v uint32
		for j := 0; j < 3; j++ {
			m := uint32(networkOrder.Uint16(p[4+2*j:]))
			v |= ((uint32(rgba[4*i+j])*m + 127) / 255) << p[10+j]
		}
		for j := 0; j < n; j++ {
			shift := j
			if p[2] != 0 {
				shift = n - 1 - j
			}
			out[i*n+j] = byte(v >> (shift * 8))
		}
	}
	return out, nil
}

type rfbState struct {
	feedback         vncfeedback.Report
	feedbackAt       time.Time
	feedbackSequence uint64
	sync.Mutex
	captureCancel      context.CancelFunc
	ardReady           chan struct{}
	ardAwaitingRefresh bool
	ardLayoutRevision  uint64
	ardScaleBase       DesktopLayout // unscaled canvas for Apple scale requests and pointer coordinates
	ardScale           float64
	resizeEncoding     uint32
	encoding           uint32
	format             rfbFormat
	continuous         bool
	desktop            DesktopLayout
	revision           uint64
	layoutPending      bool
	presented          DesktopLayout
	cursorImage        input.CursorImage
	cursorActive       bool
}

var renegotiate = errors.New("RFB encoding changed")

func (s *RFBServer) handleRFB(ctx context.Context, raw net.Conn, active *rfbClients) (result error) {
	c, err := s.authenticate(ctx, raw)
	if err != nil {
		return err
	}
	initFlag, err := readBytes(c, 1)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !active.add(raw, func() { cancel(); raw.Close() }, initFlag[0]&1 != 0) {
		return errors.New("maximum clients reached")
	}
	defer active.remove(raw)
	var controls InputSession
	if s.OpenInput != nil {
		controls, err = s.OpenInput()
		if err != nil {
			return err
		}
		defer controls.Close()
	}
	init := make([]byte, 24)
	networkOrder.PutUint16(init, uint16(s.Width))
	networkOrder.PutUint16(init[2:], uint16(s.Height))
	copy(init[4:], defaultRFBFormat[:])
	name := []byte("Mirage Go Desktop")
	ard, isARD := c.(*ardConn)
	if isARD {
		name = ardName(name)
	}
	networkOrder.PutUint32(init[20:], uint32(len(name)))
	if err = writeBytes(c, append(init, name...)); err != nil {
		return err
	}
	if isARD {
		if err = ard.startRecords(); err != nil {
			return err
		}
	}
	c.SetDeadline(time.Time{})
	state := &rfbState{format: defaultRFBFormat, desktop: s.desktop(), presented: s.desktop()}
	requests := make(chan struct{}, 1)
	peer := &rfbPeer{server: s, ctx: ctx, cancel: cancel, conn: c, state: state, controls: controls, requests: requests, advertised: map[uint32]bool{}, audioFormat: AudioFormat{3, 2, 48000}}
	if isARD {
		peer.ard = &ardPeer{conn: ard}
		defer peer.ard.close()
	}
	if s.OpenClipboard != nil {
		peer.clipboard, err = s.OpenClipboard()
		if err != nil {
			return err
		}
	}
	peer.watchClipboard()
	defer func() {
		cancel()
		peer.stopAudio()
		if peer.cursor != nil {
			peer.cursor.Close()
		}
		if peer.clipboard != nil {
			peer.clipboard.Close()
		}
		peer.wg.Wait()
	}()
	readerDone := make(chan error, 1)
	go func() { e := s.readRFB(c, state, requests, peer); readerDone <- e; cancel() }()
	defer func() {
		cancel()
		raw.Close()
		readErr := <-readerDone
		if errors.Is(result, context.Canceled) && readErr != nil {
			result = readErr
		}
	}()
	linkBudget := videoTransport(raw)
	rate := newRateController(s.VideoBitrate, s.VideoFPS)
	pixels := new(pixelEncoder)
	defer pixels.close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-requests:
		}
		state.Lock()
		enc := state.encoding
		desktop, revision := state.desktop.clone(), state.revision
		state.Unlock()
		var media *ardMedia
		if enc == ardMediaEncoding && peer.ard != nil {
			media = peer.ard.media.Load()
			if media == nil {
				continue
			}
		}
		codec := "raw"
		if enc == rfbH264 {
			codec = "h264"
		} else if enc == rfbHEVC {
			codec = "hevc"
		}
		if media != nil {
			codec = media.codec
			rate.current = media.rate()
			media.configureFrameRate(rate.fps)
			if apple, ok := media.payloader.(*ardHEVCPayloader); ok {
				apple.width = uint16(desktop.Size.X)
				apple.height = uint16(desktop.Size.Y)
			}
		}
		minQP := 0
		if media != nil {
			minQP = 30
			if media.rate() <= 3000 {
				minQP = 34
			}
			if media.rate() >= 12000 {
				minQP = 24
			}
		}
		first := true
		var setBitrate func(int)
		var forceKeyframe func() bool
		var setFPS func(int)
		captureFPS := rate.fps
		var feedbackSeen uint64
		delivery := newClientController(rate.fps)
		source := func(ctx context.Context, send func([]byte) error) error {
			if s.DesktopSource != nil {
				return s.DesktopSource(ctx, CaptureRequest{MinQP: minQP, RepeatIdle: media != nil, Codec: codec, Desktop: desktop, Bitrate: rate.current, FPS: rate.fps, BindKeyframe: func(f func() bool) { forceKeyframe = f }, BindBitrate: func(update func(int)) { setBitrate = update }, BindFPS: func(update func(int)) { setFPS = update }}, send)
			}
			return s.Source(ctx, codec, send)
		}
		captureCtx, cancelCapture := context.WithCancel(ctx)
		state.Lock()
		state.captureCancel = cancelCapture
		if state.revision != revision {
			cancelCapture()
		}
		state.Unlock()
		err = source(captureCtx, func(data []byte) error {
			state.Lock()
			continuous := state.continuous
			ready := state.ardReady
			state.Unlock()
			if first && peer.ard != nil && media == nil && ready != nil {
				select {
				case <-captureCtx.Done():
					return captureCtx.Err()
				case <-ready:
				}
			}
			if !first && !continuous && media == nil {
				select {
				case <-captureCtx.Done():
					return captureCtx.Err()
				case <-requests:
				}
			}
			state.Lock()
			current, pf, currentRevision := state.encoding, state.format, state.revision
			feedback, feedbackAt, feedbackSequence := state.feedback, state.feedbackAt, state.feedbackSequence
			pending := state.layoutPending
			resizeEncoding := state.resizeEncoding
			// Apple DisplayLayout already performs the scale resize. Sending a
			// second DesktopSize treats it as a new desktop and loses that transform.
			if peer.ard != nil && state.ardReady != nil {
				resizeEncoding = 0
			}
			if current == enc && currentRevision == revision {
				state.layoutPending = false
				if pending {
					state.presented = desktop.clone()
				}
			}
			state.Unlock()
			if feedbackSequence != feedbackSeen && time.Since(feedbackAt) < 3*time.Second {
				feedbackSeen = feedbackSequence
				if setFPS != nil && delivery.observe(time.Now(), feedback) {
					setFPS(delivery.current)
					log.Printf("RFB client adaptive fps=%d decode=%dus drawn=%d/%d pending=%d", delivery.current, feedback.DecodeUS, feedback.Presented, feedback.Frames, feedback.Pending)
				}
			}
			if current != enc || currentRevision != revision {
				log.Printf("RFB capture refresh: peer=%s encoding=%d->%d revision=%d->%d", c.RemoteAddr(), enc, current, revision, currentRevision)
				select {
				case requests <- struct{}{}:
				default:
				}
				return renegotiate
			}
			if pending {
				if rectangle := desktopNotification(desktop, resizeEncoding); rectangle != nil {
					if err := peer.send(updateMessage(rectangle)); err != nil {
						return err
					}
				}
				if err := peer.refreshCursor(); err != nil {
					return err
				}
			}
			if media != nil {
				if next := media.fps(); setFPS != nil && next > 0 && next != captureFPS {
					setFPS(next)
					captureFPS = next
				}
				if next := media.rate(); next != rate.current {
					rate.current = next
					if setBitrate != nil {
						setBitrate(next)
					} else {
						peer.wakeCapture()
						return renegotiate
					}
				}
				err := media.send(ctx, data, first, forceKeyframe)
				first = false
				// A client key/bitrate renegotiation deliberately closes the
				// old socket. Do not turn that expected close into a refusal
				// of the replacement media session.
				if peer.ard.media.Load() != media {
					peer.wakeCapture()
					return renegotiate
				}
				if err != nil && !errors.Is(err, renegotiate) && ctx.Err() == nil {
					log.Printf("ARD media stopped: %v; requesting TCP fallback", err)
					peer.ard.media.CompareAndSwap(media, nil)
					media.close()
					err = peer.ardRefuse()
					if err == nil {
						err = renegotiate
					}
				}
				if errors.Is(err, renegotiate) {
					peer.wakeCapture()
				}
				return err
			}
			if enc == 0 || enc == encZlib || enc == encTight || enc == encZRLE {
				wasFirst := first
				start := time.Now()
				sizeBytes, err := peer.sendPixelFrame(pixels, enc, pf, data, desktop.Size, revision)
				if errors.Is(err, renegotiate) {
					peer.wakeCapture()
					return err
				}
				first = false
				if wasFirst {
					log.Printf("RFB first pixel frame: peer=%s encoding=%d dimensions=%dx%d bytes=%d send=%s error=%v", c.RemoteAddr(), enc, desktop.Size.X, desktop.Size.Y, sizeBytes, time.Since(start), err)
				}
				return err
			}
			if len(data) == 0 || len(data) > 32<<20 {
				return errors.New("invalid framebuffer packet")
			}
			header := make([]byte, 16)
			networkOrder.PutUint16(header[2:], 1)
			networkOrder.PutUint16(header[8:], uint16(desktop.Size.X))
			networkOrder.PutUint16(header[10:], uint16(desktop.Size.Y))
			networkOrder.PutUint32(header[12:], enc)
			if enc != 0 {
				header = append(header, u32(uint32(len(data)))...)
				flags := uint32(0)
				if first {
					flags = 2
				}
				header = append(header, u32(flags)...)
			}
			first = false
			start := time.Now()
			_, pacingRate := linkBudget(rate.current)
			paced, err := peer.sendPaced(pacingRate, header, data)
			if err != nil {
				return err
			}
			writeTime := max(0, time.Since(start)-paced)
			queueTime, _ := linkBudget(rate.current)
			if s.DesktopSource != nil && rate.observe(time.Now(), writeTime, queueTime) {
				log.Printf("RFB adaptive codec=%s bitrate=%d kbit/s write=%s queued=%s live=%v", codec, rate.current, writeTime, queueTime, setBitrate != nil)
				if setBitrate != nil {
					setBitrate(rate.current)
					return nil
				}
				select {
				case requests <- struct{}{}:
				default:
				}
				return renegotiate
			}
			return nil
		})
		cancelCapture()
		state.Lock()
		state.captureCancel = nil
		changed := state.revision != revision
		state.Unlock()
		if changed && ctx.Err() == nil {
			peer.wakeCapture()
			continue
		}

		if errors.Is(err, renegotiate) {
			continue
		}
		return err
	}
}
func (s *RFBServer) readRFB(c net.Conn, state *rfbState, requests chan struct{}, peer *rfbPeer) error {
	for {
		t, e := readBytes(c, 1)
		if e != nil {
			return e
		}
		if peer != nil && peer.ard != nil {
			handled, err := peer.readARD(t[0])
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}
		switch t[0] {
		case 0:
			b, e := readBytes(c, 19)
			if e != nil {
				return e
			}
			var pf rfbFormat
			copy(pf[:], b[3:])
			if e = pf.validate(); e != nil {
				return e
			}
			state.Lock()
			state.format = pf
			state.Unlock()
		case 2:
			b, e := readBytes(c, 3)
			if e != nil {
				return e
			}
			n := int(networkOrder.Uint16(b[1:]))
			if n > 4096 {
				return errors.New("too many encodings")
			}
			b, e = readBytes(c, n*4)
			if e != nil {
				return e
			}
			offers := map[uint32]bool{}
			for i := 0; i < n; i++ {
				offers[networkOrder.Uint32(b[4*i:])] = true
			}
			enc := uint32(0)
			for i := 0; i < n; i++ {
				v := networkOrder.Uint32(b[4*i:])
				if v == ardMediaEncoding && peer.ard != nil {
					enc = v
					break
				}
				if v == rfbHEVC && s.PreferHEVC {
					enc = v
					break
				}
				if v == rfbH264 || v == encZlib || v == encTight || v == encZRLE || v == 0 {
					enc = v
					break
				}
			}
			if offers[ardMediaEncoding] && peer.ard != nil && peer.ard.media.Load() != nil {
				enc = ardMediaEncoding
			}
			log.Printf("RFB encoding selection: peer=%s selected=0x%x offers=%v", c.RemoteAddr(), enc, offers)
			state.Lock()
			if state.encoding != enc {
				state.invalidateCaptureLocked()
			}
			state.encoding = enc
			state.resizeEncoding = 0
			if offers[0xfffffecc] {
				state.resizeEncoding = 0xfffffecc
			} else if offers[0xffffff21] {
				state.resizeEncoding = 0xffffff21
			}
			state.Unlock()
			if err := peer.announce(offers); err != nil {
				return err
			}
		case 3:
			b, e := readBytes(c, 9)
			if e != nil {
				return e
			}
			if peer != nil && peer.ard != nil {
				state.Lock()
				armed := state.armARDFrameLocked()
				state.Unlock()
				if armed {
					log.Printf("ARD viewer ready: peer=%s region=%dx%d incremental=%t", c.RemoteAddr(), networkOrder.Uint16(b[5:]), networkOrder.Uint16(b[7:]), b[0] != 0)
				}
			}
			// A non-incremental request must restore a decoder with no references.
			// Restart the video source so its first packet includes parameter sets,
			// an independent picture, and the RFB reset flag.
			if b[0] == 0 {
				state.Lock()
				if state.encoding == ardMediaEncoding && peer != nil && peer.ard != nil {
					if media := peer.ard.media.Load(); media != nil {
						media.keyframe.Store(true)
					}
				} else if state.encoding == rfbH264 || state.encoding == rfbHEVC {
					state.invalidateCaptureLocked()
				}
				state.Unlock()
			}
			select {
			case requests <- struct{}{}:
			default:
			}
		case 4:
			b, e := readBytes(c, 7)
			if e != nil {
				return e
			}
			if b[0] > 1 {
				return errors.New("invalid key state")
			}
			if err := peer.apply(input.Event{Type: "key", Symbol: networkOrder.Uint32(b[3:]), Down: b[0] == 1}); err != nil {
				return err
			}
		case 5:
			b, e := readBytes(c, 5)
			if e != nil {
				return e
			}
			mask := uint16(b[0])
			if peer.advertised[0xfffffec4] && mask&128 != 0 {
				extra, err := readBytes(c, 1)
				if err != nil {
					return err
				}
				mask = (mask & 127) | uint16(extra[0])<<7
			}
			if err := peer.pointer(mask&511, int(networkOrder.Uint16(b[1:])), int(networkOrder.Uint16(b[3:]))); err != nil {
				return err
			}
		case 6:
			if err := peer.cutText(); err != nil {
				return err
			}
		case 150:
			b, err := readBytes(c, 9)
			if err != nil {
				return err
			}
			if !peer.advertised[0xfffffec7] || b[0] > 1 {
				return errors.New("unnegotiated continuous updates")
			}
			state.Lock()
			state.continuous = b[0] == 1
			state.Unlock()
			if b[0] == 0 {
				if err := peer.send([]byte{150}); err != nil {
					return err
				}
			}
			select {
			case requests <- struct{}{}:
			default:
			}
		case 248:
			if err := peer.fence(); err != nil {
				return err
			}
		case 251:
			if err := peer.resize(); err != nil {
				return err
			}
		case vncfeedback.Message:
			if !peer.advertised[uint32(vncfeedback.Encoding)] {
				return errors.New("unnegotiated video feedback")
			}
			b, e := readBytes(c, vncfeedback.Size-1)
			if e != nil {
				return e
			}
			report, e := vncfeedback.Parse(append([]byte{vncfeedback.Message}, b...))
			if e != nil {
				return e
			}
			state.Lock()
			// Bound accepted reports even if a peer ignores the negotiated cadence.
			if time.Since(state.feedbackAt) >= 500*time.Millisecond {
				state.feedback = report
				state.feedbackAt = time.Now()
				state.feedbackSequence++
			}
			state.Unlock()
		case 255:
			if err := peer.qemu(); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unsupported RFB client message %d", t[0])
		}
	}
}

type rfbClients struct {
	sync.Mutex
	limit   int
	clients map[net.Conn]func()
}

func (a *rfbClients) add(c net.Conn, stop func(), shared bool) bool {
	a.Lock()
	defer a.Unlock()
	if !shared {
		for conn, cancel := range a.clients {
			cancel()
			delete(a.clients, conn)
		}
	}
	if len(a.clients) >= a.limit {
		return false
	}
	a.clients[c] = stop
	return true
}
func (a *rfbClients) remove(c net.Conn) { a.Lock(); delete(a.clients, c); a.Unlock() }
