package rfb

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"

	"github.com/dyike/mirage-server/internal/audio"
	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/video"
)

// Client side of the high-performance mode's negotiation (see
// mediaoffer.go): ports in, offer out, answer or refusal in.

type mediaState struct {
	offer        *mediaOffer
	restarting   atomic.Bool // a goroutine renegotiates the stream
	portVideo    uint16
	portAudio    uint16
	video        *mediaLeg
	refreshVideo atomic.Pointer[mediaLeg]
	audio        *mediaLeg // nil without sound
	audioOn      bool      // the server enabled its audio port
	done         chan struct{}
	exited       chan struct{} // closed when the leg's reader returns
	units        atomic.Int64  // access units decoded
	// The first-picture deadline survives renegotiation. Lower bitrate offers
	// must not each buy another full wait while the viewer stays black.
	firstPictureBy time.Time
	hadPicture     bool         // an earlier leg decoded a picture
	submitted      atomic.Int64 // complete access units accepted by the decoder
	decoder        atomic.Pointer[video.Decoder]
	lost           atomic.Int64 // video packets the reorder buffer gave up on
	// opened is claimed by whoever acts on the ports first: the reader to
	// open the video leg, or watchNegotiation to give up on it.
	opened                   atomic.Bool
	audioPackets, audioBytes atomic.Int64
	player                   atomic.Pointer[audio.Player]
	once                     sync.Once
	// closeDecoder ends the decoder; call after the leg stopped.
	closeDecoder func()
}

// stopMedia ends the media stream. With resume, the client asks for RFB
// pictures again; Close passes false.
func (c *Client) stopMedia(why string, resume bool) {
	m := c.media.Load()
	if m == nil {
		return
	}
	m.once.Do(func() {
		c.trace("media stream stops: %s", why)
		c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
			d.State = "Stopped: " + why
			if resume {
				d.Fallback = why
			}
		})
		c.publishMediaDiagnostics(true)
		m.closeLegs()
		if !resume {
			return
		}
		if !c.media.CompareAndSwap(m, nil) {
			return // a restart replaced the stream
		}
		c.stopClipboard()
		c.economizeRFB()
		if err := c.setEncodings(append(c.appleEncs, appleMeta...)); err == nil {
			if err := c.requestUpdate(false); err == nil && !c.pushing.Load() {
				c.rearmAutoUpdate()
			}
		}
	})
}

// closeLegs ends the stream's legs and decoder, once they run. Never on the
// reader: the decoder may wait for a picture callback that waits for it.
func (m *mediaState) closeLegs() {
	if m.done == nil {
		return
	}
	m.refreshVideo.Store(nil)
	close(m.done)
	m.video.close()
	if m.audio != nil {
		m.audio.close()
	}
	<-m.exited
	if m.closeDecoder != nil {
		m.closeDecoder()
	}
}

// restartMedia renegotiates after the server named its ports again, which
// it does when it restarts the stream, as after a display change: it then
// waits for a new offer. It runs on its own goroutine.
func (c *Client) restartMedia(old *mediaState, audioPort, videoPort uint16, audioOn bool, maxBitrate uint64) {
	c.trace("media stream restarts, at most %d bit/s", maxBitrate)
	o := old.offer
	mode := o.mode
	if maxBitrate > 0 {
		mode = negotiatorRemoteDesktop
	}
	m := &mediaState{offer: newMediaOffer(mode, o.codecs, o.fps60), portAudio: audioPort, portVideo: videoPort, audioOn: audioOn}
	m.offer.maxBitrate = maxBitrate
	m.opened.Store(true)
	m.restarting.Store(true)
	defer m.restarting.Store(false)
	old.once.Do(old.closeLegs)
	m.firstPictureBy = old.firstPictureBy
	m.hadPicture = old.hadPicture || old.units.Load() > 0
	if !c.media.CompareAndSwap(old, m) {
		return // the stream stopped meanwhile
	}
	c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.State, d.MaxBitrate = "Renegotiating", maxBitrate })
	if err := c.offerMedia(m); err != nil {
		c.trace("media restart: %v", err)
	}
}

// offerMedia opens the video leg on the named ports and sends the offer.
func (c *Client) offerMedia(m *mediaState) error {
	if err := c.openVideo(m); err != nil {
		go c.stopMedia("video leg: "+err.Error(), true)
		return nil
	}
	msg := m.offer.message()
	c.trace("media offer mode %d, %d bytes", m.offer.mode, len(msg))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.send(msg)
}

