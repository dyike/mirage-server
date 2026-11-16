//go:build ios

package audio

import (
	"testing"
	"time"

	"github.com/ebitengine/purego/objc"
)

func TestIOSPlaybackSession(t *testing.T) {
	// Reading diagnostics must not acquire a playback session.
	if s := CurrentOutput(); !s.Available || s.Error != "" {
		t.Fatalf("output diagnostics: %+v", s)
	}
	if outputUsers != 0 {
		t.Fatal("diagnostics activated playback")
	}
	p, err := NewPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q, err := NewPCM(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if s := CurrentOutput(); s.Category != "AVAudioSessionCategoryPlayback" || s.Error != "" {
		t.Fatalf("playback category not configured: %+v", s)
	}
	p.Close()
	p.Close()
	if outputUsers != 1 {
		t.Fatal("closing one player released another player's session")
	}
	if err := q.Push(make([]byte, 3840)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && q.Status().Completed == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if s := q.Status(); s.Completed != 1 || s.Error != "" {
		t.Fatalf("remaining player stopped: %+v", s)
	}
	q.Close()
	q.Close()
	if outputUsers != 0 {
		t.Fatal("last player retained the audio session")
	}
}

func TestIOSPlaybackResumesWithInactiveSession(t *testing.T) {
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
		if st := aq.stop(p.queue, true); st != 0 {
			t.Fatal(st)
		}
		// Exercise a real AVAudioSession deactivation, while retaining the
		// same Player and ownership count, as happens during suspension.
		if err := withOutputSession(func(session objc.ID) error {
			var nativeError objc.ID
			if !objc.Send[bool](session, objc.RegisterName("setActive:error:"), false, &nativeError) {
				return outputSessionError("test deactivation", nativeError)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(idleTimeout + 100*time.Millisecond)
		if err := p.Push(packet); err != nil {
			t.Fatal(err)
		}
		waitForAudioBuffers(t, p, int64(cycle+2))
		if outputUsers != 1 {
			t.Fatal("recovery leaked audio session ownership")
		}
	}
}
