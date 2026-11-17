package audioroute

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/server/capture"
)

func TestLiveExclusiveAudio(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_EXCLUSIVE_AUDIO") != "1" {
		t.Skip("temporarily reroutes real playback")
	}
	before, err := pactl("get-default-sink")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := errors.New("captured virtual sink")
	err = m.Capture(ctx, func(ctx context.Context, target string) error {
		current, e := pactl("get-default-sink")
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(current)) != SinkName {
			t.Fatal("default sink not redirected")
		}
		tone := exec.CommandContext(ctx, "pw-cat", "--playback", "--raw", "--rate", "48000", "--channels", "2", "--format", "s16", "--target", target, "-")
		tone.Stdin = bytes.NewReader(bytes.Repeat([]byte{0x80, 0x01, 0x80, 0x01, 0x80, 0xfe, 0x80, 0xfe}, 48000*5))
		tone.Stderr = os.Stderr
		if e = tone.Start(); e != nil {
			return e
		}
		defer func() { tone.Process.Kill(); tone.Wait() }()
		packets := 0
		defer func() { t.Logf("PCM buffers: %d", packets) }()
		return capture.AudioFrom(ctx, 3, 2, 48000, false, target, func(data []byte) error {
			packets++
			for _, b := range data {
				if b != 0 {
					return done
				}
			}
			return nil
		})
	})
	if !errors.Is(err, done) {
		t.Fatal(err)
	}
	// Explicit close models service shutdown, without waiting for the grace period.
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	// Close is idempotent, including the deferred cleanup.
	after, err := pactl("get-default-sink")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("default changed: %q -> %q", before, after)
	}
	var sinks []sink
	if err = m.list("sinks", &sinks); err != nil {
		t.Fatal(err)
	}
	for _, s := range sinks {
		if s.Name == SinkName {
			t.Fatal("exclusive sink leaked")
		}
	}
	t.Log("captured nonzero PCM from silent virtual output; original default restored")
}
