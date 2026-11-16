package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type progressConn struct {
	net.Conn
	data              bytes.Buffer
	ready             bool
	deadlines, writes int
}

func (c *progressConn) SetWriteDeadline(time.Time) error { c.ready = true; c.deadlines++; return nil }
func (c *progressConn) Write(b []byte) (int, error) {
	if !c.ready || len(b) > 16<<10 {
		return 0, errors.New("write exceeded progress window")
	}
	c.ready = false
	c.writes++
	return c.data.Write(b)
}
func TestUnpacedFramebufferRefreshesDeadlinePerChunk(t *testing.T) {
	c := new(progressConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := rfbPeer{ctx: ctx, cancel: cancel, conn: c}
	payload := bytes.Repeat([]byte{1, 7, 33, 255}, 400000)
	if err := p.send(payload); err != nil {
		t.Fatal(err)
	}
	if c.writes < 2 || c.writes != c.deadlines || !bytes.Equal(payload, c.data.Bytes()) {
		t.Fatal("frame did not make bounded progress")
	}
}
