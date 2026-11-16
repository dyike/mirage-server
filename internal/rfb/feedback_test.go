package rfb

import (
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"testing"
	"time"
)

func TestFeedbackRequiresConfirmationAndCountsDraws(t *testing.T) {
	wire := &keyWriteConn{}
	c := &Client{conn: wire}
	now := time.Unix(100, 0)
	c.feedback.submitted.Store(30)
	c.FramesPresented(20)
	c.FramesPresented(20)
	c.FramesPresented(31)
	if c.feedback.drawn.Load() != 1 {
		t.Fatal("duplicate or future draw counted")
	}
	if err := c.reportVideoFeedback(now); err != nil || wire.Len() != 0 {
		t.Fatal("sent before server confirmation")
	}
	c.feedback.enabled = true
	c.reportVideoFeedback(now)
	c.feedback.submitted.Store(60)
	c.FramesPresented(60)
	c.feedback.decode = 300 * time.Millisecond
	c.feedback.decoded = 30
	if err := c.reportVideoFeedback(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := vncfeedback.Parse(wire.Bytes())
	if err != nil || got.Frames != 30 || got.Presented != 1 || got.Pending != 0 || got.DecodeUS != 10000 || !got.Presentation {
		t.Fatal(got, err)
	}
	wire.Reset()
	c.reportVideoFeedback(now.Add(1500 * time.Millisecond))
	if wire.Len() != 0 {
		t.Fatal("reported too frequently")
	}
	c.reportVideoFeedback(now.Add(time.Minute))
	if wire.Len() != 0 {
		t.Fatal("stale background interval reported as live pressure")
	}
}
