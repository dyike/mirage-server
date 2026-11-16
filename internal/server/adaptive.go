package server

import "time"

// Start conservatively: a successful local socket write is not a bandwidth
// measurement. Reduce quickly on backpressure, recover only after a quiet period.
type rateController struct {
	maximum, current, fps int
	slow, fast            int
	changed               time.Time
}

func newRateController(bitrate, fps int) *rateController {
	if bitrate <= 0 {
		bitrate = 8000
	}
	if fps <= 0 {
		fps = 30
	}
	return &rateController{maximum: bitrate, current: min(bitrate, 2000), fps: fps}
}
func (r *rateController) observe(now time.Time, writeTime, queuedFor time.Duration) bool {
	budget := time.Second / time.Duration(r.fps)
	pressure := max(writeTime, queuedFor)
	congested := writeTime > max(50*time.Millisecond, 2*budget) || queuedFor > 100*time.Millisecond
	if congested {
		r.slow++
		r.fast = 0
	} else if writeTime < budget/2 && queuedFor < 20*time.Millisecond {
		r.fast++
		r.slow = max(0, r.slow-1)
	} else {
		r.fast = 0
	}
	next := r.current
	if (r.slow >= 3 || pressure >= 250*time.Millisecond) && now.Sub(r.changed) >= time.Second {
		next = max(min(500, r.maximum), r.current*2/3)
		if pressure >= 250*time.Millisecond {
			next = max(min(500, r.maximum), r.current/2)
		}
	} else if r.fast >= r.fps*5 && now.Sub(r.changed) >= 10*time.Second && r.current < r.maximum {
		next = min(r.maximum, r.current+max(100, r.current/10))
	}
	if next == r.current {
		return false
	}
	r.current = next
	r.changed = now
	r.slow = 0
	r.fast = 0
	return true
}