// openVideo starts receiving the video leg.
func (c *Client) openVideo(m *mediaState) error {
	local := c.conn.LocalAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	remote := c.conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	leg, err := openMediaLeg(c.cfg.Dialer, local, remote, m.portVideo, m.offer.videoRecv[:], m.offer.videoSend[:], m.offer.videoSSRC)
	if err != nil {
		return err
	}
	m.video, m.done, m.exited = leg, make(chan struct{}), make(chan struct{})
	if m.firstPictureBy.IsZero() {
		m.firstPictureBy = time.Now().Add(mediaTimings().wait)
	}
	leg.measureClock = true
	leg.received = &c.mediaReceived
	c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.State = "Receiving video" })
	var (
		asm       *auAssembler
		dec       *video.Decoder
		lastAsk   time.Time
		decodeErr int
		submitted int64
	)
	askKeyframe := func() {
		if time.Since(lastAsk) >= 250*time.Millisecond {
			lastAsk = time.Now()
			leg.requestKeyframe()
		}
	}
	onVideo := func(h *rtp.Header, payload []byte) {
		if asm == nil {
			codec := video.HEVC
			switch h.PayloadType {
			case payloadHEVC:
			case payloadH264, 126:
				codec = video.H264
			default:
				return
			}
			d, err := video.NewDecoder(codec, func(f video.Frame) {
				c.showFrame(f)
				leg.arrived.Store(true)
				if n := m.units.Add(1); n <= 3 {
					c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
						d.State, d.VideoUnits = "Video active", n
					})
				}
			})
			if err != nil {
				c.trace("media decoder: %v", err)
				c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.VideoError = err.Error() })
				return
			}
			d.SetCPUFrames(c.cfg.OnVideoFrame == nil)
			asm, dec = &auAssembler{codec: codec}, d
			m.decoder.Store(d)
			c.trace("media video codec %v", codec)
		}
		au, lost := asm.push(h, payload)
		if lost {
			asm.synced = false
			askKeyframe()
		}
		if au == nil {
			return
		}
		if !asm.synced && !isKeyframe(asm.codec, au) {
			askKeyframe()
			return
		}
		var err error
		if timed, ok := any(dec).(interface {
			DecodeTimed([][]byte, frame.Timing) error
		}); ok {
			err = timed.DecodeTimed(au, leg.sourceTime(h.SSRC, h.Timestamp, time.Now()))
		} else {
			err = dec.Decode(au)
		}
		if err != nil {
			c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.VideoError = err.Error() })
			decodeErr++
			if decodeErr <= 5 {
				c.trace("media decode: %v", err)
			}
			asm.synced = false
			askKeyframe()
			return
		}
		asm.synced = true
		submitted++
		m.submitted.Add(1)
		if n := submitted; n <= 3 {
			status := dec.Status()
			c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.Video = status })
			c.trace("media submitted access unit %d (%d NAL units)", n, len(au))
		}
		// Parameter sets may reconfigure the decoder later in the session.
		if submitted%120 == 0 {
			status := dec.Status()
			c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.Video = status })
		}
	}
	var reorder reorderBuffer
	leg.readDeadline = reorder.deadline
	leg.onTimeout = func(now time.Time) {
		reorder.expire(now, onVideo)
		m.lost.Store(reorder.lost)
	}
	leg.onPacket = func(h *rtp.Header, payload []byte) {
		reorder.push(h, payload, time.Now(), onVideo)
		m.lost.Store(reorder.lost)
	}
	m.closeDecoder = func() {
		if dec != nil {
			dec.Close()
		}
	}
	audioExited := make(chan struct{})
	close(audioExited)
	if m.audioOn && !c.cfg.NoAudio {
		if a, err := c.openAudio(m, local, remote); err != nil {
			c.trace("media audio: %v", err)
			c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioState = "Initialization failed"; d.AudioError = err.Error() })
		} else {
			audioExited = a
		}
	}
	go func() {
		leg.run(m.done)
		<-audioExited
		close(m.exited)
	}()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		ticks := 0
		for {
			select {
			case <-m.done:
				return
			case <-t.C:
				ticks++
				var playback audio.PlaybackStatus
				if player := m.player.Load(); player != nil {
					playback = player.Status()
				}
				var output audio.OutputStatus
				if c.cfg.Diagnostics {
					output = audio.CurrentOutput()
				}
				var decoderStatus video.DecoderStatus
				if decoder := m.decoder.Load(); decoder != nil {
					decoderStatus = decoder.Status()
				}
				c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
					if decoderStatus.Backend != "" {
						d.Video = decoderStatus
					}
					d.VideoUnits, d.VideoPackets, d.VideoBad = m.units.Load(), leg.packets.Load(), leg.bad.Load()
					d.AudioPackets, d.AudioBytes = m.audioPackets.Load(), m.audioBytes.Load()
					d.Playback, d.Output = playback, output
					if m.audio != nil {
						d.AudioUDP, d.AudioBad = m.audio.packets.Load(), m.audio.bad.Load()
					}
				})
				c.publishMediaDiagnostics(ticks == 1 || ticks%5 == 0)
				c.trace("media video: %d packets, %d bytes, %d rtcp, %d undecryptable, %d refused, %d send errors, stopped: %v",
					leg.packets.Load(), leg.bytes.Load(), leg.rtcpIn.Load(), leg.bad.Load(), leg.refused.Load(), leg.sendErrs.Load(), leg.err.Load())
			}
		}
	}()
	go c.watchMedia(m, leg)
	m.refreshVideo.Store(leg)
	return nil
}

