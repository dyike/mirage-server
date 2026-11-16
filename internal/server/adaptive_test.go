package server

import (
	"testing"
	"time"
)

func TestAdaptiveRateBoundedAndHysteresis(t *testing.T) {
	r := newRateController(8000, 30)
	now := time.Unix(100, 0)
	if r.current != 2000 {
		t.Fatal("must not start at configured maximum")
	}
	for i := 0; i < 2; i++ {
		if r.observe(now, 100*time.Millisecond, 0) {
			t.Fatal("ordinary burst caused immediate reset")
		}
	}
	if !r.observe(now, 100*time.Millisecond, 0) || r.current != 1333 {
		t.Fatal("congestion ignored")
	}
	if r.observe(now.Add(500*time.Millisecond), time.Second, 0) {
		t.Fatal("downshift cooldown ignored")
	}
	if !r.observe(now.Add(time.Second), time.Second, 0) || r.current != 666 {
		t.Fatal("severe stall did not halve bitrate")
	}
	for i := 0; i < 200; i++ {
		r.observe(now.Add(12*time.Second), time.Millisecond, 0)
	}
	if r.current <= 666 || r.current > r.maximum {
		t.Fatal("recovery out of bounds")
	}
}
func TestAdaptiveQueueAndSingleStall(t *testing.T) {
	for _, timings := range [][2]time.Duration{{time.Second, 0}, {time.Millisecond, 500 * time.Millisecond}} {
		r := newRateController(20000, 60)
		if !r.observe(time.Unix(100, 0), timings[0], timings[1]) || r.current != 1000 {
			t.Fatal("single severe write/queued backlog must reduce immediately")
		}
	}
	r := newRateController(300, 30)
	for i := 0; i < 20; i++ {
		r.observe(time.Unix(int64(100+i), 0), time.Second, 0)
	}
	if r.current != 300 {
		t.Fatal("minimum raised a lower configured maximum")
	}
}
