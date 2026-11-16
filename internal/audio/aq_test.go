//go:build darwin

package audio

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// eldClip writes one second of stereo silence, encodes it to AAC-ELD with
// afconvert, and returns the CAF file's packets and magic cookie.
func eldClip(t *testing.T) (packets [][]byte, cookie []byte, framesPerPacket uint32) {
	if runtime.GOOS != "darwin" {
		t.Skip("AudioToolbox only")
	}
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "silence.wav")
	const rate, frames = 48000, 48000
	var w bytes.Buffer
	data := make([]byte, frames*4) // 16-bit stereo zeros
	w.WriteString("RIFF")
	binary.Write(&w, binary.LittleEndian, uint32(36+len(data)))
	w.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(2), uint32(rate), uint32(rate * 4), uint16(4), uint16(16)} {
		binary.Write(&w, binary.LittleEndian, v)
	}
	w.WriteString("data")
	binary.Write(&w, binary.LittleEndian, uint32(len(data)))
	w.Write(data)
	os.WriteFile(wav, w.Bytes(), 0o600)
	caf := filepath.Join(dir, "eld.caf")
	if out, err := exec.Command("afconvert", "-f", "caff", "-d", "aace@48000", "-c", "2", wav, caf).CombinedOutput(); err != nil {
		t.Skipf("afconvert: %v %s", err, out)
	}
	b, _ := os.ReadFile(caf)
	var sizes []int
	var body []byte
	for p := b[8:]; len(p) >= 12; {
		kind, n := string(p[:4]), int(binary.BigEndian.Uint64(p[4:]))
		chunk := p[12:min(len(p), 12+n)]
		switch kind {
		case "desc":
			framesPerPacket = binary.BigEndian.Uint32(chunk[20:])
		case "kuki":
			cookie = chunk
		case "pakt":
			count := int(binary.BigEndian.Uint64(chunk))
			q := chunk[24:]
			for range count {
				v := 0
				for len(q) > 0 {
					c := q[0]
					q = q[1:]
					v = v<<7 | int(c&0x7f)
					if c < 0x80 {
						break
					}
				}
				sizes = append(sizes, v)
			}
		case "data":
			body = chunk[4:] // after the edit count
		}
		if n < 0 || 12+n > len(p) {
			break
		}
		p = p[12+n:]
	}
	for _, n := range sizes {
		if n > len(body) {
			break
		}
		packets = append(packets, body[:n])
		body = body[n:]
	}
	if len(packets) == 0 || cookie == nil {
		t.Fatalf("CAF parse: %d packets, cookie %d bytes", len(packets), len(cookie))
	}
	return packets, cookie, framesPerPacket
}

func TestPlayAACELD(t *testing.T) {
	packets, cookie, fpp := eldClip(t)
	t.Logf("%d packets of %d frames, cookie % x", len(packets), fpp, cookie)
	p, err := NewAACELD(Format{SampleRate: 48000, Channels: 2, FramesPerPacket: int(fpp), Cookie: cookie})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Push in real time; the queue hands buffers back as it plays them,
	// so nothing is dropped once playback runs.
	tick := time.Duration(fpp) * time.Second / 48000
	for _, pkt := range packets {
		if err := p.Push(pkt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(tick)
	}
	if d := p.Dropped(); d > int64(bufferCount) {
		t.Errorf("%d of %d packets dropped: the queue is not playing", d, len(packets))
	}
	status := p.Status()
	if status.Enqueued == 0 || !status.StartRequested || status.Error != "" {
		t.Fatalf("invalid playback diagnostics: %+v", status)
	}
}

// TestRemoteDesktopCookie: the queue accepts the cookie built for macOS's
// system audio stream (480-sample AAC-ELD frames).
func TestRemoteDesktopCookie(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("AudioToolbox only")
	}
	p, err := NewAACELD(Format{SampleRate: 48000, Channels: 2, FramesPerPacket: 480, Cookie: AACELDCookie(RemoteDesktopELD)})
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
}

