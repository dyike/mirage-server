package input

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var order = binary.NativeEndian

func words(v ...uint32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		order.PutUint32(b[i*4:], x)
	}
	return b
}
func wlString(s string) []byte {
	b := make([]byte, 4+((len(s)+1+3)&^3))
	order.PutUint32(b, uint32(len(s)+1))
	copy(b[4:], s)
	return b
}
func readString(b []byte) (string, int, error) {
	if len(b) < 4 {
		return "", 0, io.ErrUnexpectedEOF
	}
	n := int(order.Uint32(b))
	used := 4 + ((n + 3) &^ 3)
	if n < 1 || used > len(b) || b[4+n-1] != 0 {
		return "", 0, errors.New("invalid Wayland string")
	}
	return string(b[4 : 4+n-1]), used, nil
}

type global struct {
	name, version uint32
	iface         string
}
type output struct {
	xdg                               uint32
	x, y, logicalWidth, logicalHeight int
	width, height                     int
	id                                uint32
	name                              string
}
type wayland struct {
	c                 *net.UnixConn
	next              uint32
	mu                sync.Mutex
	globals           []global
	outputs           []output
	keyboard, pointer uint32
	err               error
	start             time.Time
}

func (w *wayland) id() uint32 { w.next++; return w.next }
func (w *wayland) send(id uint32, op uint16, data []byte, fd int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if len(data)+8 > 65535 {
		return errors.New("Wayland message too large")
	}
	b := append(words(id, uint32(len(data)+8)<<16|uint32(op)), data...)
	w.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	var err error
	if fd >= 0 {
		var n int
		n, _, err = w.c.WriteMsgUnix(b, unix.UnixRights(fd), nil)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
	} else {
		for len(b) > 0 {
			n, e := w.c.Write(b)
			if e != nil {
				err = e
				break
			}
			b = b[n:]
		}
	}
	if err != nil {
		w.err = err
	}
	return err
}
func (w *wayland) read() (uint32, uint16, []byte, error) {
	var h [8]byte
	if _, err := io.ReadFull(w.c, h[:]); err != nil {
		return 0, 0, nil, err
	}
	id, nop := order.Uint32(h[:]), order.Uint32(h[4:])
	n := int(nop >> 16)
	if n < 8 || n%4 != 0 {
		return 0, 0, nil, errors.New("invalid Wayland event")
	}
	b := make([]byte, n-8)
	_, err := io.ReadFull(w.c, b)
	if id == 1 && uint16(nop) == 0 {
		return 0, 0, nil, fmt.Errorf("Wayland protocol error: %x", b)
	}
	return id, uint16(nop), b, err
}
func (w *wayland) roundtrip(handle func(uint32, uint16, []byte) error) error {
	callback := w.id()
	if err := w.send(1, 0, words(callback), -1); err != nil {
		return err
	}
	for {
		id, op, b, err := w.read()
		if err != nil {
			return err
		}
		if id == callback && op == 0 {
			return nil
		}
		if err := handle(id, op, b); err != nil {
			return err
		}
	}
}
func (w *wayland) bind(g global, version uint32) (uint32, error) {
	id := w.id()
	b := append(words(g.name), wlString(g.iface)...)
	b = append(b, words(version, id)...)
	return id, w.send(2, 0, b, -1)
}
func openWayland(outputName, seatName string) (*wayland, error) {
	return discoverWayland(outputName, seatName, false)
}

