package server

import (
	"bytes"
	"context"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"net"
	"testing"
	"time"
)

func TestARDAudioRequiresAuthenticatedPeerAndCloses(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer viewer.Close()
	offer := ardOffer{audioPeerSSRC: 77}
	copy(offer.audioSendKey[:], bytes.Repeat([]byte{1}, 46))
	copy(offer.audioReceiveKey[:], bytes.Repeat([]byte{2}, 46))
	payload := bytes.Repeat([]byte{0x55}, 160)
	stopped := make(chan struct{})
	started := make(chan struct{})
	a, err := newARDAudio(context.Background(), server, offer, net.IPv4(127, 0, 0, 1), func(ctx context.Context, send func([]byte) error) error {
		close(started)
		defer close(stopped)
		timer := time.NewTicker(10 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				if err := send(payload); err != nil {
					return err
				}
			}
		}
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer a.close()
	rr, _ := (&rtcp.ReceiverReport{SSRC: 77}).Marshal()
	viewer.WriteToUDP(rr, server.LocalAddr().(*net.UDPAddr))
	buf := make([]byte, 2048)
	viewer.SetReadDeadline(time.Now().Add(60 * time.Millisecond))
	if _, _, err = viewer.ReadFromUDP(buf); err == nil {
		t.Fatal("unauthenticated receiver received audio")
	}
	select {
	case <-started:
		t.Fatal("capture started before authenticated feedback")
	default:
	}
	tx, _ := srtp.CreateContext(offer.audioReceiveKey[:32], offer.audioReceiveKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80)
	rx, _ := srtp.CreateContext(offer.audioSendKey[:32], offer.audioSendKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80)
	packet, _ := tx.EncryptRTCP(nil, rr, nil)
	viewer.WriteToUDP(packet, server.LocalAddr().(*net.UDPAddr))
	viewer.SetReadDeadline(time.Now().Add(time.Second))
	var prior rtp.Header
	for i := 0; i < 2; {
		n, _, err := viewer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		if buf[1] >= 192 && buf[1] <= 223 {
			continue
		}
		var h rtp.Header
		plain, err := rx.DecryptRTP(nil, buf[:n], &h)
		if err != nil {
			t.Fatal(err)
		}
		if h.PayloadType != 101 || !bytes.Equal(plain[h.MarshalSize():], payload) {
			t.Fatal("invalid audio RTP")
		}
		if i > 0 && (h.Timestamp-prior.Timestamp != 480 || h.SequenceNumber != prior.SequenceNumber+1) {
			t.Fatal("invalid audio cadence")
		}
		prior = h
		i++
	}
	a.cancel()
	a.socket.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("audio capture leaked after cancellation")
	}
}
