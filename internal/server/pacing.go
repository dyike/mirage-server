package server

import (
	"context"
	"time"
)

// Encoders may exceed requested bitrate on difficult frames. Pace the complete
// reference stream instead of dropping encoded pictures and corrupting decode.
// Default burst credit is 50ms. ARD can use a larger bounded credit with
// a separate peak-rate limiter; raw capture queues discard stale frames.
type videoPacer struct {
	next  time.Time
	rate  int
	burst time.Duration
}

func (p *videoPacer) delay(now time.Time, bytes, rate int) time.Duration {
	if rate < 1 {
		return 0
	}
	if p.rate != rate {
		p.next = now
		p.rate = rate
	}
	if p.next.Before(now) {
		p.next = now
	}
	p.next = p.next.Add(time.Duration(int64(bytes) * 8 * int64(time.Millisecond) / int64(rate)))
	burst := p.burst
	if burst == 0 {
		burst = 50 * time.Millisecond
	}
	return max(0, p.next.Sub(now)-burst)
}
func (p *videoPacer) wait(ctx context.Context, bytes, rate int) (time.Duration, error) {
	d := p.delay(time.Now(), bytes, rate)
	if d == 0 {
		return 0, ctx.Err()
	}
	start := time.Now()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return time.Since(start), ctx.Err()
	case <-timer.C:
		return time.Since(start), nil
	}
}

// Permit short-frame delivery above the encoder average only when TCP has
// actually measured spare low-latency capacity. Private addresses alone are
// not evidence: VPN and slow Wi-Fi must keep the conservative wire budget.
func linkPacingRate(target int, rtt, queued time.Duration, bytesPerSecond uint64) int {
	if target <= 0 || rtt <= 0 || rtt > 10*time.Millisecond || queued > 5*time.Millisecond {
		return target
	}
	// 75% of measured delivery, capped to 4x the encoder target. Require
	// at least 2x headroom; a sender-limited measurement cannot inflate itself.
	kbps := min(bytesPerSecond/1000*6, uint64(target)*4)
	if kbps < uint64(target)*2 {
		return target
	}
	return int(kbps)
}
