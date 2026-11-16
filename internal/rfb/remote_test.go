package rfb

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"math"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func varint(v uint64) []byte {
	var p pbuf
	p.varint(v)
	return p
}

func TestFirstMediaOfferFitsTheLink(t *testing.T) {
	local := &Client{conn: remoteConn{remote: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 5)}}, cfg: Config{Network: NetworkLocal}}
	if o := local.firstMediaOffer(); o.mode != negotiatorNearby || o.maxBitrate != 0 {
		t.Fatalf("local offer mode %d cap %d, want Nearby uncapped", o.mode, o.maxBitrate)
	}
	remote := &Client{conn: remoteConn{remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}}, cfg: Config{Network: NetworkRemote}}
	o := remote.firstMediaOffer()
	if o.mode != negotiatorRemoteDesktop || o.maxBitrate != remoteBitrates[0] {
		t.Fatalf("remote offer mode %d cap %d", o.mode, o.maxBitrate)
	}
	// Every bit rate in the bandwidth settings is under the cap.
	blob := o.videoBlob(0)
	for _, over := range []uint64{nearbyBitrate, maxVideoBitrate, 20_000_000, 6_000_000} {
		if bytes.Contains(blob, append([]byte{2 << 3}, varint(over)...)) {
			t.Errorf("capped blob still carries %d bit/s", over)
		}
	}
	if !bytes.Contains(blob, append([]byte{2 << 3}, varint(remoteBitrates[0])...)) {
		t.Error("capped blob lacks the cap")
	}
}

func TestNetworkOverride(t *testing.T) {
	far := remoteConn{remote: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 5)}}
	near := remoteConn{remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}}
	for _, c := range []struct {
		conn    net.Conn
		network string
		want    bool
	}{
		{far, "", false}, {near, "", true}, {far, NetworkLocal, true}, {near, NetworkRemote, false},
	} {
		cl := &Client{conn: c.conn, cfg: Config{Network: c.network}}
		if got := cl.onLocalNetwork(); got != c.want {
			t.Errorf("%v with Network %q: local %v, want %v", c.conn.RemoteAddr(), c.network, got, c.want)
		}
	}
}

func TestViewerVideoLimitUsesCappedOffer(t *testing.T) {
	for _, tc := range []struct {
		name, network string
		limit, want   uint64
	}{
		{"iPad local", NetworkLocal, 3_000_000, 3_000_000},
		{"lower viewer limit", NetworkLocal, 1_000_000, 1_000_000},
		{"remote conservative cap", NetworkRemote, 8_000_000, remoteBitrates[0]},
		{"remote lower viewer limit", NetworkRemote, 1_000_000, 1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{cfg: Config{Network: tc.network, MediaMaxBitrate: tc.limit}}
			offer := c.firstMediaOffer()
			if offer.mode != negotiatorRemoteDesktop || offer.maxBitrate != tc.want || !offer.fps60 {
				t.Fatalf("offer mode=%d cap=%d fps60=%v", offer.mode, offer.maxBitrate, offer.fps60)
			}
			// Check the serialized bandwidth settings, not just the Go field.
			blob := offer.videoBlob(0)
			for _, over := range []uint64{nearbyBitrate, maxVideoBitrate, 20_000_000, 6_000_000} {
				if bytes.Contains(blob, append([]byte{2 << 3}, varint(over)...)) {
					t.Errorf("capped offer still advertises %d bit/s", over)
				}
			}
		})
	}
}

func TestRemoteDesktopVideoModeDoesNotImposeWANCapOnLAN(t *testing.T) {
	for _, tc := range []struct {
		name, network string
		limit, want   uint64
	}{
		{"LAN uncapped", NetworkLocal, 0, 0},
		{"LAN explicit limit", NetworkLocal, 12_000_000, 12_000_000},
		{"WAN default", NetworkRemote, 0, remoteBitrates[0]},
		{"WAN lower limit", NetworkRemote, 1_000_000, 1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{cfg: Config{Network: tc.network, MediaRemoteDesktop: true, MediaMaxBitrate: tc.limit}}
			offer := c.firstMediaOffer()
			if offer.mode != negotiatorRemoteDesktop || offer.maxBitrate != tc.want || !offer.fps60 {
				t.Fatalf("offer mode=%d cap=%d fps60=%v", offer.mode, offer.maxBitrate, offer.fps60)
			}
			if tc.want == 0 && !bytes.Contains(offer.videoBlob(0), append([]byte{2 << 3}, varint(maxVideoBitrate)...)) {
				t.Fatal("uncapped LAN offer lost its normal bandwidth settings")
			}
		})
	}
}

