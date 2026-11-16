package input

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestOutputConfigurationTransaction(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		name := "rejected"
		if succeeds {
			name = "applied"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wayland-output")
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			t.Setenv("WAYLAND_DISPLAY", path)
			done := make(chan error, 1)
			go func() {
				c, err := l.AcceptUnix()
				if err != nil {
					done <- err
					return
				}
				defer c.Close()
				w := &wayland{c: c}
				var manager, config, changedHead uint32
				mode, position, other := false, false, false
				for {
					header := make([]byte, 8)
					_, err := io.ReadFull(c, header)
					var id uint32
					var op uint16
					var b []byte
					if err == nil {
						id = order.Uint32(header)
						h := order.Uint32(header[4:])
						op = uint16(h)
						b = make([]byte, int(h>>16)-8)
						_, err = io.ReadFull(c, b)
					}
					if err != nil {
						if !mode || !position || !other {
							done <- fmt.Errorf("incomplete transaction: %v %v %v: %w", mode, position, other, err)
						} else {
							done <- nil
						}
						return
					}
					switch {
					case id == 1 && op == 1:
						payload := append(words(1), wlString("zwlr_output_manager_v1")...)
						w.send(2, 0, append(payload, words(1)...), -1)
					case id == 1 && op == 0:
						w.send(order.Uint32(b), 0, words(1), -1)
					case id == 2:
						_, n, e := readString(b[4:])
						if e != nil {
							done <- e
							return
						}
						manager = order.Uint32(b[n+8:])
						for i, name := range []string{"MIRAGE", "OTHER"} {
							head := uint32(0xff000001 + i)
							w.send(manager, 0, words(head), -1)
							w.send(head, 0, wlString(name), -1)
							w.send(head, 4, words(1), -1)
						}
						w.send(manager, 1, words(42), -1)
					case id == manager && op == 0:
						if len(b) != 8 || order.Uint32(b[4:]) != 42 {
							done <- io.ErrUnexpectedEOF
							return
						}
						config = order.Uint32(b)
					case id == config && op == 0:
						if order.Uint32(b[4:]) == 0xff000001 {
							changedHead = order.Uint32(b)
						} else if order.Uint32(b[4:]) == 0xff000002 {
							other = true
						}
					case id == changedHead && op == 1:
						mode = len(b) == 12 && order.Uint32(b) == 800 && order.Uint32(b[4:]) == 600
					case id == changedHead && op == 2:
						position = len(b) == 8 && order.Uint32(b) == 100 && order.Uint32(b[4:]) == 200
					case id == config && op == 2:
						result := uint16(1)
						if succeeds {
							result = 0
						}
						w.send(config, result, nil, -1)
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = ConfigureOutputs(ctx, []OutputMode{{Name: "MIRAGE", Width: 800, Height: 600, X: 100, Y: 200}})
			if (err == nil) != succeeds {
				t.Fatalf("configuration result %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
