package server

import (
	"context"
	"testing"
	"time"
)

func TestVideoPacerBoundsBurstAndHonorsChanges(t *testing.T) {
	var p videoPacer
	now := time.Unix(100, 0)
	if d := p.delay(now, 12500, 2000); d != 0 {
		t.Fatal(d)
	}
	if d := p.delay(now, 12500, 2000); d != 50*time.Millisecond {
		t.Fatal(d)
	}
	if d := p.delay(now, 125000, 2000); d != 550*time.Millisecond {
		t.Fatal(d)
	}
	if d := p.delay(now.Add(time.Second), 12500, 1000); d != 50*time.Millisecond {
		t.Fatal(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.wait(ctx, 1<<20, 1000); err != context.Canceled {
		t.Fatal("pacer did not cancel", err)
	}
}
func TestVideoPacerSustainedRate(t *testing.T) {
	var p videoPacer
	start := time.Unix(100, 0)
	now := start
	for i := 0; i < 100; i++ {
		now = now.Add(p.delay(now, 16000, 2000))
	}
	elapsed := now.Sub(start)
	// 1.6MB at 2Mbit/s takes 6.4s, less only the initial 50ms credit.
	if elapsed < 6350*time.Millisecond || elapsed > 6400*time.Millisecond {
		t.Fatalf("wire budget not honored: %s", elapsed)
	}
}

func TestPacingUsesMeasuredHeadroomOnly(t *testing.T) {
	for _, tt := range []struct {
		rtt, queue time.Duration
		delivery   uint64
		want       int
	}{
		{5 * time.Millisecond, 0, 6500000, 8000},
		{40 * time.Millisecond, 0, 6500000, 2000},
		{5 * time.Millisecond, 20 * time.Millisecond, 6500000, 2000},
		{5 * time.Millisecond, 0, 300000, 2000},
		{0, 0, 6500000, 2000},
		{5 * time.Millisecond, 0, 0, 2000},
	} {
		if got := linkPacingRate(2000, tt.rtt, tt.queue, tt.delivery); got != tt.want {
			t.Fatalf("%+v: got %d", tt, got)
		}
	}
}

func TestBurstPacingPreservesSustainedBudget(t *testing.T) {
	p := videoPacer{burst: 500 * time.Millisecond}
	start := time.Unix(100, 0)
	now := start
	for range 100 {
		now = now.Add(p.delay(now, 16000, 2000))
	}
	if got := now.Sub(start); got != 5900*time.Millisecond {
		t.Fatalf("sustained budget: %s", got)
	}
	peak := videoPacer{burst: 5 * time.Millisecond}
	if got := peak.delay(start, 25000, 20000); got != 5*time.Millisecond {
		t.Fatalf("peak budget: %s", got)
	}
}
