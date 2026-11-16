package server

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/srtp/v3"
)

func TestARDPacedAccessUnitStaysComplete(t *testing.T) {
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	source, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	tx, _ := srtp.CreateContext(make([]byte, 32), make([]byte, 14), srtp.ProtectionProfileAes256CmHmacSha1_80)
	rx, _ := srtp.CreateContext(make([]byte, 32), make([]byte, 14), srtp.ProtectionProfileAes256CmHmacSha1_80)
	now := time.Now()
	m := &ardMedia{socket: source, tx: tx, remote: sink.LocalAddr().(*net.UDPAddr), lastFeedback: now, lastReport: now, epoch: now, bitrate: 1000, maximum: 1000, codec: "h264", ssrc: 123, sequence: 1, payloader: &codecs.H264Payloader{}}
	m.pacer = videoPacer{rate: 1000, next: now.Add(150 * time.Millisecond)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// At 1 Mbit/s this takes over the old 150ms cutoff even after burst credit.
	data := append([]byte{0, 0, 0, 1, 0x41}, bytes.Repeat([]byte{0x55}, 40000)...)
	sent := make(chan error, 1)
	go func() { sent <- m.send(ctx, data, false, nil) }()
	sink.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	count := 0
	var last uint16
	for {
		n, err := sink.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		var header rtp.Header
		if _, err = rx.DecryptRTP(nil, buf[:n], &header); err != nil {
			t.Fatal(err)
		}
		if count == 0 && time.Duration(header.Timestamp)*time.Second/ardClock < 90*time.Millisecond {
			t.Fatal("RTP timestamp includes sender pacing wait as network delay")
		}
		if count > 0 && header.SequenceNumber != last+1 {
			t.Fatal("sender introduced a packet gap")
		}
		last = header.SequenceNumber
		count++
		if header.Marker {
			break
		}
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	if time.Since(now) < 150*time.Millisecond {
		t.Fatal("test did not exercise paced frame deadline")
	}
	if m.rate() != 1000 {
		t.Fatal("sender reduced bitrate because of its own pacing")
	}
}

func TestARDInitialRateHonorsBothCaps(t *testing.T) {
	for _, tc := range []struct{ server, client, want int }{
		{8000, 12000, 8000}, {8000, 1500, 1500}, {0, 0, 8000},
	} {
		socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		m, err := newARDMedia(socket, ardOffer{codec: "h264", bitrate: tc.client}, net.IPv4(127, 0, 0, 1), tc.server)
		if err != nil {
			socket.Close()
			t.Fatal(err)
		}
		got := m.rate()
		if m.pacer.burst != 500*time.Millisecond || m.peakPacer.burst != time.Millisecond || m.wireMaximum < got {
			m.close()
			t.Fatal("invalid bounded burst configuration")
		}
		m.close()
		if got != tc.want {
			t.Fatalf("caps %+v: initial rate %d", tc, got)
		}
	}
}

func TestARDDiagnosticBitrateRespectsServerLimit(t *testing.T) {
	t.Setenv("MIRAGE_ARD_TEST_BITRATE", "20000")
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := newARDMedia(socket, ardOffer{codec: "h264", bitrate: 3000}, net.IPv4(127, 0, 0, 1), 12000)
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	defer m.close()
	if m.rate() != 12000 {
		t.Fatalf("diagnostic exceeded server cap: %d", m.rate())
	}
}
