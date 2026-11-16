package video

import (
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

func TestFrameTimesMatchTokensAcrossDropsAndReordering(t *testing.T) {
	var times frameTimes
	base := time.Unix(1000, 0)
	for id := uint64(1); id <= 3; id++ {
		times.put(id, frame.Timing{Source: base.Add(time.Duration(id) * time.Millisecond), ClockSynced: true, ClockError: time.Millisecond})
	}
	// Picture 2 was dropped and output 3 overtook output 1.
	for _, id := range []uint64{3, 1} {
		if got := times.take(id); got != (frame.Timing{Source: base.Add(time.Duration(id) * time.Millisecond), ClockSynced: true, ClockError: time.Millisecond}) {
			t.Fatalf("token %d consumed another picture's timestamp: %v", id, got)
		}
		if !times.take(id).Source.IsZero() {
			t.Fatal("duplicate output reused a timestamp")
		}
	}
	times.put(258, frame.Timing{Source: base.Add(time.Second)})
	if !times.take(2).Source.IsZero() || times.take(258).Source != base.Add(time.Second) {
		t.Fatal("bounded timestamp slot retained or confused an evicted frame")
	}
}
