package rfb

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func TestReceivedBytesIncludesRFBAndMediaAcrossRestarts(t *testing.T) {
	c := &Client{}
	r := &countingReader{r: bytes.NewReader(make([]byte, 512)), n: &c.received}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	want := int64(512)
	if got := c.ReceivedBytes(); got != want {
		t.Fatalf("RFB bytes %d, want %d", got, want)
	}
	// Video, audio, and replacement legs all share the session counter.
	for range 3 {
		c.media.Store(&mediaState{})
		want += receiveMediaTraffic(t, c)
		if got := c.ReceivedBytes(); got != want {
			t.Fatalf("RFB + UDP bytes %d, want %d", got, want)
		}
	}
	c.media.Store(nil) // falling back to RFB must not lose earlier UDP bytes
	if c.ReceivedBytes() != want {
		t.Fatal("fallback reset the receive counter")
	}
}

func receiveMediaTraffic(t *testing.T, c *Client) int64 {
	t.Helper()
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	viewer, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	makeContext := func() *mediaLeg {
		rx, err := newSRTP(make([]byte, 46))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := newSRTP(make([]byte, 46))
		if err != nil {
			t.Fatal(err)
		}
		return &mediaLeg{rx: rx, tx: tx}
	}
	leg, sender := makeContext(), makeContext()
	leg.conn, leg.received = viewer, &c.mediaReceived
	arrived := make(chan struct{}, 1)
	leg.onPacket = func(*rtp.Header, []byte) { arrived <- struct{}{} }
	done, exited := make(chan struct{}), make(chan struct{})
	go func() { defer close(exited); leg.run(done) }()
	defer func() { close(done); viewer.Close(); <-exited }()
	rawReport, err := rtcp.Marshal([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 1}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := sender.tx.EncryptRTCP(nil, rawReport, nil)
	if err != nil {
		t.Fatal(err)
	}
	packet := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SSRC: 1, SequenceNumber: 1}, Payload: make([]byte, 128)}
	raw, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	video, err := sender.tx.EncryptRTP(nil, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	// Count even undecryptable data and RTCP, not only decoded video payloads.
	for _, payload := range [][]byte{{0x80, 0, 0}, report, video} {
		n, err := server.WriteToUDP(payload, viewer.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		total += int64(n)
	}
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("media datagrams did not arrive")
	}
	if leg.rtcpIn.Load() != 1 || leg.bad.Load() != 1 || leg.packets.Load() != 1 {
		t.Fatal("test did not exercise media control, invalid data and RTP")
	}
	return total
}
