package rfb

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestPictureWatchIgnoresIdleAndBoundsRefreshes(t *testing.T) {
	var watch pictureWatch
	now := time.Unix(100, 0)
	if attempt, _ := watch.due(now); attempt != 0 {
		t.Fatal("idle desktop triggered recovery")
	}
	watch.frame(now)
	watch.input(now)
	for _, test := range []struct {
		after time.Duration
		want  int
	}{
		{time.Second, 0}, {1600 * time.Millisecond, 1}, {2 * time.Second, 0}, {4 * time.Second, 2}, {6 * time.Second, 3}, {7 * time.Second, 0}, {9 * time.Second, 0},
	} {
		if got, _ := watch.due(now.Add(test.after)); got != test.want {
			t.Fatalf("at %v got attempt %d want %d", test.after, got, test.want)
		}
	}
	watch.input(now.Add(10 * time.Second))
	if got, _ := watch.due(now.Add(12 * time.Second)); got != 1 {
		t.Fatal("new input burst did not rearm bounded recovery")
	}
}

func TestPictureWatchAllowsTransitionFramesBeforeAStall(t *testing.T) {
	var watch pictureWatch
	now := time.Unix(100, 0)
	watch.input(now)
	watch.frame(now.Add(time.Second)) // login animation, then push stops
	if got, _ := watch.due(now.Add(2 * time.Second)); got != 0 {
		t.Fatal("fresh frame triggered recovery")
	}
	if got, _ := watch.due(now.Add(3 * time.Second)); got != 1 {
		t.Fatal("an intermediate animation frame disabled stall recovery")
	}
	watch.frame(now.Add(4 * time.Second))
	if got, _ := watch.due(now.Add(5 * time.Second)); got != 0 {
		t.Fatal("ongoing frames did not suppress recovery")
	}
}

func TestPictureRecoveryOnlyRequestsPixelsAndRearmsPush(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := &Client{conn: client}
	defer c.Close()
	c.pushing.Store(true)
	c.size.Store(uint64(1280)<<32 | 720)
	now := time.Now()
	c.pictureWatch.frame(now.Add(-3 * time.Second))
	c.pictureWatch.input(now.Add(-3 * time.Second))
	done := make(chan struct{})
	go func() { c.recoverPicture(now); close(done) }()
	server.SetReadDeadline(time.Now().Add(time.Second))
	packet := (&fakeServer{t: t, conn: server}).read(26)
	if packet[0] != 3 || packet[1] != 0 || packet[10] != msgAppleAutoUpdate || be.Uint16(packet[6:]) != 1280 || be.Uint16(packet[8:]) != 720 {
		t.Fatalf("recovery sent input or incorrect refresh: %x", packet)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery sent unexpected extra traffic")
	}
	c.closing.Store(true)
	c.recoverPicture(now.Add(3 * time.Second)) // must not send anything on exit
}

func TestRunFailureClosesOldMediaAndConnection(t *testing.T) {
	c, err := pipe(t, Config{}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(1, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	decoderClosed := false
	m := &mediaState{video: &mediaLeg{conn: udp}, done: make(chan struct{}), exited: make(chan struct{}), closeDecoder: func() { decoderClosed = true }}
	close(m.exited)
	c.media.Store(m)
	if err := c.Run(); err == nil {
		t.Fatal("server close unexpectedly succeeded")
	}
	if !c.closing.Load() || !decoderClosed {
		t.Fatal("read failure retained the old session resources")
	}
	select {
	case <-m.done:
	default:
		t.Fatal("media receiver not stopped")
	}
	if _, err := c.conn.Write([]byte{1}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("old TCP socket remained open: %v", err)
	}
	if _, _, err := udp.ReadFromUDP(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("old media socket remained open: %v", err)
	}
	c.Close() // idempotent during UI / reader teardown
}
