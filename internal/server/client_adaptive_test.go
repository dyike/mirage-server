package server

import (
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"testing"
	"time"
)

func TestClientPressureHysteresisAndRecovery(t *testing.T) {
	c := newClientController(30)
	now := time.Unix(100, 0)
	bad := vncfeedback.Report{Presentation: true, IntervalMS: 1000, Frames: 30, Presented: 10, DecodeUS: 40000, Pending: 10}
	if c.observe(now, bad) {
		t.Fatal("one bad sample reduced FPS")
	}
	if !c.observe(now.Add(time.Second), bad) || c.current != 22 {
		t.Fatal("sustained pressure ignored", c)
	}
	if c.observe(now.Add(1500*time.Millisecond), bad) {
		t.Fatal("cooldown ignored")
	}
	for i := 2; i < 30; i++ {
		c.observe(now.Add(time.Duration(i)*time.Second), bad)
	}
	if c.current != 10 {
		t.Fatal("FPS floor", c.current)
	}
	good := vncfeedback.Report{Presentation: true, IntervalMS: 1000, Frames: 10, Presented: 10, DecodeUS: 1000}
	for i := 30; i < 80; i++ {
		c.observe(now.Add(time.Duration(i)*time.Second), good)
	}
	if c.current != 30 {
		t.Fatal("healthy client did not recover", c.current)
	}
}
func TestStaticAndBorderlineClientsDoNotOscillate(t *testing.T) {
	c := newClientController(30)
	for i := 0; i < 20; i++ {
		c.observe(time.Unix(int64(100+i), 0), vncfeedback.Report{Frames: 1, DecodeUS: 100000, Pending: 20, Presentation: true})
	}
	if c.current != 30 {
		t.Fatal("static desktop interpreted as overload")
	}
	c.current = 20
	for i := 0; i < 30; i++ {
		c.observe(time.Unix(int64(100+i), 0), vncfeedback.Report{Frames: 20, Presented: 20, Presentation: true, DecodeUS: 32000})
	}
	if c.current != 20 {
		t.Fatal("borderline decode headroom caused oscillation", c.current)
	}
}
