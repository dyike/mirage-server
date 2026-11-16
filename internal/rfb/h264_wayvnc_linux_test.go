//go:build linux && !android

package rfb

import (
	"context"
	"github.com/dyike/keel/gio/gpu/headless"
	"github.com/dyike/mirage-server/internal/video"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in: capture the current desktop in memory only, with remote input disabled.
func TestWayVNCH264Hardware(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_WAYVNC_H264") != "1" {
		t.Skip("opt-in live Wayland GPU interoperability")
	}
	testWayVNCVideoHardware(t, EncH264, "H.264")
}

func TestWayVNCHEVCHardware(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_WAYVNC_HEVC") != "1" {
		t.Skip("opt-in experimental HEVC server")
	}
	testWayVNCVideoHardware(t, EncMirageHEVC, "HEVC")
}

func testWayVNCVideoHardware(t *testing.T, encoding int32, name string) {
	t.Helper()
	window, err := headless.NewWindow(64, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Release()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	listener.Close()
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	os.WriteFile(config, []byte("address=127.0.0.1\nport="+port+"\nenable_auth=false\n"), 0600)
	log, _ := os.Create(filepath.Join(dir, "server.log"))
	defer log.Close()
	cmd := exec.Command("wayvnc", "--config="+config, "--socket="+filepath.Join(dir, "socket"), "--gpu", "--disable-input", "--max-fps=60", "--log-level=info")
	if output := os.Getenv("MIRAGE_TEST_WAYVNC_OUTPUT"); output != "" {
		cmd.Args = append(cmd.Args, "--output="+output)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan video.Frame, 8)
	client, err := Dial(ctx, addr, Config{Encodings: []int32{encoding, EncH264, EncTight, EncRaw, EncPseudoLastRect, EncPseudoContinuousUpdates, EncPseudoDesktopSize, EncPseudoExtendedDesktopSize}, OnVideoFrame: func(f video.Frame) {
		select {
		case frames <- video.Frame{W: f.W, H: f.H, Buffer: f.Buffer, Surface: f.Surface}:
		default:
		}
	}})
	if err != nil {
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("dial: %v server: %s", err, data)
	}
	run := make(chan error, 1)
	go func() { run <- client.Run() }()
	defer func() { client.Close(); <-run }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	count := 0
	for count < 10 {
		select {
		case <-ticker.C:
			if err := client.Refresh(); err != nil {
				t.Fatal(err)
			}
		case f := <-frames:
			count++
			status := client.VideoStatus()
			if client.Encoding() != name || !status.Hardware || !status.ZeroCopy || f.Surface == 0 {
				t.Fatalf("encoding=%s status=%+v frame=%+v", client.Encoding(), status, f)
			}
			if count == 10 {
				t.Logf("10 native pictures: %dx%d %s %+v", f.W, f.H, client.Encoding(), status)
			}
		case <-ctx.Done():
			data, _ := os.ReadFile(log.Name())
			t.Fatalf("no native picture: %s %+v server: %s", client.Encoding(), client.VideoStatus(), data)
		}
	}

}
