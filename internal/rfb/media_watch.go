package rfb

import (
	"fmt"
	"sync/atomic"
	"time"
)

// The media stream fails quietly: over the internet the server's UDP
// datagrams may never pass a NAT or firewall, or stop after a while, and
// the server sends no RFB pictures meanwhile. mediaWatch notices a stream
// that shows nothing and sends the client back to RFB pictures.

const (
	// mediaStallProbe is how long the picture may stand still before the
	// client asks for one keyframe, in case the last picture was lost.
	mediaStallProbe = 3 * time.Second
	// mediaStallLimit is how long without a decoded picture before the
	// stream counts as lost: when video packets keep coming but decode to
	// nothing, or when nothing at all comes, not even the sender reports,
	// for mediaSilence. A still or sleeping display sends no video but
	// keeps sending reports (macOS then sends Apple status 5), and keyframe
	// requests every second would cost a slow link Mbit/s.
	mediaStallLimit = 10 * time.Second
	mediaSilence    = 5 * time.Second
)

// mediaTiming holds the waits tests shorten: wait is how long the client
// waits for the media ports, and then for the first decoded picture,
// before going back to RFB pictures; tick paces the watch.
type mediaTiming struct{ wait, tick time.Duration }

var mediaTimes atomic.Pointer[mediaTiming]

func mediaTimings() mediaTiming {
	if t := mediaTimes.Load(); t != nil {
		return *t
	}
	return mediaTiming{wait: 8 * time.Second, tick: 500 * time.Millisecond}
}

type mediaVerdict int

const (
	mediaOK mediaVerdict = iota
	mediaProbe
	mediaFallback
)

// Off the local network the offer asks for remoteDesktop mode under a bit
// rate cap: macOS keeps under the cap but does not adapt below it to a
// slower link, and with no cap sends tens of Mbit/s. Measured through a
// shaped relay: under a 4 Mbit/s link, Nearby mode with no cap decoded one
// picture in 20 s; remoteDesktop at 3 Mbit/s gave 46 frames/s with no
// loss. When packets go missing the client renegotiates one step lower.
var remoteBitrates = []uint64{3_000_000, 1_800_000, 1_000_000, 600_000}

// lowerBitrate returns the next step below cap, or 0 at the bottom.
// An uncapped stream must also back off if a tunnel was mistaken for LAN.
func lowerBitrate(cap uint64) uint64 {
	for _, b := range remoteBitrates {
		if cap == 0 || b < cap {
			return b
		}
	}
	return 0
}

// firstMediaOffer chooses negotiation mode separately from bitrate. A local
// viewer can request RemoteDesktop mode without inheriting the WAN cap.
func (c *Client) firstMediaOffer() *mediaOffer {
	codecs := []int{payloadHEVC, payloadH264}
	local := c.onLocalNetwork()
	if local && !c.cfg.MediaRemoteDesktop && c.cfg.MediaMaxBitrate == 0 {
		return newMediaOffer(negotiatorNearby, codecs, true)
	}
	o := newMediaOffer(negotiatorRemoteDesktop, codecs, true)
	if !local {
		o.maxBitrate = remoteBitrates[0]
	}
	if limit := c.cfg.MediaMaxBitrate; limit > 0 {
		if local {
			o.maxBitrate = min(limit, maxVideoBitrate)
		} else {
			o.maxBitrate = min(limit, o.maxBitrate)
		}
	}
	return o
}

const (
	// congestionWindow is how long a loss rate is measured over, after
	// congestionGrace lets the first keyframe's burst pass.
	congestionWindow = 2 * time.Second
	congestionGrace  = time.Second
	congestionLoss   = 0.10
	congestionMin    = 50 // packets in the window before judging
	congestionDelay  = 200 * time.Millisecond
)

type lossSample struct {
	at            time.Time
	packets, lost int64
	delay         time.Duration
}