func TestReorderBufferCountsLoss(t *testing.T) {
	var b reorderBuffer
	now := time.Now()
	in := []uint16{1, 2, 5} // 3 and 4 lost
	for s := uint16(6); s < 6+reorderDepth; s++ {
		in = append(in, s)
	}
	for _, s := range in {
		h := rtp.Header{SequenceNumber: s}
		b.push(&h, nil, now, func(*rtp.Header, []byte) {})
	}
	if b.lost != 2 {
		t.Fatalf("lost %d, want 2", b.lost)
	}
}

func economyClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() })
	c := &Client{conn: client, apple: true, appleExt: true, cfg: Config{Network: NetworkRemote}}
	c.appleCmds[msgAppleSetDisplay/8] |= 0x80 >> (msgAppleSetDisplay % 8)
	return c, server
}

func TestRemoteRFBScalesAndShowsMainDisplay(t *testing.T) {
	c, server := economyClient(t)
	go c.economizeRFB()
	server.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 10)
	if _, err := io.ReadFull(server, b); err != nil || b[0] != 0x08 || math.Float64frombits(be.Uint64(b[2:])) != 0.5 {
		t.Fatalf("no half scale: % x, %v", b, err)
	}
	l := twoDisplays(image4864())
	go c.showMainDisplay(l)
	b = make([]byte, 8)
	if _, err := io.ReadFull(server, b); err != nil || b[0] != msgAppleSetDisplay || be.Uint32(b[4:]) != 3 {
		t.Fatalf("main display not selected: % x, %v", b, err)
	}
	// Once: later layouts, or the viewer's own choice, stand.
	c.showMainDisplay(l)
	c.economizeRFB()
	server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatalf("sent %d more bytes", n)
	}
}

func TestRemoteRFBKeepsViewerChoices(t *testing.T) {
	c, server := economyClient(t)
	c.cfg.AppleScale = 1 // the viewer asked for full size
	c.economy.picked.Store(true)
	done := make(chan struct{})
	go func() { c.economizeRFB(); c.showMainDisplay(twoDisplays(image4864())); close(done) }()
	server.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatalf("overrode the viewer's choices with %d bytes", n)
	}
	<-done
}

func TestLocalRFBKeepsFullPicture(t *testing.T) {
	c, server := economyClient(t)
	c.cfg.Network = NetworkLocal
	c.economizeRFB()
	server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatalf("scaled a local session (%d bytes)", n)
	}
}

func TestMacSessionsDefaultToMainDisplay(t *testing.T) {
	for _, network := range []string{NetworkLocal, NetworkRemote} {
		for _, media := range []bool{false, true} {
			name := fmt.Sprintf("%s/media=%v", network, media)
			t.Run(name, func(t *testing.T) {
				c, server := economyClient(t)
				c.cfg.Network, c.cfg.MediaStream = network, media
				layout := twoDisplays(image.Pt(9728, 2880))
				go c.showMainDisplay(layout)
				server.SetReadDeadline(time.Now().Add(time.Second))
				var message [8]byte
				if _, err := io.ReadFull(server, message[:]); err != nil {
					t.Fatal(err)
				}
				if message[0] != msgAppleSetDisplay || be.Uint32(message[4:]) != 3 {
					t.Fatalf("did not default to main display: % x", message)
				}
				// The user's All Displays choice must survive later layouts.
				go c.SelectDisplay(AllDisplays)
				if _, err := io.ReadFull(server, message[:]); err != nil || be.Uint32(message[4:]) != AllDisplays {
					t.Fatalf("could not select all displays: % x, %v", message, err)
				}
				c.showMainDisplay(layout)
				server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				if n, err := server.Read(message[:]); err == nil {
					t.Fatalf("overrode All Displays with %d bytes", n)
				}
			})
		}
	}
}

func image4864() image.Point { return image.Pt(4864, 1440) }
