package rfb

import (
	"math"
	"time"

	"github.com/pion/rtcp"
)

// Receiver feedback for the video leg. AVConference adapts its bitrate to
// two signals: receiver reports (loss, jitter, round trip through the
// sender report echo) once a second, and its own "RCTL" application packet
// every 50 ms, carrying the newest packet's timestamp, how long ago it
// arrived, a 1024 Hz clock, a queuing-delay estimate, the longest burst of
// lost packets and a received count. Layouts follow the open-screen-sharing
// project's notes on AVConference.

const (
	videoClock       = 24000 // RTP clock of the video leg
	feedbackInterval = 50 * time.Millisecond
)

// reception tracks one source for receiver reports (RFC 3550 A.3, A.8).
type reception struct {
	started                bool
	base, highest          uint32 // extended sequence numbers
	received               uint32
	expectedPrior          uint32
	receivedPrior          uint32
	jitter, transit        float64
	lastSR                 uint32 // middle 32 bits of the last SR's NTP time
	lastSRAt, lastPacketAt time.Time
	clock                  mediaClock
}

func (r *reception) record(seq uint16, ts uint32, now time.Time) {
	r.lastPacketAt = now
	transit := float64(now.UnixNano())/1e9*videoClock - float64(ts)
	if !r.started {
		r.started, r.base, r.highest, r.received, r.transit = true, uint32(seq), uint32(seq), 1, transit
		return
	}
	if d := seq - uint16(r.highest); d != 0 && d < 0x8000 {
		cycles := r.highest &^ 0xffff
		if seq < uint16(r.highest) {
			cycles += 1 << 16
		}
		r.highest = cycles | uint32(seq)
	}
	r.received++
	if d := math.Abs(transit - r.transit); d < videoClock*10 {
		r.jitter += (d - r.jitter) / 16
	}
	r.transit = transit
}

func (r *reception) report(ssrc uint32, now time.Time) rtcp.ReceptionReport {
	expected := r.highest - r.base + 1
	lost := int64(expected) - int64(r.received)
	lost = min(max(lost, -0x800000), 0x7fffff)
	ei, ri := expected-r.expectedPrior, r.received-r.receivedPrior
	r.expectedPrior, r.receivedPrior = expected, r.received
	var fraction uint8
	if ei > ri {
		fraction = uint8(uint64(ei-ri) * 256 / uint64(ei))
	}
	var delay uint32
	if !r.lastSRAt.IsZero() {
		delay = uint32(now.Sub(r.lastSRAt).Seconds() * 65536)
	}
	return rtcp.ReceptionReport{
		SSRC: ssrc, FractionLost: fraction, TotalLost: uint32(lost) & 0xffffff,
		LastSequenceNumber: r.highest, Jitter: uint32(r.jitter),
		LastSenderReport: r.lastSR, Delay: delay,
	}
}

// rateFeedback builds the RCTL packets.
type rateFeedback struct {
	epoch                    time.Time
	have, videoArrived       bool
	sampleTS                 uint32
	sampleAt                 time.Time
	sampleReceived, received uint32
	burstLoss                uint32
	highest                  uint16
	delay                    queueDelay
}

func (f *rateFeedback) record(seq uint16, ts uint32, now time.Time) {
	if f.received > 0 {
		if step := seq - f.highest; step != 0 && step < 0x7fff {
			f.burstLoss = max(f.burstLoss, uint32(step)-1)
			f.highest = seq
		}
	} else {
		f.highest = seq
	}
	if !f.have || f.sampleTS-ts > 0x7ffffffe { // a newer timestamp
		f.have, f.sampleTS, f.sampleAt, f.sampleReceived = true, ts, now, f.received
		f.delay.sample(float64(ts)/videoClock, now.Sub(f.epoch).Seconds())
	}
	f.received++
}

// packet returns the RCTL application packet, or nil before any video.
func (f *rateFeedback) packet(ssrc uint32, now time.Time) []byte {
	if !f.have || !f.videoArrived {
		return nil
	}
	p := make([]byte, 32)
	copy(p, []byte{0x80, 204, 0, 7}) // APP, 8 words
	be.PutUint32(p[4:], ssrc)
	copy(p[8:], "RCTL")
	copy(p[12:], []byte{0x85, 0, 0, 4})
	be.PutUint16(p[16:], uint16(f.sampleTS>>8)) // low-precision echo timestamp
	be.PutUint16(p[22:], uint16(min(max(now.Sub(f.sampleAt).Milliseconds(), 0), math.MaxUint16)))
	be.PutUint16(p[24:], uint16(now.Sub(f.epoch)*1024/time.Second)) // 1024 Hz
	be.PutUint16(p[26:], uint16(min(max(f.delay.value*8192, 0), math.MaxUint16)))
	be.PutUint16(p[28:], uint16(min(f.burstLoss, 15)<<12|f.sampleReceived&0xfff))
	f.burstLoss = 0
	return p
}

// queueDelay estimates how much the path's queues delay packets now,
// relative to its long-run delay: the one-way delay (arrival minus send
// time, up to a constant) smoothed fast and slow, and their difference. As
// in AVConference's estimator, a sample far from both averages is skipped,
// and many in a row restart the estimate.
type queueDelay struct {
	firstSend, firstRecv float64
	started              bool
	short, long, value   float64
	spurious             int
	lastSend             float64
}

func (q *queueDelay) sample(send, recv float64) {
	if q.started && send <= q.lastSend {
		return
	}
	q.lastSend = send
	if !q.started {
		q.started, q.firstSend, q.firstRecv = true, send, recv
	}
	lag := (recv - q.firstRecv) - (send - q.firstSend)
	if q.short == 0 && q.long == 0 {
		q.short, q.long = lag, lag
	}
	const spuriousLag = 30 // as AVConference's estimator, in its units (seconds)
	if lag-q.short > spuriousLag || q.long-lag > spuriousLag {
		if q.spurious++; q.spurious > 30 {
			*q = queueDelay{value: q.value}
		}
		return
	}
	q.spurious = 0
	q.long = 0.9999*q.long + 0.0001*lag
	q.short = 0.9*q.short + 0.1*lag
	q.value = q.short - q.long
	if q.value < 0 {
		q.long, q.value = q.short, 0
	}
}