// mediaWatch tracks decoded access units against time. It is not safe for
// concurrent use; one goroutine checks it.
type mediaWatch struct {
	start      time.Time // the video leg opened
	wait       time.Duration
	units      int64 // decoded access units at the last check
	lastUnitAt time.Time
	probed     bool // since the last picture
	packets    int64
	lastRTPAt  time.Time
	rtcp       int64
	lastRTCPAt time.Time
	samples    []lossSample // the last congestionWindow of counters
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// congested detects sustained loss or queue growth. A reliable VPN can queue
// video for seconds without dropping enough packets to trigger loss control.
func (w *mediaWatch) congested(now time.Time, packets, lost int64, delay time.Duration) bool {
	if now.Sub(w.start) < congestionGrace {
		return false
	}
	w.samples = append(w.samples, lossSample{now, packets, lost, delay})
	for len(w.samples) > 1 && now.Sub(w.samples[1].at) >= congestionWindow {
		w.samples = w.samples[1:]
	}
	first := w.samples[0]
	if now.Sub(first.at) < congestionWindow {
		return false
	}
	got, missing := packets-first.packets, lost-first.lost
	if got+missing < congestionMin {
		return false
	}
	if float64(missing) > congestionLoss*float64(got+missing) {
		return true
	}
	for _, sample := range w.samples {
		if sample.delay < congestionDelay {
			return false
		}
	}
	return true
}

// queueLag reads the existing RTP timestamp/arrival estimator. Do not reuse a
// stale estimate after the desktop stops producing video.
func (l *mediaLeg) queueLag(now time.Time) time.Duration {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	if !l.rate.have || now.Sub(l.rate.sampleAt) > time.Second {
		return 0
	}
	return time.Duration(max(l.rate.delay.value, 0) * float64(time.Second))
}

func newMediaWatch(now time.Time) *mediaWatch {
	return &mediaWatch{start: now, wait: mediaTimings().wait}
}

// check takes the decoded access unit count and says what to do.
func (w *mediaWatch) check(now time.Time, units, packets, rtcp int64) mediaVerdict {
	if packets > w.packets {
		w.packets, w.lastRTPAt = packets, now
	}
	if rtcp > w.rtcp {
		w.rtcp, w.lastRTCPAt = rtcp, now
	}
	if units > w.units {
		w.units, w.lastUnitAt, w.probed = units, now, false
		return mediaOK
	}
	if w.units == 0 {
		if now.Sub(w.start) >= w.wait {
			return mediaFallback
		}
		return mediaOK // the decoder asks for keyframes by itself meanwhile
	}
	idle := now.Sub(w.lastUnitAt)
	if idle >= mediaStallLimit {
		undecodable := now.Sub(w.lastRTPAt) < mediaSilence // video still comes
		silent := now.Sub(latest(w.lastRTPAt, w.lastRTCPAt)) >= mediaSilence
		if undecodable || silent {
			return mediaFallback
		}
	}
	if idle >= mediaStallProbe && !w.probed {
		w.probed = true
		return mediaProbe
	}
	return mediaOK
}

// legCounts are a video leg's transport counters at one moment.
type legCounts struct {
	packets, bad, rtcp, refused, sendErrs int64
}

func (l *mediaLeg) counts() legCounts {
	return legCounts{l.packets.Load(), l.bad.Load(), l.rtcpIn.Load(), l.refused.Load(), l.sendErrs.Load()}
}

// mediaFailure names why a stream showed nothing, from the leg's counters,
// which it appends: they tell a tunnel's MTU from a closed port.
func mediaFailure(units int64, l legCounts) string {
	var why string
	switch {
	case units > 0:
		why = "video stalled"
	case l.packets == 0 && l.bad == 0 && l.rtcp > 0:
		why = "no picture: only control packets arrived; video packets may exceed the VPN tunnel MTU"
	case l.packets == 0 && l.bad == 0 && l.refused > 0:
		why = "no picture: the Mac refused UDP on its media port"
	case l.packets == 0 && l.bad == 0 && l.sendErrs > 0:
		why = "no picture: could not send UDP to the Mac"
	case l.packets == 0 && l.bad == 0:
		why = "no picture: no UDP video arrived (blocked by NAT, firewall or VPN?)"
	case l.packets == 0:
		why = "no picture: UDP video could not be decrypted"
	default:
		why = "no picture: video arrived but none decoded"
	}
	return fmt.Sprintf("%s; rtp %d, rtcp %d, refused %d, send errors %d", why, l.packets, l.rtcp, l.refused, l.sendErrs)
}

// watchMedia runs until the stream stops, probing a still picture and
// falling back to RFB pictures when the stream shows nothing.
func (c *Client) watchMedia(m *mediaState, leg *mediaLeg) {
	w := newMediaWatch(time.Now())
	if !m.hadPicture && !m.firstPictureBy.IsZero() {
		w.wait = m.firstPictureBy.Sub(w.start)
	}
	t := time.NewTicker(mediaTimings().tick)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-t.C:
			if c.media.Load() != m {
				return
			}
			units := m.units.Load()
			// An undecodable stream is not necessarily a slow network. Bound
			// the blank startup across all offers, including congestion retries.
			if units == 0 && !m.hadPicture && !now.Before(m.firstPictureBy) {
				go c.stopMedia(mediaFailure(0, leg.counts()), true)
				return
			}
			if w.congested(now, leg.packets.Load(), m.lost.Load(), leg.queueLag(now)) && c.reduceMediaBitrate(m, "network congestion") {
				return
			}
			switch w.check(now, units, leg.packets.Load(), leg.rtcpIn.Load()) {
			case mediaProbe:
				leg.requestKeyframe()
			case mediaFallback:
				// A previously working stream may recover at a lower rate.
				// Never keep an entirely undecodable startup black through the ladder.
				if (units > 0 || m.hadPicture) && leg.packets.Load() >= congestionMin && c.reduceMediaBitrate(m, "video stalled") {
					return
				}
				go c.stopMedia(mediaFailure(m.units.Load(), leg.counts()), true)
				return
			}
		}
	}
}

func (c *Client) reduceMediaBitrate(m *mediaState, why string) bool {
	next := lowerBitrate(m.offer.maxBitrate)
	if next == 0 || c.media.Load() != m || !m.restarting.CompareAndSwap(false, true) {
		return false
	}
	c.trace("media %s: reducing bitrate from %d to %d bit/s", why, m.offer.maxBitrate, next)
	go c.restartMedia(m, m.portAudio, m.portVideo, m.audioOn, next)
	return true
}

// watchNegotiation gives the server the media wait to name its ports; without
// them no video leg opens and no watchMedia runs.
func (c *Client) watchNegotiation(m *mediaState) {
	time.AfterFunc(mediaTimings().wait, func() {
		if c.media.Load() == m && m.opened.CompareAndSwap(false, true) {
			c.stopMedia("no media ports from the server", true)
		}
	})
}
