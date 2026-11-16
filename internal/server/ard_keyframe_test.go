package server

import (
	"testing"
	"time"
)

func TestARDRepairFrameBudget(t *testing.T) {
	for _, tc := range []struct {
		bytes, rate int
		want        time.Duration
	}{
		{0, 3000, 250 * time.Millisecond},
		{210000, 3000, 1120 * time.Millisecond},
		{210000, 20000, 250 * time.Millisecond},
		{300000, 600, 2 * time.Second},
	} {
		if got := ardKeyframeInterval(tc.bytes, tc.rate); got != tc.want {
			t.Fatalf("bytes=%d rate=%d got=%v want=%v", tc.bytes, tc.rate, got, tc.want)
		}
	}
}
func TestARDKeyframeAccessUnits(t *testing.T) {
	for _, tc := range []struct {
		codec string
		data  []byte
		key   bool
	}{
		{"h264", []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 1, 0x65, 2}, true},
		{"h264", []byte{0, 0, 1, 0x41, 2}, false},
		{"hevc", []byte{0, 0, 0, 1, 32 << 1, 1, 0, 0, 1, 19 << 1, 1, 2}, true},
		{"hevc", []byte{0, 0, 1, 1 << 1, 1, 2}, false},
		{"hevc", nil, false},
	} {
		if got := ardKeyframe(tc.codec, tc.data); got != tc.key {
			t.Fatalf("codec=%s data=%x key=%v", tc.codec, tc.data, got)
		}
	}
}

func TestARDQueueReducesCaptureWithoutCollapsingBitrate(t *testing.T) {
	now := time.Now()
	m := &ardMedia{maximum: 3000, bitrate: 3000, maximumFPS: 60, frameRate: 60, lastAdjust: now, lastLoss: now, lastFPSAdjust: now}
	packet := make([]byte, 32)
	copy(packet[8:], "RCTL")
	m.observeRateControl(packet, now)
	for sec := 2; sec <= 10; sec += 2 {
		networkOrder.PutUint16(packet[26:], uint16(sec*100*8192/1000))
		m.observeRateControl(packet, now.Add(time.Duration(sec)*time.Second))
	}
	if m.bitrate != 3000 || m.frameRate != 15 {
		t.Fatalf("bitrate=%d fps=%d", m.bitrate, m.frameRate)
	}
	networkOrder.PutUint16(packet[26:], 0)
	m.observeRateControl(packet, now.Add(21*time.Second))
	if m.frameRate != 20 {
		t.Fatalf("capture did not recover: %d", m.frameRate)
	}
	m.adjust(now.Add(22*time.Second), true, false)
	if m.bitrate >= 3000 {
		t.Fatal("capture adaptation disabled real loss control")
	}
}
