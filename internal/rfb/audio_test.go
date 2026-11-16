package rfb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestQEMUAudioNegotiationAndFraming(t *testing.T) {
	conn := &keyWriteConn{}
	var got []byte
	c := &Client{conn: conn, cfg: Config{OnAudio: func(rate, chs int, pcm []byte) {
		if rate != 48000 || chs != 2 {
			t.Errorf("format %d/%d", rate, chs)
		}
		got = append(got, pcm...)
	}}}
	if err := c.enableAudio(); err != nil {
		t.Fatal(err)
	}
	if err := c.enableAudio(); err != nil {
		t.Fatal(err)
	}
	want := []byte{255, 1, 0, 2, 3, 2, 0, 0, 0xbb, 0x80, 255, 1, 0, 0}
	if !bytes.Equal(conn.Bytes(), want) {
		t.Fatalf("negotiation %x", conn.Bytes())
	}
	c.r = bufio.NewReader(bytes.NewReader([]byte{255, 1, 0, 1, 255, 1, 0, 2, 0, 0, 0, 4, 1, 2, 3, 4, 255, 1, 0, 0, 2}))
	for range 4 {
		if _, err := c.readMessage(); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, []byte{1, 2, 3, 4}) || c.AudioStatus().Active || c.AudioStatus().ReceivedBytes != 4 {
		t.Fatalf("PCM/status %v %+v", got, c.AudioStatus())
	}
}

func TestQEMUAudioRejectsInvalidAndUnsolicitedPackets(t *testing.T) {
	for _, data := range [][]byte{{1, 0, 2, 0xff, 0xff, 0xff, 0xff}, {1, 0, 2, 0, 0, 0, 3}, {1, 0, 2, 0, 0, 0, 4, 1}, {1, 0, 9}, {2, 0, 1}} {
		c := &Client{r: bufio.NewReader(bytes.NewReader(data))}
		c.audio.supported.Store(true)
		if err := c.readAudio(); err == nil {
			t.Fatalf("accepted %x", data)
		}
	}
	c := &Client{r: bufio.NewReader(bytes.NewReader([]byte{1, 0, 1}))}
	if err := c.readAudio(); err == nil {
		t.Fatal("accepted unsolicited audio")
	}
	c = &Client{conn: &keyWriteConn{}}
	if err := c.enableAudio(); err != nil || c.AudioStatus().Supported {
		t.Fatal("audio enabled without callback")
	}
}

// Run against an input-disabled, loopback-only server while a known test tone
// is playing through its output monitor. No microphone or local playback.
func TestLiveVNCAudio(t *testing.T) {
	addr := os.Getenv("MIRAGE_TEST_VNC_AUDIO_ADDRESS")
	if addr == "" {
		t.Skip("requires isolated audio test server")
	}
	var received, nonzero atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started := time.Now()
	c, err := Dial(ctx, addr, Config{Encodings: append([]int32{EncH264, EncTight}, DefaultEncodings...), OnAudio: func(rate, chs int, pcm []byte) {
		received.Add(int64(len(pcm)))
		for _, b := range pcm {
			if b != 0 {
				nonzero.Add(1)
				break
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("audio timed out: %+v nonzero=%d", c.AudioStatus(), nonzero.Load())
		case <-ticker.C:
			if received.Load() >= 192000 && nonzero.Load() > 10 && time.Since(started) > 6*time.Second {
				if c.Encoding() != "H.264" || !c.VideoStatus().Hardware {
					t.Fatalf("video regressed: %s %+v", c.Encoding(), c.VideoStatus())
				}
				t.Logf("received=%d nonzero packets=%d status=%+v", received.Load(), nonzero.Load(), c.AudioStatus())
				return
			}
		}
	}
}

// The isolated capture helper emits 64 numbered 20 ms blocks in 160 ms
// bursts. No samples may be discarded just because several blocks arrive in
// one pipe read cycle. This models capture delayed by video processing.
func TestLiveVNCAudioBursts(t *testing.T) {
	addr := os.Getenv("MIRAGE_TEST_VNC_AUDIO_BURST_ADDRESS")
	if addr == "" {
		t.Skip("requires numbered burst capture helper")
	}
	var packets atomic.Int64
	var invalid atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, Config{OnAudio: func(_, _ int, pcm []byte) {
		index := packets.Add(1)
		if len(pcm) != 3840 {
			invalid.Store(true)
			return
		}
		for i := 0; i < len(pcm); i += 2 {
			if int64(binary.LittleEndian.Uint16(pcm[i:])) != index {
				invalid.Store(true)
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("burst capture incomplete: packets=%d invalid=%v", packets.Load(), invalid.Load())
		case <-ticker.C:
			if packets.Load() >= 64 {
				if packets.Load() != 64 || invalid.Load() {
					t.Fatalf("PCM discontinuity: packets=%d invalid=%v", packets.Load(), invalid.Load())
				}
				t.Log("64 consecutive PCM blocks received intact through capture bursts")
				return
			}
		}
	}
}
