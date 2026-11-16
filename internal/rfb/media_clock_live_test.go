package rfb

import (
	"bufio"
	"context"
	"image"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/video"
)

func TestMediaClockLiveServer(t *testing.T) {
	if os.Getenv("MIRAGE_LIVE_CLOCK_TEST") != "1" {
		t.Skip("explicit live clock probe only")
	}
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".config/mirage/test.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	var mu sync.Mutex
	var count int
	var sum, min, max, clockError time.Duration
	consume := func(timing frame.Timing) {
		if !timing.ClockSynced {
			return
		}
		delay := time.Since(timing.Source)
		mu.Lock()
		defer mu.Unlock()
		if count == 0 || delay < min {
			min = delay
		}
		if count == 0 || delay > max {
			max = delay
		}
		count++
		sum += delay
		clockError = timing.ClockError
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, env["MIRAGE_HOST"], Config{Username: env["MIRAGE_USER"], Password: env["MIRAGE_PASS"], Apple: true, MediaStream: true, MuteAudio: true,
		OnVideoFrame: func(f video.Frame) { consume(f.Timing) }, OnUpdate: func(fb *frame.Framebuffer, _ []image.Rectangle) { consume(fb.Timing) }})
	if err != nil {
		t.Fatal("live connection failed", err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	select {
	case err := <-done:
		t.Fatal("live stream ended", err)
	case <-time.After(12 * time.Second):
	}
	c.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if count == 0 {
		t.Fatal("no clock-calibrated video pictures received")
	}
	t.Logf("CALIBRATED frame-to-decoder samples=%d avg=%v min=%v max=%v clock_error=%v", count, sum/time.Duration(count), min, max, clockError)
	if min < -clockError {
		t.Fatal("frame clock mapping fell outside the measured clock error")
	}
}
