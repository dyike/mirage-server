package rfb

import (
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"sync/atomic"
	"time"
)

type videoFeedback struct {
	enabled                     bool // receive goroutine owns window and decode counters
	since                       time.Time
	decode                      time.Duration
	decoded                     uint64
	submitted, drawn, watermark atomic.Uint64
	presentation                atomic.Bool
	lastSubmitted, lastDrawn    uint64
}

func (c *Client) SubmittedFrames() uint64 { return c.feedback.submitted.Load() }
func (c *Client) FramesPresented(seq uint64) {
	if seq == 0 || seq > c.feedback.submitted.Load() {
		return
	}
	for {
		old := c.feedback.watermark.Load()
		if seq <= old {
			return
		}
		if c.feedback.watermark.CompareAndSwap(old, seq) {
			break
		}
	}
	c.feedback.presentation.Store(true)
	c.feedback.drawn.Add(1)
}
func (c *Client) reportVideoFeedback(now time.Time) error {
	f := &c.feedback
	if !f.enabled {
		return nil
	}
	if f.since.IsZero() {
		f.since = now
		f.lastSubmitted = f.submitted.Load()
		f.lastDrawn = f.drawn.Load()
		return nil
	}
	elapsed := now.Sub(f.since)
	if elapsed < time.Second {
		return nil
	}
	submitted, drawn := f.submitted.Load(), f.drawn.Load()
	if elapsed <= 10*time.Second {
		avg := time.Duration(0)
		if f.decoded > 0 {
			avg = f.decode / time.Duration(f.decoded)
		}
		pending := uint64(0)
		if mark := f.watermark.Load(); mark < submitted {
			pending = submitted - mark
		}
		r := vncfeedback.Report{Presentation: f.presentation.Load(), IntervalMS: uint32(elapsed / time.Millisecond), Frames: uint32(min(10000, submitted-f.lastSubmitted)), Presented: uint32(min(10000, drawn-f.lastDrawn)), DecodeUS: uint32(min(10000000, max(0, avg/time.Microsecond))), Pending: uint32(min(10000, pending))}
		if err := c.send(r.Marshal()); err != nil {
			return err
		}
	}
	f.since = now
	f.lastSubmitted, f.lastDrawn = submitted, drawn
	f.decode, f.decoded = 0, 0
	return nil
}
