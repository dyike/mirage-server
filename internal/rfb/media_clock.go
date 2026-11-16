package rfb

import (
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

// A sender report maps RTP to the peer's clock. Its report block also echoes
// our SR token and reports how long it was held (RFC 3550, LSR/DLSR). Together
// those timestamps bound the clock offset without assuming synchronized UTC.
type clockSample struct {
	peer, local, arrived time.Time
	error                time.Duration
}

type mediaClock struct {
	rtp              uint32
	wall, at         time.Time
	samples          [16]clockSample
	next, consistent int
}

func ntpTime(ntp uint64) time.Time {
	return time.Unix(int64(ntp>>32)-2208988800, int64(uint64(uint32(ntp))*1_000_000_000>>32)).UTC()
}

func (c *mediaClock) report(ntp uint64, rtp uint32, now time.Time) {
	if ntp == 0 {
		*c = mediaClock{}
		return
	}
	wall := ntpTime(ntp)
	if !c.at.IsZero() {
		drift := wall.Sub(c.wall) - now.Sub(c.at)
		if drift < -250*time.Millisecond || drift > 250*time.Millisecond {
			c.samples, c.consistent = [16]clockSample{}, 0
		}
	}
	c.rtp, c.wall, c.at = rtp, wall, now
}

func (c *mediaClock) calibrate(ntp uint64, sent, arrived time.Time, hold uint32) bool {
	if ntp == 0 || sent.IsZero() {
		return false
	}
	elapsed := arrived.Sub(sent)
	held := time.Duration(uint64(hold) * uint64(time.Second) / 65536)
	rtt := elapsed - held
	// A negative RTT is an invalid echo, not a zero-delay path. Very old
	// exchanges are too imprecise for frame timing and may have wrapped tokens.
	if rtt < 0 || elapsed > 20*time.Second || rtt > time.Second {
		return false
	}
	sample := clockSample{peer: ntpTime(ntp), local: arrived.Add(-rtt / 2), arrived: arrived, error: rtt/2 + time.Millisecond}
	if previous, ok := c.best(arrived); ok {
		residual := sample.local.Sub(previous.local.Add(sample.peer.Sub(previous.peer)))
		limit := sample.error + previous.error + arrived.Sub(previous.arrived)/10000
		if residual < -limit || residual > limit {
			c.samples, c.consistent = [16]clockSample{}, 0
		}
	} else {
		c.consistent = 0
	}
	c.samples[c.next] = sample
	c.next = (c.next + 1) % len(c.samples)
	c.consistent++
	return true
}

func (c *mediaClock) best(now time.Time) (clockSample, bool) {
	var best clockSample
	for _, sample := range c.samples {
		age := now.Sub(sample.arrived)
		if sample.arrived.IsZero() || age < 0 || age > 10*time.Second {
			continue
		}
		sample.error += age / 10000 // allow 100 ppm clock drift between reports
		if best.arrived.IsZero() || sample.error < best.error {
			best = sample
		}
	}
	return best, !best.arrived.IsZero()
}

func (c *mediaClock) source(ts uint32, now time.Time) frame.Timing {
	if c.at.IsZero() || now.Sub(c.at) < 0 || now.Sub(c.at) > 10*time.Second {
		return frame.Timing{}
	}
	delta := time.Duration(int64(int32(ts-c.rtp))) * time.Second / videoClock
	if delta < -10*time.Second || delta > 10*time.Second {
		return frame.Timing{}
	}
	source := c.wall.Add(delta)
	best, ok := c.best(now)
	if !ok || c.consistent < 2 {
		return frame.Timing{Source: source}
	}
	return frame.Timing{Source: best.local.Add(source.Sub(best.peer)), ClockError: best.error + time.Second/videoClock, ClockSynced: true}
}

func (l *mediaLeg) sourceTime(ssrc, ts uint32, now time.Time) frame.Timing {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	if source := l.sources[ssrc]; source != nil {
		return source.clock.source(ts, now)
	}
	return frame.Timing{}
}
