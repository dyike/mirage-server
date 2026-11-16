package server

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/rfb"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/video"
)

// Opt-in local performance probe using the Go ARD client and 4K capture (not macOS Screen Sharing).
// Logs timing/byte counts only, never screen content or authentication material.
func TestARDLocalSoak(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_ARD_SOAK") != "1" {
		t.Skip("opt-in local 4K SRTP soak")
	}
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cfg, _ := testTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	width, height, fps, bitrate := 3840, 2160, 30, 8000
	if os.Getenv("MIRAGE_TEST_ARD_5K") == "1" {
		width, height, fps, bitrate = 4608, 2592, 60, 20000
	}
	if value, err := strconv.Atoi(os.Getenv("MIRAGE_TEST_ARD_KBPS")); err == nil && value > 0 {
		bitrate = value
	}
	serverBitrate := bitrate
	if os.Getenv("MIRAGE_TEST_ARD_5K") == "1" {
		serverBitrate = 40000
	}
	var starts, encoded, bytesSent, maxFrame atomic.Int64
	var timedFrames, timestampedFrames atomic.Int64
	var timingMu sync.Mutex
	var sends, gaps []time.Duration
	var last time.Time
	frames := 0
	srv := RFBServer{TLS: cfg, ARD: true, Username: "probe", Password: "local-test", Width: width, Height: height, PreferHEVC: true, VideoBitrate: serverBitrate, VideoFPS: fps, DesktopSource: func(ctx context.Context, r CaptureRequest, send func([]byte) error) error {
		starts.Add(1)
		output := os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT")
		return capture.Run(ctx, capture.Options{Test: output == "", Output: output, Codec: r.Codec, Width: width, Height: height, FPS: fps, Bitrate: r.Bitrate, BindBitrate: r.BindBitrate, BindKeyframe: r.BindKeyframe, RepeatIdle: r.RepeatIdle, MinQP: r.MinQP}, func(f capture.Frame) error {
			encoded.Add(1)
			bytesSent.Add(int64(len(f.Data)))
			for old := maxFrame.Load(); int64(len(f.Data)) > old; old = maxFrame.Load() {
				if maxFrame.CompareAndSwap(old, int64(len(f.Data))) {
					break
				}
			}
			start := time.Now()
			err := send(f.Data)
			timingMu.Lock()
			sends = append(sends, time.Since(start))
			timingMu.Unlock()
			return err
		})
	}}
	audioEnabled := os.Getenv("MIRAGE_TEST_ARD_AUDIO") == "1"
	if audioEnabled {
		srv.ARDAudio = func(ctx context.Context, send func([]byte) error) error { return capture.AudioELD(ctx, true, send) }
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(ctx, listener) }()
	defer func() { cancel(); <-serverDone }()
	c, err := rfb.Dial(ctx, listener.Addr().String(), rfb.Config{Apple: true, MediaStream: true, Username: "probe", Password: "local-test", NoAudio: !audioEnabled, MuteAudio: true, Network: rfb.NetworkLocal, MediaMaxBitrate: uint64(bitrate) * 1000, Dialer: ardSoakDialer{}, OnVideoFrame: func(f video.Frame) {
		if !f.Timing.Source.IsZero() {
			timestampedFrames.Add(1)
		}
		if f.Timing.ClockSynced && !f.Timing.Source.IsZero() {
			timedFrames.Add(1)
		}
		now := time.Now()
		timingMu.Lock()
		defer timingMu.Unlock()
		if !last.IsZero() {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
		frames++
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run() }()
	<-ctx.Done()
	c.Close()
	<-done
	timingMu.Lock()
	defer timingMu.Unlock()
	percentile := func(v []time.Duration, p int) time.Duration {
		if len(v) == 0 {
			return 0
		}
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		return v[(len(v)-1)*p/100]
	}
	t.Logf("source=%q captures=%d encoded=%d decoded=%d bytes=%d maxFrame=%d send(p50/p95/max)=%s/%s/%s frameGap(p50/p95/max)=%s/%s/%s diagnostics=%+v", os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT"), starts.Load(), encoded.Load(), frames, bytesSent.Load(), maxFrame.Load(), percentile(sends, 50), percentile(sends, 95), percentile(sends, 100), percentile(gaps, 50), percentile(gaps, 95), percentile(gaps, 100), c.MediaDiagnostics())
	if audioEnabled {
		d := c.MediaDiagnostics()
		if d.AudioPackets < 100 || d.AudioBad != 0 || d.AudioPort == 0 {
			t.Fatalf("ARD audio failed: %+v", d)
		}
	}
	t.Logf("calibrated decoded frames=%d timestamped=%d", timedFrames.Load(), timestampedFrames.Load())
	if timedFrames.Load() < 10 {
		t.Fatal("decoded frames lost calibrated source timing")
	}
	if frames < 120 || starts.Load() > 1 || c.MediaDiagnostics().Fallback != "" {
		t.Fatalf("4K stream stalls: decoded=%d capture restarts=%d", frames, starts.Load())
	}
}

type ardSoakDialer struct{ ardLoopbackDialer }

func (d ardSoakDialer) DialUDP(local, remote netip.AddrPort) (net.Conn, error) {
	c, err := d.ardLoopbackDialer.DialUDP(local, remote)
	if err != nil {
		return nil, err
	}
	every, _ := strconv.Atoi(os.Getenv("MIRAGE_TEST_DROP_EVERY"))
	if every > 0 {
		return &ardPeriodicLoss{Conn: c, every: every}, nil
	}
	return c, nil
}

type ardPeriodicLoss struct {
	net.Conn
	every, count int
}

func (c *ardPeriodicLoss) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil {
			return n, err
		}
		if n > 2 && (b[1] < 192 || b[1] > 223) {
			c.count++
			if c.count%c.every == 0 {
				continue
			}
		}
		return n, nil
	}
}