// TestPlayPCM queues 0.25 s of silent 48 kHz stereo in uneven pushes, one
// larger than a buffer, and checks the queue starts without errors.
func TestPlayPCM(t *testing.T) {
	p, err := NewPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, n := range []int{3840, 40000, 3840} { // 20 ms, ~208 ms, 20 ms
		if err := p.Push(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.Status().Completed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	s := p.Status()
	if s.Error != "" || s.Enqueued != 4 || !s.StartRequested || s.Completed == 0 {
		t.Fatalf("PCM playback %+v", s)
	}
	if _, err := NewPCM(48000, 3); err == nil {
		t.Fatal("accepted 3 channels")
	}
}

// A remote notification can fit in one packet, with no later sound to
// satisfy a multi-buffer startup threshold.
func TestPlayPCMShortSound(t *testing.T) {
	p, err := NewPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Push(make([]byte, 3840)); err != nil { // one 20 ms packet
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.Status().Completed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	s := p.Status()
	if s.Error != "" || !s.StartRequested || s.Completed != 1 {
		t.Fatalf("short PCM sound did not play: %+v", s)
	}
}

func waitForAudioBuffers(t *testing.T, p *Player, completed int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.Status().Completed < completed {
		time.Sleep(10 * time.Millisecond)
	}
	if s := p.Status(); s.Completed < completed || s.Error != "" {
		t.Fatalf("audio did not complete %d buffers: %+v", completed, s)
	}
}

func TestPCMResumesAfterQueueStops(t *testing.T) {
	p, err := NewPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	packet := make([]byte, 3840)
	if err := p.Push(packet); err != nil {
		t.Fatal(err)
	}
	waitForAudioBuffers(t, p, 1)
	for cycle := range 3 {
		// Model the system stopping an already-started queue. Playback
		// must recover on the next packet, without replacing the player.
		if st := aq.stop(p.queue, true); st != 0 {
			t.Fatal(st)
		}
		time.Sleep(150 * time.Millisecond)
		if err := p.Push(packet); err != nil {
			t.Fatal(err)
		}
		waitForAudioBuffers(t, p, int64(cycle+2))
	}
}

func TestAACELDResumesAfterSilence(t *testing.T) {
	packets, cookie, fpp := eldClip(t)
	p, err := NewAACELD(Format{SampleRate: 48000, Channels: 2, FramesPerPacket: int(fpp), Cookie: cookie})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tick := time.Duration(fpp) * time.Second / 48000
	for cycle := range 3 {
		before := p.Status().Completed
		for _, pkt := range packets[:20] {
			if err := p.Push(pkt); err != nil {
				t.Fatal(err)
			}
			time.Sleep(tick)
		}
		waitForAudioBuffers(t, p, before+10)
		time.Sleep(idleTimeout + 100*time.Millisecond)
		if cycle == 0 {
			// Returning from suspension can leave a formerly running
			// AudioQueue stopped as well as its decoder starved.
			if st := aq.stop(p.queue, true); st != 0 {
				t.Fatal(st)
			}
		}
	}
	if p.Status().Dropped != 0 {
		t.Fatal("resumed AAC exhausted its buffers", p.Status())
	}
}

func TestNetworkPCMPrerollAndUnderrunRecovery(t *testing.T) {
	p, err := NewNetworkPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	packet := make([]byte, 3840)
	for cycle := range 6 {
		before := p.Status().Completed
		// First packet detects the previous underrun and raises the preroll.
		if err := p.Push(packet); err != nil {
			t.Fatal(err)
		}
		packets := p.primeBytes / len(packet)
		if packets > 10 || packets < 3 {
			t.Fatalf("unbounded preroll: %d", packets)
		}
		for i := 1; i < packets; i++ {
			if p.started {
				t.Fatalf("cycle %d started before preroll", cycle)
			}
			if err := p.Push(packet); err != nil {
				t.Fatal(err)
			}
		}
		if !p.started {
			t.Fatal("preroll did not start")
		}
		waitForAudioBuffers(t, p, before+int64(packets))
	}
	if p.Status().Dropped != 0 || p.Status().Underruns != 5 {
		t.Fatal("recovery counters", p.Status())
	}
}
