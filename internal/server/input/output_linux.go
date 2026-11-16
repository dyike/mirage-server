package input

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

type OutputMode struct {
	Name                string
	Width, Height, X, Y int
}

// ConfigureOutputs applies a single atomic wlr-output-management transaction.
// Outputs not named in changes keep their existing enabled state and settings.
func ConfigureOutputs(ctx context.Context, changes []OutputMode) error {
	if len(changes) == 0 || len(changes) > 16 {
		return errors.New("invalid output configuration")
	}
	requested := map[string]OutputMode{}
	for _, m := range changes {
		if m.Name == "" || m.Width < 64 || m.Height < 64 || m.Width > 8192 || m.Height > 8192 {
			return errors.New("invalid output mode")
		}
		if _, exists := requested[m.Name]; exists {
			return errors.New("duplicate output")
		}
		requested[m.Name] = m
	}
	path := os.Getenv("WAYLAND_DISPLAY")
	if path == "" {
		return errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), path)
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	conn := raw.(*net.UnixConn)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	w := &wayland{c: conn, next: 2, start: time.Now()}
	if err = w.send(1, 1, words(2), -1); err != nil {
		return err
	}
	var manager uint32
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		if id != 2 || op != 0 {
			return nil
		}
		if len(b) < 4 {
			return io.ErrUnexpectedEOF
		}
		iface, n, e := readString(b[4:])
		if e != nil {
			return e
		}
		if len(b) < n+8 {
			return io.ErrUnexpectedEOF
		}
		if iface == "zwlr_output_manager_v1" {
			manager, e = w.bind(global{order.Uint32(b), order.Uint32(b[n+4:]), iface}, 1)
			return e
		}
		return nil
	})
	if err != nil {
		return err
	}
	if manager == 0 {
		return errors.New("output management unavailable")
	}
	type head struct {
		name    string
		enabled bool
	}
	heads := map[uint32]*head{}
	var serial uint32
	complete := false
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		if id == manager {
			switch op {
			case 0:
				if len(b) != 4 {
					return io.ErrUnexpectedEOF
				}
				heads[order.Uint32(b)] = &head{}
			case 1:
				if len(b) != 4 {
					return io.ErrUnexpectedEOF
				}
				serial = order.Uint32(b)
				complete = true
			case 2:
				return errors.New("output manager stopped")
			}
			return nil
		}
		if h := heads[id]; h != nil {
			switch op {
			case 0:
				name, _, e := readString(b)
				if e != nil {
					return e
				}
				h.name = name
			case 4:
				if len(b) != 4 {
					return io.ErrUnexpectedEOF
				}
				h.enabled = order.Uint32(b) != 0
			case 9:
				delete(heads, id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !complete {
		return errors.New("incomplete output configuration")
	}
	found := map[string]bool{}
	for _, h := range heads {
		found[h.name] = true
	}
	for name := range requested {
		if !found[name] {
			return fmt.Errorf("output %q not found", name)
		}
	}
	config := w.id()
	if err = w.send(manager, 0, words(config, serial), -1); err != nil {
		return err
	}
	defer w.send(config, 4, nil, -1)
	for id, h := range heads {
		m, change := requested[h.name]
		if !h.enabled && !change {
			if err = w.send(config, 1, words(id), -1); err != nil {
				return err
			}
			continue
		}
		ch := w.id()
		if err = w.send(config, 0, words(ch, id), -1); err != nil {
			return err
		}
		if change {
			if err = w.send(ch, 1, words(uint32(m.Width), uint32(m.Height), 0), -1); err != nil {
				return err
			}
			if err = w.send(ch, 2, words(uint32(m.X), uint32(m.Y)), -1); err != nil {
				return err
			}
		}
	}
	if err = w.send(config, 2, nil, -1); err != nil {
		return err
	}
	for {
		id, op, _, e := w.read()
		if e != nil {
			return e
		}
		if id == config {
			switch op {
			case 0:
				return nil
			case 1:
				return errors.New("compositor rejected output mode")
			case 2:
				return errors.New("output configuration changed concurrently")
			}
		}
	}
}
