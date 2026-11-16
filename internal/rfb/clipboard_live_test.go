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

	"github.com/dyike/mirage-server/internal/clip"
	"github.com/dyike/mirage-server/internal/frame"
)

// Explicit integration test: exchange synthetic content with a real Mac,
// then restore its original supported clipboard. Credentials never enter logs.
func TestMacClipboardLive(t *testing.T) {
	if os.Getenv("MIRAGE_MAC_CLIPBOARD_TEST") != "1" {
		t.Skip("set MIRAGE_MAC_CLIPBOARD_TEST=1 to test the configured Mac")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(home, ".config", "mirage", "test.env"))
	if err != nil {
		t.Fatal("Mac test credentials unavailable")
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "="); ok && !strings.HasPrefix(k, "#") {
			values[k] = v
		}
	}
	if scanner.Err() != nil || values["MIRAGE_HOST"] == "" {
		t.Fatal("Mac test configuration is incomplete")
	}
	updates := make(chan clip.Content, 32)
	ready := make(chan struct{})
	videoReady := make(chan struct{})
	var readyOnce sync.Once
	var videoOnce sync.Once
	withVideo := os.Getenv("MIRAGE_MAC_CLIPBOARD_MEDIA_TEST") == "1"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Dial(ctx, values["MIRAGE_HOST"], Config{Apple: true, Username: values["MIRAGE_USER"], Password: values["MIRAGE_PASS"], MediaStream: true, MuteAudio: true,
		OnUpdate: func(*frame.Framebuffer, []image.Rectangle) { readyOnce.Do(func() { close(ready) }) },
		OnMediaDiagnostics: func(d MediaDiagnostics) {
			if d.State == "Video active" {
				videoOnce.Do(func() { close(videoReady) })
			}
			if d.Fallback != "" {
				t.Logf("media fallback: %s", d.Fallback)
			}
		},
		OnClipboard: func(content clip.Content) {
			select {
			case updates <- content:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal("Mac connection failed:", err)
	}
	// Exercise the same Apple clipboard wire without requesting a second
	// HEVC stream: a viewer may already be using the server's media slot.
	c.cfg.MediaStream = withVideo
	if err := c.start(); err != nil {
		c.Close()
		t.Fatal(err)
	}
	c.cfg.MediaStream = true
	if err := c.startClipboard(); err != nil {
		c.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := c.readMessage(); err != nil {
				done <- err
				return
			}
		}
	}()
	defer func() {
		c.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Mac session did not stop")
		}
	}()
	wait := func(want *clip.Content) clip.Content {
		t.Helper()
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		poll := time.NewTicker(150 * time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case got := <-updates:
				if want == nil || got.Hash() == want.Hash() {
					return got
				}
			case <-poll.C:
				if want != nil {
					if err := c.fetchClipboard(); err != nil {
						t.Fatal(err)
					}
				}
			case err := <-done:
				t.Fatal("Mac session ended during clipboard exchange:", err)
			case <-timer.C:
				t.Fatal("Mac did not return the expected clipboard")
			}
		}
	}
	original := wait(nil)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("Mac did not produce its initial framebuffer")
	}
	if withVideo {
		select {
		case <-videoReady:
		case <-time.After(5 * time.Second):
			t.Fatal("Mac high-performance video did not become active")
		}
	}
	defer func() {
		if err := c.SetClipboardConfirmed(original); err != nil {
			t.Error("could not restore Mac clipboard:", err)
			return
		}
		wait(&original)
	}()
	for _, content := range []clip.Content{{MIME: clip.Text, Data: []byte("Mirage 双向剪贴板 A → B → A 👋")}, clipboardPicture(t)} {
		if err := c.SetClipboardConfirmed(content); err != nil {
			t.Fatal(err)
		}
		if err := c.fetchClipboard(); err != nil {
			t.Fatal(err)
		}
		wait(&content)
		t.Logf("real Mac accepted and returned %s", content.MIME)
	}
	// Use a second viewer to change the Mac pasteboard. The first must
	// receive both formats via change notifications, without polling.
	peerReady := make(chan struct{})
	var peerOnce sync.Once
	peer, err := Dial(ctx, values["MIRAGE_HOST"], Config{Apple: true, Username: values["MIRAGE_USER"], Password: values["MIRAGE_PASS"], MediaStream: true, MuteAudio: true,
		OnClipboard: func(clip.Content) {},
		OnUpdate:    func(*frame.Framebuffer, []image.Rectangle) { peerOnce.Do(func() { close(peerReady) }) },
	})
	if err != nil {
		t.Fatal("second Mac viewer failed:", err)
	}
	defer peer.Close()
	peer.cfg.MediaStream = false
	if err := peer.start(); err != nil {
		t.Fatal(err)
	}
	peer.cfg.MediaStream = true
	if err := peer.startClipboard(); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			if _, err := peer.readMessage(); err != nil {
				return
			}
		}
	}()
	defer func() { peer.Close(); <-peerDone }()
	select {
	case <-peerReady:
	case <-time.After(5 * time.Second):
		t.Fatal("second viewer did not become ready")
	}
	for _, content := range []clip.Content{{MIME: clip.Text, Data: []byte("Mirage copied at B → A 中文 👋")}, clipboardPicture(t)} {
	drain:
		for {
			select {
			case <-updates:
			default:
				break drain
			}
		}
		if err := peer.SetClipboardConfirmed(content); err != nil {
			t.Fatal(err)
		}
		timer := time.NewTimer(5 * time.Second)
	matched:
		for {
			select {
			case got := <-updates:
				if got.Hash() == content.Hash() {
					break matched
				}
			case <-timer.C:
				t.Fatalf("Mac did not notify the first viewer of %s copied at the other end", content.MIME)
			}
		}
		timer.Stop()
		t.Logf("Mac pushed %s from B to A without polling", content.MIME)
	}
}
