package rfb

import (
	"context"
	"image"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

// Opt-in against a dedicated input-disabled server, normally over an SSH tunnel.
func TestLiveVNCMultiDisplayH264(t *testing.T) {
	addr := os.Getenv("MIRAGE_TEST_VNC_ADDRESS")
	if addr == "" {
		t.Skip("requires an isolated VNC test server")
	}
	minScreens := 2
	if value := os.Getenv("MIRAGE_TEST_VNC_MIN_SCREENS"); value != "" {
		var err error
		minScreens, err = strconv.Atoi(value)
		if err != nil || minScreens < 1 {
			t.Fatal("invalid MIRAGE_TEST_VNC_MIN_SCREENS")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var frames atomic.Int64
	var mu sync.Mutex
	seen := map[image.Rectangle]bool{}
	started := time.Now()
	c, err := Dial(ctx, addr, Config{Encodings: append([]int32{EncH264, EncTight}, DefaultEncodings...), OnUpdate: func(_ *frame.Framebuffer, dirty []image.Rectangle) {
		frames.Add(1)
		mu.Lock()
		for _, r := range dirty {
			seen[r] = true
		}
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	defer func() { c.Close(); <-done }()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("frames=%d encoding=%s status=%+v", frames.Load(), c.Encoding(), c.VideoStatus())
		case <-tick.C:
			if frames.Load() >= 60 && time.Since(started) > 6*time.Second {
				l := c.Desktop()
				status := c.VideoStatus()
				if c.Encoding() != "H.264" || !status.Hardware || l == nil || len(l.Screens) < minScreens {
					t.Fatalf("encoding=%s status=%+v layout=%+v", c.Encoding(), status, l)
				}
				if expected := os.Getenv("MIRAGE_TEST_VNC_SIZE"); expected != "" && expected != strconv.Itoa(l.Size.X)+"x"+strconv.Itoa(l.Size.Y) {
					t.Fatalf("size=%v want=%s", l.Size, expected)
				}
				mu.Lock()
				for _, screen := range l.Screens {
					if !seen[screen.Bounds] {
						t.Errorf("no decoded picture for screen %v", screen)
					}
				}
				mu.Unlock()
				t.Logf("frames=%d encoding=%s status=%+v layout=%+v", frames.Load(), c.Encoding(), status, l)
				return
			}
			if err := c.Refresh(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
