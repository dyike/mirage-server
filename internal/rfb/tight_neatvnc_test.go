//go:build linux && !android

package rfb

import (
	"context"
	"fmt"
	"github.com/dyike/mirage-server/internal/frame"
	"image"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNeatVNCAnimatedFrames(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_NEATVNC") != "1" {
		t.Skip("opt-in independent NeatVNC animation test")
	}
	flags, err := exec.Command("pkg-config", "--cflags", "--libs", "neatvnc").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "frames")
	args := append([]string{"-O2", "testdata/neatvnc_frames.c", "-o", binary}, strings.Fields(string(flags))...)
	args = append(args, "-laml")
	if output, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, output)
	}
	for _, encoding := range []int32{EncZRLE, EncTight} {
		t.Run(fmt.Sprint(encoding), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			_, port, _ := net.SplitHostPort(addr)
			listener.Close()
			log, err := os.Create(filepath.Join(dir, fmt.Sprintf("%d.log", encoding)))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd := exec.Command(binary, port)
			cmd.Stdout = log
			cmd.Stderr = log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			ready := false
			for i := 0; i < 100; i++ {
				conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
				if err == nil {
					conn.Close()
					ready = true
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if !ready {
				data, _ := os.ReadFile(log.Name())
				t.Fatalf("start: %s", data)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			frames := 0
			done := make(chan struct{}, 1)
			fault := make(chan string, 1)
			config := Config{Encodings: []int32{encoding, -25, EncCopyRect, EncRaw, EncPseudoLastRect, EncPseudoContinuousUpdates, EncPseudoExtendedDesktopSize}, OnUpdate: func(fb *frame.Framebuffer, dirty []image.Rectangle) {
				if frames == 0 && fb.Pix[(80*fb.W+20)*4+3] == 0 {
					return
				}
				frames++
				// Stable gradient below the animated strip must survive every update.
				for y := 80; y < 220; y += 23 {
					for x := 20; x < 300; x += 31 {
						i := (y*fb.W + x) * 4
						want := []int{30 + x/2, 40 + y/2, 90, 255}
						for k, v := range want {
							delta := int(fb.Pix[i+k]) - v
							if delta < -6 || delta > 6 {
								select {
								case fault <- fmt.Sprintf("frame %d pixel %d,%d channel %d got %d want %d", frames, x, y, k, fb.Pix[i+k], v):
								default:
								}
								return
							}
						}
					}
				}
				if frames >= 30 {
					select {
					case done <- struct{}{}:
					default:
					}
				}
			}}
			client, err := Dial(ctx, addr, config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			run := make(chan error, 1)
			go func() { run <- client.Run() }()
			defer func() { client.Close(); <-run }()
			select {
			case <-done:
				t.Logf("30 animated updates decoded; actual encoding %s", client.Encoding())
			case err := <-fault:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal("timed out waiting for animated updates")
			}
		})
	}
}
