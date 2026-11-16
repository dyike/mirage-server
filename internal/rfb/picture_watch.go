package rfb

import (
	"sync"
	"time"
)

const (
	pictureStallDelay       = 1500 * time.Millisecond
	pictureRecoveryInterval = 2 * time.Second
	pictureRecoveryWindow   = 8 * time.Second
	pictureRecoveryLimit    = 3
)

// A static desktop is normal. Watch only briefly after real input/startup,
// and bound full refreshes even when the remote screen intentionally stays
// unchanged. No key values, text or credentials are retained here.
type pictureWatch struct {
	mu                     sync.Mutex
	lastFrame, until, next time.Time
	tries                  int
}

func (w *pictureWatch) input(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.until.IsZero() || now.After(w.until) {
		w.tries, w.next = 0, now.Add(pictureStallDelay)
	}
	w.until = now.Add(pictureRecoveryWindow)
}

func (w *pictureWatch) frame(now time.Time) {
	w.mu.Lock()
	w.lastFrame = now
	w.mu.Unlock()
}

func (w *pictureWatch) due(now time.Time) (attempt int, age time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.until.IsZero() || now.After(w.until) || now.Before(w.next) || w.tries >= pictureRecoveryLimit {
		return
	}
	if !w.lastFrame.IsZero() && now.Sub(w.lastFrame) < pictureStallDelay {
		return
	}
	w.tries++
	w.next = now.Add(pictureRecoveryInterval)
	return w.tries, now.Sub(w.lastFrame)
}

func (c *Client) recoverPicture(now time.Time) {
	if c.closing.Load() {
		return
	}
	attempt, age := c.pictureWatch.due(now)
	if attempt == 0 {
		return
	}
	// Refresh contains only a full-frame request, push rearming and (if a
	// video leg is active) a keyframe request. Never replay any input.
	err := c.Refresh()
	c.recoveryLog(attempt, age, err)
}

func (c *Client) startPictureWatch() func() {
	// This workaround targets Apple's auto-push mode. Plain VNC keeps its
	// normal update-request pipeline and does not acquire a polling loop.
	if !c.apple || !c.pushing.Load() {
		return func() {}
	}
	now := time.Now()
	c.pictureWatch.frame(now)
	c.pictureWatch.input(now)
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				c.recoverPicture(now)
			}
		}
	}()
	return func() {
		close(done)
		// Closing the socket first also interrupts an in-flight recovery
		// write, so Run cannot wait forever while the network is down.
		c.Close()
		<-exited
	}
}