// openAudio starts the audio leg: AAC-ELD, 48 kHz stereo, 480-sample
// packets, played by AudioQueue. It returns a channel closed when the leg's
// reader and the player are done.
func (c *Client) openAudio(m *mediaState, local, remote netip.Addr) (chan struct{}, error) {
	leg, err := openMediaLeg(c.cfg.Dialer, local, remote, m.portAudio, m.offer.audioRecv[:], m.offer.audioSend[:], m.offer.audioSSRC)
	if err != nil {
		return nil, err
	}
	leg.received = &c.mediaReceived
	var player *audio.Player
	if !c.cfg.MuteAudio {
		player, err = audio.NewAACELD(audio.Format{SampleRate: 48000, Channels: 2, FramesPerPacket: 480,
			Cookie: audio.AACELDCookie(audio.RemoteDesktopELD)})
		if err != nil {
			leg.close()
			return nil, err
		}
	}
	m.audio = leg
	m.player.Store(player)
	c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioState = "Waiting for packets" })
	var seen int
	var audioFailed bool
	leg.onPacket = func(h *rtp.Header, p []byte) {
		if h.PayloadType != payloadAudio {
			return
		}
		count := m.audioPackets.Add(1)
		m.audioBytes.Add(int64(len(p)))
		if count == 1 {
			c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioState = "Receiving AAC-ELD" })
		}
		if seen < 3 {
			seen++
			c.trace("media audio rtp seq=%d ts=%d len=%d head % x", h.SequenceNumber, h.Timestamp, len(p), p[:min(len(p), 8)])
		}
		// An RFC 3640 AU header section (16 bits of headers: a 13-bit size
		// and a 3-bit index) precedes the frame in our own uplink format;
		// strip it when the size matches.
		if len(p) > 4 && be.Uint16(p) == 16 && int(be.Uint16(p[2:])>>3) == len(p)-4 {
			p = p[4:]
		}
		if player != nil {
			if err := player.Push(p); err != nil {
				audioFailed = true
				c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioError = err.Error() })
			} else if audioFailed {
				audioFailed = false
				c.updateMediaDiagnostics(func(d *MediaDiagnostics) { d.AudioError = "" })
			}
		}
	}
	exited := make(chan struct{})
	go func() {
		leg.run(m.done)
		if player != nil {
			player.Close()
		}
		close(exited)
	}()
	return exited, nil
}

// mediaReply handles a MediaStream pseudo-rectangle: a u16 length, then a
// message whose first u16 is its kind: 1 ports, 2 answer, 3 refusal.
func (c *Client) mediaReply() error {
	var n [2]byte
	if _, err := io.ReadFull(c.r, n[:]); err != nil {
		return err
	}
	body := make([]byte, be.Uint16(n[:]))
	if _, err := io.ReadFull(c.r, body); err != nil {
		return err
	}
	m := c.media.Load()
	if len(body) < 8 || m == nil {
		c.trace("media reply of %d bytes ignored", len(body))
		return nil
	}
	switch kind := be.Uint16(body); kind {
	case 1:
		if len(body) < 26 {
			return protoErr("media ports reply of %d bytes", len(body))
		}
		leg := func(off int) (uint16, uint32) { return be.Uint16(body[off:]), be.Uint32(body[off+2:]) }
		ap, af := leg(8)
		vp, vf := leg(14)
		v2p, v2f := leg(20)
		c.trace("media ports audio %d (%#x) video %d (%#x) video2 %d (%#x)", ap, af, vp, vf, v2p, v2f)
		audioOn := af&1 != 0 && ap != 0
		if !m.opened.CompareAndSwap(false, true) {
			if m.restarting.CompareAndSwap(false, true) {
				go c.restartMedia(m, ap, vp, audioOn, m.offer.maxBitrate)
			}
			return nil
		}
		m.portAudio, m.portVideo, m.audioOn = ap, vp, audioOn
		c.updateMediaDiagnostics(func(d *MediaDiagnostics) {
			d.AudioEnabled, d.AudioPort = m.audioOn, ap
			d.AudioState = "Not offered by server"
			if m.audioOn {
				d.AudioState = "Offered by server"
			}
			if c.cfg.NoAudio {
				d.AudioState = "Disabled by client"
			}
			if c.cfg.MuteAudio {
				d.AudioState = "Muted by client"
			}
		})
		return c.offerMedia(m)
	case 2:
		if a, err := parseMediaAnswer(body); err != nil {
			c.trace("media answer: %v", err)
		} else {
			// LTRP acknowledgments only apply when frames come in several
			// bands; this offer asks for one.
			c.trace("media answer: video %dx%d, %d band(s), LTRP %v, SSRC %#x", a.width, a.height, a.tiles, a.ltrp, a.ssrc)
		}
		if c.cfg.OnMediaAnswer != nil {
			c.cfg.OnMediaAnswer(body)
		}
	case 3:
		why := "refused"
		if len(body) >= 16 {
			why = fmt.Sprintf("refused: type %d sub-code %d", be.Uint32(body[8:]), be.Uint32(body[12:]))
			if be.Uint32(body[8:]) == 1 && be.Uint32(body[12:]) == 1 {
				why += " (busy: another viewer has the media stream)"
			}
		}
		go c.stopMedia(why, true)
	default:
		c.trace("media reply kind %d, %d bytes", kind, len(body))
	}
	return nil
}
