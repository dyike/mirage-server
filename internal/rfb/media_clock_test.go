package rfb

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

func TestMediaClockUsesSenderTimeAndRTPWrap(t *testing.T) {
	wall := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	arrival := wall.Add(40 * time.Millisecond)
	var c mediaClock
	if !c.source(12, arrival).Source.IsZero() {
		t.Fatal("RTP alone invented an absolute source time")
	}
	rtp := uint32(0xffffff00)
	c.report(ntp64(wall), rtp, arrival)
	if got := c.source(rtp+120, arrival); !got.Source.Equal(wall.Add(5*time.Millisecond)) || got.ClockSynced {
		t.Fatal("used report arrival instead of sender time", got)
	}
	wrapped := uint32(uint64(rtp) + videoClock)
	if got := c.source(wrapped, arrival.Add(time.Second)); !got.Source.Equal(wall.Add(time.Second)) || got.ClockSynced {
		t.Fatal("RTP wrap changed the source epoch", got)
	}
	if !c.source(wrapped, arrival.Add(11*time.Second)).Source.IsZero() {
		t.Fatal("stale sender report remained usable")
	}
	c.report(ntp64(wall.Add(2*time.Second)), wrapped, arrival.Add(time.Second))
	if c.source(wrapped, arrival.Add(time.Second)).ClockSynced {
		t.Fatal("sender clock jump remained usable")
	}
	c.report(ntp64(time.Unix(100, 0)), 1, arrival)
	if c.source(1, arrival).ClockSynced {
		t.Fatal("sender uptime was treated as UTC")
	}
}

func TestClockCalibrationRequiresMatchingAuthenticatedEcho(t *testing.T) {
	key := make([]byte, 46)
	peer, err := newSRTP(key)
	if err != nil {
		t.Fatal(err)
	}
	rx, err := newSRTP(key)
	if err != nil {
		t.Fatal(err)
	}
	l := &mediaLeg{rx: rx, ssrc: 7}
	base := time.Now()
	for i := range 2 {
		sent := base.Add(time.Duration(i) * time.Second)
		stamp := uint32(ntp64(sent) >> 16)
		l.clockSent[i] = struct {
			stamp uint32
			at    time.Time
		}{stamp, sent}
		for _, match := range []bool{false, true} {
			echo := stamp
			if !match {
				echo++
			}
			sr := &rtcp.SenderReport{SSRC: 9, NTPTime: ntp64(sent.Add(-2500 * time.Millisecond)), RTPTime: 1000 + uint32(i*videoClock), Reports: []rtcp.ReceptionReport{{SSRC: 7, LastSenderReport: echo}}}
			plain, err := sr.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			packet, err := peer.EncryptRTCP(nil, plain, nil)
			if err != nil {
				t.Fatal(err)
			}
			if match {
				bad := append([]byte(nil), packet...)
				bad[len(bad)-1] ^= 1
				l.senderReports(bad, sent.Add(10*time.Millisecond))
			}
			l.senderReports(packet, sent.Add(10*time.Millisecond))
			got := l.sources[9].clock.consistent
			want := i
			if match {
				want++
			}
			if got != want {
				t.Fatalf("matching=%v: accepted an uncorrelated/unauthenticated clock report: %d want %d", match, got, want)
			}
		}
	}
	if !l.sourceTime(9, 1000+videoClock, base.Add(time.Second+10*time.Millisecond)).ClockSynced {
		t.Fatal("matched authenticated reports did not calibrate")
	}
}

func TestMediaClockCalibratesOffsetWithMatchedRoundTrips(t *testing.T) {
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, skew := range []time.Duration{-2500 * time.Millisecond, 1300 * time.Millisecond, -time.Hour} {
		for _, forward := range []time.Duration{0, 8 * time.Millisecond, 20 * time.Millisecond} {
			var c mediaClock
			const held = 200 * time.Millisecond
			backward := 20*time.Millisecond - forward
			for i := range 3 {
				sent := base.Add(time.Duration(i) * time.Second)
				peer := sent.Add(forward + held + skew)
				arrived := sent.Add(forward + held + backward)
				rtp := uint32(100000 + i*videoClock)
				c.report(ntp64(peer), rtp, arrived)
				if !c.calibrate(ntp64(peer), sent, arrived, uint32(held*65536/time.Second)) {
					t.Fatal("valid exchange was rejected")
				}
				// The picture is 10 ms older than the sender report. Its
				// remote wall clock is skewed, but the local result must not be.
				timing := c.source(rtp-videoClock/100, arrived)
				if i == 0 {
					if timing.ClockSynced {
						t.Fatal("one observation prematurely verified the clock")
					}
					continue
				}
				wantSource := peer.Add(-skew - 10*time.Millisecond)
				error := timing.Source.Sub(wantSource)
				if !timing.ClockSynced || error < -timing.ClockError || error > timing.ClockError || timing.ClockError > 12*time.Millisecond {
					t.Fatalf("skew=%v forward=%v: source error=%v outside bound, timing=%+v", skew, forward, error, timing)
				}
			}
			if c.source(c.rtp, base.Add(15*time.Second)).ClockSynced {
				t.Fatal("stale calibration remained valid")
			}
		}
	}
}

func TestMediaClockRejectsInvalidEchoAndClockStep(t *testing.T) {
	base := time.Now()
	var c mediaClock
	if c.calibrate(ntp64(base), base, base.Add(time.Millisecond), 65536) {
		t.Fatal("server hold longer than elapsed became a valid RTT")
	}
	for i := range 2 {
		sent := base.Add(time.Duration(i) * time.Second)
		c.report(ntp64(sent), 1000+uint32(i*videoClock), sent.Add(20*time.Millisecond))
		c.calibrate(ntp64(sent), sent.Add(-10*time.Millisecond), sent.Add(20*time.Millisecond), 0)
	}
	if !c.source(c.rtp, base.Add(time.Second+20*time.Millisecond)).ClockSynced {
		t.Fatal("expected a verified clock")
	}
	// The server's clock stepped by 2.5 seconds after calibration.
	arrived := base.Add(2 * time.Second)
	c.report(ntp64(arrived.Add(2500*time.Millisecond)), 1000+2*videoClock, arrived)
	if c.source(c.rtp, arrived).ClockSynced {
		t.Fatal("clock step retained the previous calibration")
	}
}
