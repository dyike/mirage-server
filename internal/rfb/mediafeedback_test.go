package rfb

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// TestRateFeedbackLayout matches AVConference's serializer output for
// echo 0x1234, burst loss 3, count 0x567, arrival 126 ms ago, clock 129 and
// packed delay 1024: 85000004 1234 00000000 007e 0081 0400 3567 0000.
func TestRateFeedbackLayout(t *testing.T) {
	epoch := time.Unix(1000, 0)
	now := epoch.Add(129*time.Second/1024 + 1) // 129/1024 s is not a whole number of ns
	f := rateFeedback{epoch: epoch, have: true, videoArrived: true,
		sampleTS: 0x1234 << 8, sampleAt: now.Add(-126 * time.Millisecond),
		sampleReceived: 0x567, burstLoss: 3, delay: queueDelay{value: 1024.0 / 8192}}
	p := f.packet(0xaabbccdd, now)
	want, _ := hex.DecodeString("850000041234000000000" + "07e008104003567" + "0000")
	if !bytes.Equal(p[:8], []byte{0x80, 204, 0, 7, 0xaa, 0xbb, 0xcc, 0xdd}) || string(p[8:12]) != "RCTL" {
		t.Fatalf("header % x", p[:12])
	}
	if !bytes.Equal(p[12:], want) {
		t.Errorf("body % x\nwant % x", p[12:], want)
	}
	if f.burstLoss != 0 {
		t.Error("burst loss not reset after a report")
	}
}

func TestReceptionReport(t *testing.T) {
	var r reception
	now := time.Now()
	for _, seq := range []uint16{65534, 65535, 1, 2} { // 0 is lost, across the wrap
		r.record(seq, uint32(seq)*100, now)
	}
	rep := r.report(7, now)
	if rep.LastSequenceNumber != 1<<16|2 || rep.TotalLost != 1 || rep.FractionLost != 256/5 {
		t.Errorf("report %+v", rep)
	}
}