func discoverWayland(outputName, seatName string, readOnly bool) (*wayland, error) {
	path := os.Getenv("WAYLAND_DISPLAY")
	if path == "" {
		return nil, errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), path)
	}
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	w := &wayland{c: c, next: 2, start: time.Now()}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err = w.send(1, 1, words(2), -1); err != nil {
		return nil, err
	}
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		if id != 2 || op != 0 {
			return nil
		}
		if len(b) < 4 {
			return io.ErrUnexpectedEOF
		}
		name := order.Uint32(b)
		iface, n, err := readString(b[4:])
		if err != nil {
			return err
		}
		if len(b) < 4+n+4 {
			return io.ErrUnexpectedEOF
		}
		w.globals = append(w.globals, global{name, order.Uint32(b[4+n:]), iface})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var seat, pm, km, xdgManager uint32
	var seats []output
	for _, g := range w.globals {
		switch g.iface {
		case "wl_seat":
			id, e := w.bind(g, min(g.version, 2))
			err = e
			seats = append(seats, output{id: id})
			if seat == 0 {
				seat = id
			}
		case "zwlr_virtual_pointer_manager_v1":
			if g.version >= 2 {
				pm, err = w.bind(g, 2)
			}
		case "zxdg_output_manager_v1":
			xdgManager, err = w.bind(g, min(g.version, 3))
		case "zwp_virtual_keyboard_manager_v1":
			km, err = w.bind(g, 1)
		case "wl_output":
			if g.version >= 4 {
				id, e := w.bind(g, 4)
				err = e
				w.outputs = append(w.outputs, output{id: id})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if xdgManager != 0 {
		for i := range w.outputs {
			w.outputs[i].xdg = w.id()
			if err = w.send(xdgManager, 1, words(w.outputs[i].xdg, w.outputs[i].id), -1); err != nil {
				return nil, err
			}
		}
	}
	if !readOnly && (seat == 0 || pm == 0 || km == 0) {
		return nil, errors.New("compositor lacks virtual input protocols")
	}
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		for i := range w.outputs {
			if id == w.outputs[i].xdg && len(b) == 8 {
				switch op {
				case 0:
					w.outputs[i].x, w.outputs[i].y = int(int32(order.Uint32(b))), int(int32(order.Uint32(b[4:])))
				case 1:
					w.outputs[i].logicalWidth, w.outputs[i].logicalHeight = int(order.Uint32(b)), int(order.Uint32(b[4:]))
				}
			}
		}
		if op == 1 && len(b) == 16 && order.Uint32(b)&1 != 0 {
			for i := range w.outputs {
				if w.outputs[i].id == id {
					w.outputs[i].width = int(order.Uint32(b[4:]))
					w.outputs[i].height = int(order.Uint32(b[8:]))
				}
			}
		}
		if op == 1 {
			for i := range seats {
				if seats[i].id == id {
					name, _, e := readString(b)
					if e != nil {
						return e
					}
					seats[i].name = name
				}
			}
		}
		if op == 4 {
			for i := range w.outputs {
				if w.outputs[i].id == id {
					name, _, err := readString(b)
					if err != nil {
						return err
					}
					w.outputs[i].name = name
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if readOnly {
		ok = true
		return w, nil
	}
	if seatName != "" {
		seat = 0
		for _, candidate := range seats {
			if candidate.name == seatName {
				seat = candidate.id
			}
		}
		if seat == 0 {
			return nil, fmt.Errorf("input seat %q not found", seatName)
		}
	}
	var target uint32
	for _, o := range w.outputs {
		if o.name == outputName {
			target = o.id
		}
	}
	if outputName == "" || (target == 0 && outputName != "*") {
		return nil, fmt.Errorf("input output %q not found; specify captured output explicitly", outputName)
	}
	w.keyboard = w.id()
	if err = w.send(km, 0, words(seat, w.keyboard), -1); err != nil {
		return nil, err
	}
	w.pointer = w.id()
	if err = w.send(pm, 2, words(seat, target, w.pointer), -1); err != nil {
		return nil, err
	}
	if err = w.roundtrip(func(uint32, uint16, []byte) error { return nil }); err != nil {
		return nil, err
	}
	c.SetReadDeadline(time.Time{})
	ok = true
	go func() {
		for {
			_, _, _, e := w.read()
			if e != nil {
				w.mu.Lock()
				w.err = e
				w.mu.Unlock()
				c.Close()
				return
			}
		}
	}()
	return w, nil
}
func (w *wayland) stamp() uint32 { return uint32(time.Since(w.start) / time.Millisecond) }
func (w *wayland) motion(x, y float64) error {
	const extent = uint32(math.MaxInt32)
	return w.send(w.pointer, 1, words(w.stamp(), uint32(max(0, min(1, x))*float64(extent)), uint32(max(0, min(1, y))*float64(extent)), extent, extent), -1)
}
func (w *wayland) button(code uint32, down bool) error {
	return w.send(w.pointer, 2, words(w.stamp(), code, boolWord(down)), -1)
}
func (w *wayland) axis(axis uint32, value float64, steps int32) error {
	source := uint32(0) // wheel
	if steps == 0 {
		source = 3
	} // continuous
	if err := w.send(w.pointer, 5, words(source), -1); err != nil {
		return err
	}
	if steps != 0 {
		return w.send(w.pointer, 7, words(w.stamp(), axis, uint32(int32(value*256)), uint32(steps)), -1)
	}
	return w.send(w.pointer, 3, words(w.stamp(), axis, uint32(int32(value*256))), -1)
}
func (w *wayland) axisStop(axis uint32) error {
	if err := w.send(w.pointer, 5, words(3), -1); err != nil {
		return err
	}
	return w.send(w.pointer, 6, words(w.stamp(), axis), -1)
}
func (w *wayland) frame() error { return w.send(w.pointer, 4, nil, -1) }
func (w *wayland) key(code uint32, down bool) error {
	return w.send(w.keyboard, 1, words(w.stamp(), code, boolWord(down)), -1)
}
func (w *wayland) mods(d, l, k, g uint32) error { return w.send(w.keyboard, 2, words(d, l, k, g), -1) }
func (w *wayland) keymap(data []byte) error {
	fd, err := unix.MemfdCreate("mirage-keymap", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "mirage-keymap")
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return w.send(w.keyboard, 0, words(1, uint32(len(data))), fd)
}
func (w *wayland) close() error {
	w.send(w.keyboard, 3, nil, -1)
	w.send(w.pointer, 8, nil, -1)
	return w.c.Close()
}
func boolWord(v bool) uint32 {
	if v {
		return 1
	}
	return 0
}
