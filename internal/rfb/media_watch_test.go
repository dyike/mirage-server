package rfb

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/dyike/mirage-server/internal/frame"
)

func TestMediaWatchFallsBackWithoutFirstPicture(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	wait := mediaTimings().wait
	if v := w.check(start.Add(wait-time.Millisecond), 0, 0, 3); v != mediaOK {
		t.Fatalf("before the wait: %v", v)
	}
	if v := w.check(start.Add(wait), 0, 0, 4); v != mediaFallback {
		t.Fatalf("at the wait without a picture: %v, want fallback", v)
	}
}

// replay feeds the watch one check per half second from start to end,
// with counters from at, and collects the verdicts that are not mediaOK.
func replay(w *mediaWatch, start time.Time, from, to time.Duration, at func(d time.Duration) (units, packets, rtcp int64)) map[time.Duration]mediaVerdict {
	got := map[time.Duration]mediaVerdict{}
	for d := from; d <= to; d += 500 * time.Millisecond {
		u, p, r := at(d)
		if v := w.check(start.Add(d), u, p, r); v != mediaOK {
			got[d] = v
		}
	}
	return got
}

// A still or sleeping display sends no video for a long time but keeps
// sending reports: one keyframe request, no fallback.
func TestMediaWatchStillDisplayKeepsStream(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	got := replay(w, start, 0, 60*time.Second, func(d time.Duration) (int64, int64, int64) {
		if d < time.Second {
			return int64(d / (100 * time.Millisecond)), int64(d / (10 * time.Millisecond)), 0
		}
		return 10, 100, int64(d / time.Second) // pictures stop, reports go on
	})
	if len(got) != 1 || got[time.Second+mediaStallProbe] != mediaProbe {
		t.Fatalf("verdicts %v, want one probe %v after the last picture", got, mediaStallProbe)
	}
}

func TestMediaWatchFallsBackWhenEverythingStops(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	got := replay(w, start, 0, 20*time.Second, func(d time.Duration) (int64, int64, int64) {
		if d < time.Second {
			return int64(d / (100 * time.Millisecond)), int64(d / (10 * time.Millisecond)), 1
		}
		return 10, 100, 1 // the path died: no video, no reports
	})
	first := 20 * time.Second
	for d, v := range got {
		if v == mediaFallback && d < first {
			first = d
		}
	}
	if want := time.Second + mediaStallLimit; first != want {
		t.Fatalf("fell back at %v, want %v (verdicts %v)", first, want, got)
	}
}

func TestMediaWatchFallsBackWhenVideoDecodesToNothing(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	got := replay(w, start, 0, 12*time.Second, func(d time.Duration) (int64, int64, int64) {
		units := min(int64(d/(100*time.Millisecond)), 10)
		return units, int64(d / (10 * time.Millisecond)), int64(d / time.Second) // packets keep coming
	})
	if got[time.Second+mediaStallLimit] != mediaFallback {
		t.Fatalf("verdicts %v, want a fallback at %v", got, time.Second+mediaStallLimit)
	}
}

func TestMediaWatchStragglerIsNotUndecodable(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	got := replay(w, start, 0, 30*time.Second, func(d time.Duration) (int64, int64, int64) {
		packets := int64(100)
		if d >= 2*time.Second {
			packets = 101 // one late packet after the last picture
		}
		units := min(int64(d/(100*time.Millisecond)), 10)
		return units, packets, int64(d / time.Second)
	})
	for d, v := range got {
		if v == mediaFallback {
			t.Fatalf("fell back at %v", d)
		}
	}
}

func TestMediaWatchAnsweredProbeKeepsStream(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	w.check(start, 1, 10, 0)
	if v := w.check(start.Add(mediaStallProbe), 1, 10, 3); v != mediaProbe {
		t.Fatalf("no probe: %v", v)
	}
	// The keyframe the probe asked for arrives: the next stall probes again.
	w.check(start.Add(mediaStallProbe+100*time.Millisecond), 2, 20, 3)
	if v := w.check(start.Add(2*mediaStallProbe+100*time.Millisecond), 2, 20, 6); v != mediaProbe {
		t.Fatalf("second still period not probed: %v", v)
	}
}

