package server

import (
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"time"
)

// Client processing pressure is separate from network bandwidth. Sparse/static
// pictures are not low FPS evidence. Two bad reports reduce load; five healthy
// reports and a ten-second cooldown recover gradually, preventing oscillation.
type clientController struct {
	maximum, current, slow, fast int
	changed                      time.Time
}

func newClientController(fps int) *clientController {
	return &clientController{maximum: fps, current: fps}
}
func (c *clientController) observe(now time.Time, r vncfeedback.Report) bool {
	if r.Frames < 5 {
		c.slow, c.fast = 0, 0
		return false
	}
	budget := uint32(1000000 / max(1, c.current))
	slow := r.DecodeUS > budget*8/10 || r.Presentation && (r.Presented*100 < r.Frames*70 || r.Pending > uint32(max(3, c.current/4)))
	if slow {
		c.slow++
		c.fast = 0
	} else if r.DecodeUS < budget/2 && (!r.Presentation || r.Presented*100 >= r.Frames*90 && r.Pending <= 1) {
		c.slow = 0
		c.fast++
	} else {
		c.slow, c.fast = 0, 0
	}
	next := c.current
	if c.slow >= 2 && now.Sub(c.changed) >= 2*time.Second {
		next = max(min(10, c.maximum), c.current*3/4)
	} else if c.fast >= 5 && now.Sub(c.changed) >= 10*time.Second {
		next = min(c.maximum, c.current+5)
	}
	if next == c.current {
		return false
	}
	c.current = next
	c.changed = now
	c.slow, c.fast = 0, 0
	return true
}
