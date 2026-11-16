//go:build windows || linux || android

package audio

import (
	"os"
	"testing"
	"time"
)

// Opt in on a machine with an audio device. This sends silence, checks that
// the native backend initializes and consumes PCM, and switches formats on
// the same process-wide context as a reconnect would.
func TestNativePCMPlayback(t *testing.T) {
	if os.Getenv("MIRAGE_AUDIO_TEST") != "1" {
		t.Skip("set MIRAGE_AUDIO_TEST=1 on a machine with an audio output")
	}
	for _, format := range [][2]int{{44100, 2}, {22050, 1}, {48000, 2}} {
		p, err := NewPCM(format[0], format[1])
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Push(make([]byte, format[0]/50*format[1]*2)); err != nil {
			p.Close()
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			status := p.Status()
			if status.Error != "" {
				p.Close()
				t.Fatal(status.Error)
			}
			if status.Running && status.Completed > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		status := p.Status()
		p.Close()
		if !status.Running || status.Completed == 0 {
			t.Fatalf("native output did not consume %v PCM: %+v", format, status)
		}
		if p.Status().Running {
			t.Fatal("closed player is still running")
		}
	}
}