func TestMediaWatchCongestion(t *testing.T) {
	start := time.Now()
	w := newMediaWatch(start)
	var packets, lost int64
	for d := time.Duration(0); d <= 5*time.Second; d += 500 * time.Millisecond {
		packets += 100
		if d >= 2*time.Second {
			lost += 5 // 5%: tolerable
		}
		if w.congested(start.Add(d), packets, lost, 0) {
			t.Fatalf("congested at %v with 5%% loss", d)
		}
	}
	for d := 5500 * time.Millisecond; d <= 8*time.Second; d += 500 * time.Millisecond {
		packets += 70
		lost += 30
		if w.congested(start.Add(d), packets, lost, 0) {
			return
		}
	}
	t.Fatal("30% loss never counted as congestion")
}

func TestLowerBitrate(t *testing.T) {
	steps := []uint64{remoteBitrates[0]}
	for b := lowerBitrate(steps[0]); b > 0; b = lowerBitrate(b) {
		steps = append(steps, b)
	}
	if len(steps) != len(remoteBitrates) || steps[len(steps)-1] != 600_000 {
		t.Fatalf("ladder %v", steps)
	}
	if lowerBitrate(0) != remoteBitrates[0] {
		t.Fatal("an uncapped stream cannot back off on congestion")
	}
}

func TestMediaWatchQueueCongestionWithoutLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		lag  func(time.Duration) time.Duration
		pps  int64
		want bool
	}{
		{"sustained queue", func(time.Duration) time.Duration { return 300 * time.Millisecond }, 100, true},
		{"short burst", func(d time.Duration) time.Duration {
			if d == 2*time.Second {
				return time.Second
			}
			return 0
		}, 100, false},
		{"healthy link", func(time.Duration) time.Duration { return 20 * time.Millisecond }, 100, false},
		{"idle desktop", func(time.Duration) time.Duration { return time.Second }, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			w := newMediaWatch(start)
			got := false
			for d := time.Duration(0); d <= 5*time.Second; d += 500 * time.Millisecond {
				if w.congested(start.Add(d), int64(d/time.Millisecond)*tc.pps/1000, 0, tc.lag(d)) {
					got = true
					break
				}
			}
			if got != tc.want {
				t.Fatalf("congested=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestMediaQueueLagExpiresAndTracksRTPDelay(t *testing.T) {
	start := time.Now()
	l := &mediaLeg{}
	if got := l.queueLag(start); got != 0 {
		t.Fatalf("no packets yet: delay %v", got)
	}
	l.rate.epoch = start
	for i := range 201 {
		// The sender produces a frame every 20 ms, but a bottleneck delivers
		// them every 25 ms. No packet is lost while the queue keeps growing.
		l.rate.record(uint16(i), uint32(i*videoClock/50), start.Add(time.Duration(i)*25*time.Millisecond))
	}
	now := start.Add(5 * time.Second)
	if got := l.queueLag(now); got < congestionDelay {
		t.Fatalf("queue growth was missed: delay %v", got)
	}
	if got := l.queueLag(now.Add(2 * time.Second)); got != 0 {
		t.Fatalf("idle desktop kept a stale delay: %v", got)
	}
}

func TestMediaFailureReasons(t *testing.T) {
	for _, c := range []struct {
		units int64
		leg   legCounts
		want  string
	}{
		{0, legCounts{}, "no UDP video"},
		{0, legCounts{rtcp: 4}, "MTU"},
		{0, legCounts{refused: 2}, "refused"},
		{0, legCounts{sendErrs: 2}, "could not send"},
		{0, legCounts{bad: 3}, "decrypted"},
		{0, legCounts{packets: 9}, "none decoded"},
		{4, legCounts{packets: 9}, "stalled"},
	} {
		got := mediaFailure(c.units, c.leg)
		if !strings.Contains(got, c.want) || !strings.Contains(got, fmt.Sprintf("rtp %d, rtcp %d", c.leg.packets, c.leg.rtcp)) {
			t.Errorf("mediaFailure(%d, %+v) = %q, want %q and the counters", c.units, c.leg, got, c.want)
		}
	}
}

// shortMediaTimes speeds the watch up for one test.
func shortMediaTimes(t *testing.T) {
	old := mediaTimes.Swap(&mediaTiming{wait: 300 * time.Millisecond, tick: 20 * time.Millisecond})
	t.Cleanup(func() { mediaTimes.Store(old) })
}

// expectRFBResumed reads what the client sends after a fallback: encodings
// without the MediaStream one, then a full-screen update request.
func expectRFBResumed(t *testing.T, server net.Conn, encs []int32) {
	t.Helper()
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 4)
	if _, err := io.ReadFull(server, head); err != nil {
		t.Fatalf("no encodings after the fallback: %v", err)
	}
	if head[0] != 2 {
		t.Fatalf("message %d, want SetEncodings", head[0])
	}
	body := make([]byte, 4*int(be.Uint16(head[2:])))
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(body); i += 4 {
		if int32(be.Uint32(body[i:])) == EncAppleMediaStream {
			t.Fatal("fallback kept the MediaStream encoding")
		}
	}
	if got := int32(be.Uint32(body)); got != encs[0] {
		t.Fatalf("first encoding %d, want %d", got, encs[0])
	}
	req := make([]byte, 10)
	if _, err := io.ReadFull(server, req); err != nil {
		t.Fatalf("no update request after the fallback: %v", err)
	}
	if req[0] != 3 || req[1] != 0 || be.Uint16(req[6:]) != 640 || be.Uint16(req[8:]) != 480 {
		t.Fatalf("update request % x, want a full 640x480 one", req)
	}
}

func TestMediaFallbackExplicitlyRearmsPolling(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	encs := []int32{EncAppleRGB555, EncZRLE, EncRaw}
	c := &Client{conn: client, apple: true, appleExt: true, appleEncs: encs,
		cfg: Config{Network: NetworkRemote, AppleScale: 1}}
	defer c.Close()
	c.appleCmds[msgAppleAutoUpdate/8] |= 0x80 >> (msgAppleAutoUpdate % 8)
	c.size.Store(uint64(640)<<32 | 480)
	c.media.Store(&mediaState{})
	done := make(chan struct{})
	go func() { c.stopMedia("no UDP picture", true); close(done) }()
	expectRFBResumed(t, server, encs)
	arm := make([]byte, 16)
	server.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(server, arm); err != nil {
		t.Fatalf("fallback did not restore polling mode: %v", err)
	}
	if arm[0] != msgAppleAutoUpdate || be.Uint32(arm[4:]) != ^uint32(0) ||
		be.Uint16(arm[12:]) != 640 || be.Uint16(arm[14:]) != 480 || c.pushing.Load() {
		t.Fatalf("fallback did not disable periodic pushes for the whole framebuffer: % x", arm)
	}
	<-done
}

// A server that names its ports but whose UDP never reaches the viewer (a
// NAT or firewall in between) must not leave the screen black.
func TestMediaFallsBackWhenNoUDPVideoArrives(t *testing.T) {
	shortMediaTimes(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		s, _ := ln.Accept()
		accepted <- s
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close()
	// A port nobody sends from: datagrams to it are dropped.
	probe, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.LocalAddr().(*net.UDPAddr).Port)
	probe.Close()

	var published []MediaDiagnostics
	diag := make(chan MediaDiagnostics, 16)
	encs := []int32{EncZRLE, EncRaw}
	c := &Client{conn: conn, appleEncs: encs, cfg: Config{OnMediaDiagnostics: func(d MediaDiagnostics) { diag <- d }}}
	defer c.Close()
	c.size.Store(uint64(640)<<32 | 480)
	m := &mediaState{offer: newMediaOffer(negotiatorNearby, []int{payloadHEVC}, true), portVideo: port}
	c.media.Store(m)
	m.opened.Store(true)
	if err := c.openVideo(m); err != nil {
		t.Fatal(err)
	}
	expectRFBResumed(t, server, encs)
	// Complete the fallback with an actual ordinary framebuffer update. A
	// successful SetEncodings alone does not establish that pixels resume.
	c.fb.Resize(640, 480)
	c.cov.reset(640, 480)
	c.r = bufio.NewReader(conn)
	var picture []byte
	c.cfg.OnUpdate = func(fb *frame.Framebuffer, _ []image.Rectangle) {
		picture = bytes.Clone(fb.Pix[:4])
	}
	update := make([]byte, 16+640*480*4)
	be.PutUint16(update[2:], 1)
	be.PutUint16(update[8:], 640)
	be.PutUint16(update[10:], 480)
	for i := 16; i < len(update); i += 4 {
		copy(update[i:], []byte{20, 80, 140, 0})
	}
	written := make(chan error, 1)
	server.SetWriteDeadline(time.Now().Add(5 * time.Second))
	go func() { _, err := server.Write(update); written <- err }()
	if _, err := c.readMessage(); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(picture, []byte{20, 80, 140, 255}) {
		t.Fatalf("fallback did not deliver opaque pixels: %v", picture)
	}
	if c.media.Load() != nil {
		t.Fatal("media state kept after the fallback")
	}
	for len(diag) > 0 {
		published = append(published, <-diag)
	}
	last := c.MediaDiagnostics()
	// On loopback the leg's socket hears its own reports, so only the
	// missing video is certain.
	if !strings.Contains(last.Fallback, "no picture") || !strings.Contains(last.Fallback, "rtp 0,") || !strings.HasPrefix(last.State, "Stopped: ") {
		t.Fatalf("diagnostics %+v, want a no-UDP fallback", last)
	}
	if len(published) == 0 || published[len(published)-1].Fallback == "" {
		t.Fatal("the fallback was not published to the UI")
	}
}

// A server that never names its ports must not hold the picture either.
func TestMediaFallsBackWithoutPorts(t *testing.T) {
	shortMediaTimes(t)
	client, server := net.Pipe()
	defer server.Close()
	encs := []int32{EncZRLE}
	c := &Client{conn: client, appleEncs: encs}
	defer c.Close()
	c.size.Store(uint64(640)<<32 | 480)
	m := &mediaState{offer: newMediaOffer(negotiatorNearby, []int{payloadHEVC}, true)}
	c.media.Store(m)
	c.watchNegotiation(m)
	expectRFBResumed(t, server, encs)
	if d := c.MediaDiagnostics(); !strings.Contains(d.Fallback, "no media ports") {
		t.Fatalf("fallback %q", d.Fallback)
	}
	// Ports that come late open nothing.
	if m.opened.CompareAndSwap(false, true) {
		t.Fatal("late ports could still open the video leg")
	}
}

func TestMediaNegotiationWatchIgnoresOpenedStream(t *testing.T) {
	shortMediaTimes(t)
	client, server := net.Pipe()
	defer server.Close()
	c := &Client{conn: client}
	defer c.Close()
	m := &mediaState{}
	c.media.Store(m)
	m.opened.Store(true) // the ports came in time
	c.watchNegotiation(m)
	server.SetReadDeadline(time.Now().Add(2 * mediaTimings().wait))
	if n, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatalf("negotiation watch sent %d bytes for an opened stream", n)
	}
	if c.media.Load() != m {
		t.Fatal("negotiation watch stopped an opened stream")
	}
}

func seqs(b *reorderBuffer, in []uint16, now time.Time) []uint16 {
	var out []uint16
	for _, s := range in {
		h := rtp.Header{SequenceNumber: s}
		b.push(&h, nil, now, func(h *rtp.Header, _ []byte) { out = append(out, h.SequenceNumber) })
	}
	return out
}

func equalSeqs(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReorderBufferRestoresOrder(t *testing.T) {
	var b reorderBuffer
	now := time.Now()
	got := seqs(&b, []uint16{10, 12, 11, 13, 15, 14, 14, 9}, now)
	if want := []uint16{10, 11, 12, 13, 14, 15}; !equalSeqs(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if b.late != 2 {
		t.Fatalf("late %d, want the second 14 and the stale 9 dropped", b.late)
	}
}

func TestReorderBufferWrapsAround(t *testing.T) {
	var b reorderBuffer
	got := seqs(&b, []uint16{65534, 0, 65535, 1}, time.Now())
	if want := []uint16{65534, 65535, 0, 1}; !equalSeqs(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReorderBufferGivesUpOnLostPackets(t *testing.T) {
	var b reorderBuffer
	now := time.Now()
	in := []uint16{100}
	for s := uint16(102); s < 102+reorderDepth; s++ { // 101 is lost
		in = append(in, s)
	}
	got := seqs(&b, in, now)
	if len(got) != len(in) || got[0] != 100 || got[1] != 102 || b.held != 0 {
		t.Fatalf("after %d packets past the gap: handed on %v, %d held", reorderDepth, got, b.held)
	}
	// The assembler sees the gap and drops the broken access unit.
	a := &auAssembler{}
	if _, lost := a.push(&rtp.Header{SequenceNumber: 100}, nil); lost {
		t.Fatal("first packet lost")
	}
	if _, lost := a.push(&rtp.Header{SequenceNumber: 102}, nil); !lost {
		t.Fatal("assembler missed the gap")
	}
}

func TestReorderBufferGivesUpAfterDelay(t *testing.T) {
	var b reorderBuffer
	now := time.Now()
	got := seqs(&b, []uint16{1, 3}, now)
	got = append(got, seqs(&b, []uint16{4}, now.Add(reorderDelay))...)
	if want := []uint16{1, 3, 4}; !equalSeqs(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReorderBufferJumps(t *testing.T) {
	var b reorderBuffer
	now := time.Now()
	got := seqs(&b, []uint16{1, 3, 1000, 1001}, now)
	if want := []uint16{1, 3, 1000, 1001}; !equalSeqs(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// portsReply is a MediaStream ports pseudo-rectangle body naming the
// audio and video ports, as mediaReply reads it.
func portsReply(audio, video uint16) []byte {
	body := make([]byte, 26)
	be.PutUint16(body, 1)
	be.PutUint16(body[8:], audio)
	be.PutUint32(body[10:], 1)
	be.PutUint16(body[14:], video)
	be.PutUint32(body[16:], 1)
	return append(be.AppendUint16(nil, uint16(len(body))), body...)
}

func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	return uint16(u.LocalAddr().(*net.UDPAddr).Port)
}

func readOffer(t *testing.T, server net.Conn) []byte {
	t.Helper()
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 4)
	if _, err := io.ReadFull(server, head); err != nil {
		t.Fatalf("no offer: %v", err)
	}
	if head[0] != msgAppleMediaStream {
		t.Fatalf("message %#x, want a media offer", head[0])
	}
	body := make([]byte, be.Uint16(head[2:]))
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	return append(head, body...)
}

func TestMediaWatchChoosesBitrateOrRFB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cap, want uint64
		loss      int64
		delay     float64
		pictures  bool
	}{
		{"uncapped queued VPN", 0, 3_000_000, 0, 0.3, true},
		{"uncapped lossy link", 0, 3_000_000, 30, 0, true},
		{"no first picture", 1_800_000, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortMediaTimes(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			server, err := ln.Accept()
			if err != nil {
				conn.Close()
				t.Fatal(err)
			}
			defer server.Close()
			encs := []int32{EncAppleRGB555, EncZRLE, EncRaw}
			c := &Client{conn: conn, appleEncs: encs, cfg: Config{NoAudio: true, Network: NetworkLocal}}
			defer c.Close()
			c.size.Store(uint64(640)<<32 | 480)
			m := &mediaState{offer: newMediaOffer(negotiatorNearby, []int{payloadHEVC}, true), portVideo: freeUDPPort(t)}
			m.offer.maxBitrate = tc.cap
			c.media.Store(m)
			if err := c.openVideo(m); err != nil {
				t.Fatal(err)
			}
			stop, exited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(exited)
				tick := time.NewTicker(20 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case now := <-tick.C:
						m.video.packets.Add(100)
						m.lost.Add(tc.loss)
						if tc.pictures {
							m.units.Add(1)
						}
						m.video.statsMu.Lock()
						m.video.rate.have, m.video.rate.sampleAt = true, now
						m.video.rate.delay.value = tc.delay
						m.video.statsMu.Unlock()
					}
				}
			}()
			defer func() { close(stop); <-exited }()
			if !tc.pictures {
				// Receiving packets is not evidence that the device can decode
				// them. The first-picture timeout must resume ordinary pictures.
				expectRFBResumed(t, server, encs)
				if c.media.Load() != nil || c.MediaDiagnostics().Fallback == "" {
					t.Fatal("undecodable startup did not fall back")
				}
				return
			}
			readOffer(t, server) // A new video offer, rather than RFB SetEncodings.
			next := c.media.Load()
			if next == nil || next == m || next.offer.maxBitrate != tc.want || next.offer.mode != negotiatorRemoteDesktop {
				t.Fatalf("stream did not switch to a capped RemoteDesktop offer: %+v", next)
			}
			if d := c.MediaDiagnostics(); d.MaxBitrate != tc.want || d.Fallback != "" {
				t.Fatalf("diagnostics did not reflect the reduced video rate: %+v", d)
			}
			if next.firstPictureBy != m.firstPictureBy || !next.hadPicture {
				t.Fatal("renegotiation lost picture progress")
			}
		})
	}
}

// After a display change macOS names its ports again and waits for a new
// offer; ignoring it left the video frozen.
func TestMediaRenegotiatesWhenPortsComeAgain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		s, _ := ln.Accept()
		accepted <- s
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close()
	c := &Client{conn: conn, cfg: Config{NoAudio: true}}
	defer c.Close()
	first := &mediaState{offer: newMediaOffer(negotiatorRemoteDesktop, []int{payloadHEVC}, true)}
	first.offer.maxBitrate = 1_800_000
	c.media.Store(first)
	port := freeUDPPort(t)

	feed := func(b []byte) {
		t.Helper()
		c.r = bufio.NewReader(bytes.NewReader(b))
		if err := c.mediaReply(); err != nil {
			t.Fatal(err)
		}
	}
	feed(portsReply(0, port))
	offer1 := readOffer(t, server)
	feed(portsReply(0, port))
	offer2 := readOffer(t, server)
	// Reading the TCP offer can finish before the sending goroutine returns
	// from Write and clears its restart guard. Wait for that completion before
	// sending another ports reply; otherwise the test races the guard.
	waitRestart := func(m *mediaState) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for m != nil && m.restarting.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if m == nil || m.restarting.Load() {
			t.Fatal("restart did not finish")
		}
	}

	second := c.media.Load()
	waitRestart(second)
	if second == first || second == nil {
		t.Fatal("ports named again did not start a new stream")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("the old video leg still runs")
	}
	if bytes.Equal(offer1, offer2) || second.offer.videoSend == first.offer.videoSend {
		t.Fatal("the new offer reused the old keys")
	}
	if second.offer.maxBitrate != first.offer.maxBitrate || second.offer.mode != first.offer.mode {
		t.Fatalf("restart changed the plan: mode %d cap %d", second.offer.mode, second.offer.maxBitrate)
	}
	if second.firstPictureBy != first.firstPictureBy || second.hadPicture {
		t.Fatal("renegotiation restarted the first-picture deadline")
	}
	// A third naming restarts again rather than being dropped.
	feed(portsReply(0, port))
	readOffer(t, server)
	third := c.media.Load()
	waitRestart(third)
	if third == second {
		t.Fatal("second restart ignored")
	}
}
