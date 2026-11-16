package rfb

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestReorderFinalPacketsExpireWithoutMoreInput(t *testing.T) {
	for _, first := range []uint16{1, 65532} {
		var b reorderBuffer
		now := time.Now()
		got := seqs(&b, []uint16{first, first + 2, first + 4, first + 5}, now)
		emit := func(h *rtp.Header, _ []byte) { got = append(got, h.SequenceNumber) }
		deadline := now.Add(reorderDelay)
		if !b.deadline().Equal(deadline) {
			t.Fatalf("deadline %v, want %v", b.deadline(), deadline)
		}
		b.expire(deadline.Add(-time.Nanosecond), emit)
		if len(got) != 1 {
			t.Fatalf("expired too early: %v", got)
		}
		b.expire(deadline, emit)
		want := []uint16{first, first + 2, first + 4, first + 5}
		if !equalSeqs(got, want) || b.lost != 2 || !b.deadline().IsZero() {
			t.Fatalf("final packets %v, loss %d, deadline %v", got, b.lost, b.deadline())
		}
	}
}

func TestMediaLegWakesForFinalReorderGap(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := newSRTP(make([]byte, 46))
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	var b reorderBuffer
	now := time.Now()
	seqs(&b, []uint16{1, 3}, now)
	frames := make(chan uint16, 1)
	leg := &mediaLeg{conn: conn, tx: tx, readDeadline: b.deadline}
	leg.onTimeout = func(now time.Time) {
		b.expire(now, func(h *rtp.Header, _ []byte) { frames <- h.SequenceNumber })
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() { defer close(exited); leg.run(done) }()
	defer func() { close(done); conn.Close(); <-exited }()
	select {
	case seq := <-frames:
		if seq != 3 || time.Since(now) < reorderDelay {
			t.Fatalf("unexpected expiration: seq %d at %v", seq, time.Since(now))
		}
	case <-time.After(time.Second):
		t.Fatal("socket never woke up for the last buffered packet")
	}
}
