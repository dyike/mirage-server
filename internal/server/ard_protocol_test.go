package server

import (
	"bytes"
	"compress/zlib"
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"github.com/pion/rtcp"
	"testing"
	"time"
)

func ardTestOffer() []byte {
	codec := ardPBInt(nil, 1, 100)
	stream := ardPBInt(nil, 1, 42)
	stream = ardPBData(stream, 3, codec)
	bandwidth := ardPBInt(nil, 1, 0)
	bandwidth = ardPBInt(bandwidth, 2, 1500000)
	bandwidth = ardPBInt(bandwidth, 3, 262144)
	pb := ardPBData(nil, 5, stream)
	pb = ardPBData(pb, 9, bandwidth)
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	z.Write(pb)
	z.Close()
	plist := ardMediaPlist(compressed.Bytes())
	b := make([]byte, 128+92)
	b[0] = 28
	networkOrder.PutUint16(b[4:], 3)
	networkOrder.PutUint16(b[12:], uint16(len(plist)))
	for i := 128; i < 128+46; i++ {
		b[i] = 1
	}
	for i := 128 + 46; i < len(b); i++ {
		b[i] = 2
	}
	b = append(b, plist...)
	networkOrder.PutUint16(b[2:], uint16(len(b)-4))
	return b
}
func TestARDOffer(t *testing.T) {
	b := ardTestOffer()
	o, err := parseARDOffer(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.codec != "hevc" || o.peerSSRC != 42 || o.bitrate != 1500 || o.receiveKey[0] != 1 || o.sendKey[0] != 2 {
		t.Fatalf("incorrect offer fields: codec=%s SSRC=%d bitrate=%d", o.codec, o.peerSSRC, o.bitrate)
	}
	for n := 0; n < len(b); n++ {
		if _, err := parseARDOffer(b[:n], true); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	for _, offset := range []int{0, 2, 4, 10, 12} {
		bad := bytes.Clone(b)
		bad[offset] ^= 255
		if _, err := parseARDOffer(bad, true); err == nil {
			t.Fatalf("accepted corrupt header at %d", offset)
		}
	}
}
func TestARDAdaptiveRate(t *testing.T) {
	now := time.Now()
	m := &ardMedia{maximum: 8000, bitrate: 2000, lastAdjust: now, lastLoss: now}
	m.adjust(now.Add(time.Second), true, false)
	if m.bitrate >= 2000 {
		t.Fatal("loss did not reduce rate")
	}
	lower := m.bitrate
	for i := 0; i < 10; i++ {
		m.adjust(now.Add(1100*time.Millisecond), true, true)
	}
	if m.bitrate != lower {
		t.Fatal("feedback burst repeatedly reduced rate")
	}
	m.adjust(now.Add(12*time.Second), false, false)
	if m.bitrate <= lower {
		t.Fatal("quiet link did not recover rate")
	}
	m.maximum = 300
	m.bitrate = 300
	m.adjust(now.Add(20*time.Second), true, false)
	if m.bitrate > 300 {
		t.Fatal("minimum exceeded client cap")
	}
}
func FuzzARDOffer(f *testing.F) {
	f.Add(ardTestOffer())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65539 {
			return
		}
		parseARDOffer(b, true)
	})
}
func FuzzARDPlist(f *testing.F) {
	f.Add(ardMediaPlist([]byte("seed")))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			return
		}
		ardPlistData(b, "avcMediaStreamNegotiatorMediaBlob")
	})
}

func TestARDIsolatedLossDoesNotCollapseBitrate(t *testing.T) {
	now := time.Now()
	m := &ardMedia{maximum: 8000, bitrate: 2000, lastAdjust: now, lastLoss: now}
	packet := make([]byte, 32)
	copy(packet[8:], "RCTL")
	networkOrder.PutUint16(packet[28:], 1<<12)
	for i := 1; i <= 8; i++ {
		m.observeRateControl(packet, now.Add(time.Duration(i)*time.Second))
	}
	if m.bitrate != 2000 {
		t.Fatalf("isolated loss collapsed rate to %d", m.bitrate)
	}
	networkOrder.PutUint16(packet[28:], 3<<12)
	m.observeRateControl(packet, now.Add(9*time.Second))
	if m.bitrate != 2000 {
		t.Fatal("transient sequence gap reduced bitrate without interval loss")
	}
}

func TestARDDoesNotNegotiateVNCFeedback(t *testing.T) {
	p := &rfbPeer{ard: &ardPeer{}, server: &RFBServer{}, advertised: map[uint32]bool{}}
	if err := p.announce(map[uint32]bool{uint32(vncfeedback.Encoding): true}); err != nil {
		t.Fatal(err)
	}
	if p.advertised[uint32(vncfeedback.Encoding)] {
		t.Fatal("ARD must use RTCP, not ordinary VNC feedback")
	}
}

func TestARDStableReceiverDelayDoesNotRepeatedlyCutRate(t *testing.T) {
	now := time.Now()
	m := &ardMedia{maximum: 6000, bitrate: 6000, lastAdjust: now, lastLoss: now}
	packet := make([]byte, 32)
	copy(packet[8:], "RCTL")
	report := func(ms uint16, sec int) {
		networkOrder.PutUint16(packet[26:], uint16(uint32(ms)*8192/1000))
		m.observeRateControl(packet, now.Add(time.Duration(sec)*time.Second))
	}
	report(10, 1)
	report(120, 2)
	if m.bitrate != 4000 {
		t.Fatalf("growing queue not handled: %d", m.bitrate)
	}
	for i := 3; i < 10; i++ {
		report(120, i)
	}
	if m.bitrate != 4000 {
		t.Fatalf("stable delay collapsed bitrate: %d", m.bitrate)
	}
	report(160, 10)
	if m.bitrate >= 4000 {
		t.Fatal("additional growth not handled")
	}
}

func TestARDQueueDiagnosticRetainsLossControl(t *testing.T) {
	now := time.Now()
	m := &ardMedia{ignoreQueueFeedback: true, maximum: 3000, bitrate: 3000, lastAdjust: now, lastLoss: now}
	p := make([]byte, 32)
	copy(p[8:], "RCTL")
	m.observeRateControl(p, now)
	networkOrder.PutUint16(p[26:], 8192)
	m.observeRateControl(p, now.Add(time.Second))
	if m.bitrate != 3000 {
		t.Fatal("queue diagnostic reduced bitrate")
	}
	networkOrder.PutUint16(p[28:], 3<<12)
	m.observeRateControl(p, now.Add(2*time.Second))
	if m.bitrate != 3000 {
		t.Fatal("unconfirmed sequence gap reduced bitrate")
	}
	m.observeReceptionReport(rtcp.ReceptionReport{SSRC: m.ssrc, FractionLost: 8}, now.Add(3*time.Second))
	if m.bitrate >= 3000 {
		t.Fatal("queue diagnostic disabled loss control")
	}
}

func TestARDReceiverReportLossEvidence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		ssrc     uint32
		fraction uint8
		reduced  bool
	}{
		{"quiet", 42, 0, false},
		{"below threshold", 42, 2, false},
		{"interval loss", 42, 3, true},
		{"other stream", 43, 128, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &ardMedia{ssrc: 42, maximum: 20000, bitrate: 20000, lastAdjust: now, lastLoss: now}
			m.observeReceptionReport(rtcp.ReceptionReport{SSRC: tc.ssrc, FractionLost: tc.fraction}, now.Add(time.Second))
			if (m.bitrate < 20000) != tc.reduced {
				t.Fatalf("rate=%d reduced=%t", m.bitrate, tc.reduced)
			}
		})
	}
}
