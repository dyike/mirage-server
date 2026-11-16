package input

import (
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveClipboardLifecycle(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_CLIPBOARD") != "1" {
		t.Skip("opt-in data-control lifecycle, no clipboard writes")
	}
	c, err := OpenClipboard("")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-c.done:
		t.Fatal("clipboard protocol disconnected")
	default:
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClipboardWaylandFDTransfers(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "wayland-test")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("WAYLAND_DISPLAY", socket)
	local := make(chan string, 1)
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.AcceptUnix()
		if err != nil {
			failed <- err
			return
		}
		defer conn.Close()
		w := &wayland{c: conn}
		var manager, device uint32
		for {
			header := make([]byte, 8)
			oob := make([]byte, 128)
			n, on, _, _, err := conn.ReadMsgUnix(header, oob)
			if err != nil {
				return
			}
			if n != 8 {
				failed <- fmt.Errorf("short header %d", n)
				return
			}
			id, h := order.Uint32(header), order.Uint32(header[4:])
			size := int(h >> 16)
			op := uint16(h)
			b := make([]byte, size-8)
			if _, err = io.ReadFull(conn, b); err != nil {
				failed <- err
				return
			}
			var fds []int
			messages, _ := unix.ParseSocketControlMessage(oob[:on])
			for _, m := range messages {
				rights, _ := unix.ParseUnixRights(&m)
				fds = append(fds, rights...)
			}
			switch {
			case id == 1 && op == 1:
				for i, name := range []string{"wl_seat", "zwlr_data_control_manager_v1"} {
					payload := append(words(uint32(i+1)), wlString(name)...)
					payload = append(payload, words(2)...)
					if err = w.send(2, 0, payload, -1); err != nil {
						failed <- err
						return
					}
				}
			case id == 1 && op == 0:
				if err = w.send(order.Uint32(b), 0, words(1), -1); err != nil {
					failed <- err
					return
				}
			case id == 2:
				name, n, err := readString(b[4:])
				if err != nil {
					failed <- err
					return
				}
				object := order.Uint32(b[4+n+4:])
				if name == "wl_seat" {
					w.send(object, 1, wlString("seat0"), -1)
				} else {
					manager = object
				}
			case id == manager && op == 1:
				device = order.Uint32(b)
				offer := uint32(0xff000001)
				w.send(device, 0, words(offer), -1)
				w.send(offer, 0, wlString("text/plain;charset=utf-8"), -1)
				w.send(device, 1, words(offer), -1)
			case id == 0xff000001 && op == 0:
				if len(fds) != 1 {
					failed <- fmt.Errorf("missing receive FD")
					return
				}
				f := os.NewFile(uintptr(fds[0]), "clipboard-destination")
				io.WriteString(f, "桌面文字")
				f.Close()
			case id == device && op == 0:
				source := order.Uint32(b)
				r, writer, err := os.Pipe()
				if err != nil {
					failed <- err
					return
				}
				if err = w.send(source, 0, wlString("text/plain;charset=utf-8"), int(writer.Fd())); err != nil {
					failed <- err
					return
				}
				writer.Close()
				r.SetReadDeadline(time.Now().Add(time.Second))
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					failed <- err
					return
				}
				local <- string(data)
			}
		}
	}()
	c, err := OpenClipboard("seat0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close(); <-done }()
	select {
	case text := <-c.Updates():
		if text != "桌面文字" {
			t.Fatal("desktop text corrupted")
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("clipboard receive timeout")
	}
	if err = c.Set("客户端文字"); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-local:
		if text != "客户端文字" {
			t.Fatal("source text corrupted")
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("clipboard send timeout")
	}
}
