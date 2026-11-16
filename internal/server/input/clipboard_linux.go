package input

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// DesktopClipboard owns a separate data-control connection; no focused window,
// wl-copy process, or C server is needed. Only regular selection is shared.
type DesktopClipboard struct {
	w               *wayland
	mu              sync.Mutex
	manager, device uint32
	sources         map[uint32]string
	offers          map[uint32]string
	generation      uint64
	last            string
	updates         chan string
	done            chan struct{}
	once            sync.Once
	workers         sync.WaitGroup
	transfers       chan struct{}
}

func OpenClipboard(seatName string) (*DesktopClipboard, error) {
	path := os.Getenv("WAYLAND_DISPLAY")
	if path == "" {
		return nil, errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), path)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	w := &wayland{c: conn, next: 2, start: time.Now()}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
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
		iface, n, e := readString(b[4:])
		if e != nil {
			return e
		}
		if len(b) < n+8 {
			return io.ErrUnexpectedEOF
		}
		w.globals = append(w.globals, global{order.Uint32(b), order.Uint32(b[4+n:]), iface})
		return nil
	})
	if err != nil {
		return nil, err
	}
	cb := &DesktopClipboard{w: w, sources: map[uint32]string{}, offers: map[uint32]string{}, updates: make(chan string, 1), done: make(chan struct{}), transfers: make(chan struct{}, 4)}
	var seats []output
	for _, g := range w.globals {
		switch g.iface {
		case "zwlr_data_control_manager_v1":
			cb.manager, err = w.bind(g, 1)
		case "wl_seat":
			id, e := w.bind(g, min(2, g.version))
			err = e
			seats = append(seats, output{id: id})
		}
		if err != nil {
			return nil, err
		}
	}
	if cb.manager == 0 || len(seats) == 0 {
		return nil, errors.New("compositor lacks data-control or seat")
	}
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	seat := seats[0].id
	if seatName != "" {
		seat = 0
		for _, s := range seats {
			if s.name == seatName {
				seat = s.id
			}
		}
	}
	if seat == 0 {
		return nil, errors.New("clipboard seat not found")
	}
	cb.device = w.id()
	if err = w.send(cb.manager, 1, words(cb.device, seat), -1); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	ok = true
	go cb.run()
	return cb, nil
}
func (c *DesktopClipboard) Updates() <-chan string { return c.updates }
func (c *DesktopClipboard) Set(text string) error {
	if len(text) > 1<<20 || !utf8.ValidString(text) {
		return errors.New("invalid clipboard text")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}
	if len(c.sources) >= 128 {
		return errors.New("too many outstanding clipboard sources")
	}
	id := c.w.id()
	c.sources[id] = text
	c.last = text
	if err := c.w.send(c.manager, 0, words(id), -1); err != nil {
		return err
	}
	for _, mime := range []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING"} {
		if err := c.w.send(id, 0, wlString(mime), -1); err != nil {
			return err
		}
	}
	return c.w.send(c.device, 0, words(id), -1)
}
func (c *DesktopClipboard) Close() error {
	c.once.Do(func() { c.w.c.Close() })
	<-c.done
	c.workers.Wait()
	return nil
}
func (c *DesktopClipboard) run() {
	defer func() { close(c.done); c.workers.Wait(); close(c.updates) }()
	defer c.w.c.Close()
	var buffer []byte
	var fds []int
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	data := make([]byte, 65536)
	oob := make([]byte, 1024)
	for {
		n, on, flags, _, err := c.w.c.ReadMsgUnix(data, oob)
		if err != nil {
			return
		}
		if flags&unix.MSG_CTRUNC != 0 {
			return
		}
		messages, err := unix.ParseSocketControlMessage(oob[:on])
		if err != nil {
			return
		}
		for _, m := range messages {
			rights, e := unix.ParseUnixRights(&m)
			if e != nil {
				return
			}
			for _, fd := range rights {
				unix.CloseOnExec(fd)
			}
			fds = append(fds, rights...)
		}
		buffer = append(buffer, data[:n]...)
		for len(buffer) >= 8 {
			id, header := order.Uint32(buffer), order.Uint32(buffer[4:])
			size := int(header >> 16)
			op := uint16(header)
			if size < 8 || size%4 != 0 {
				return
			}
			if len(buffer) < size {
				break
			}
			b := buffer[8:size]
			c.mu.Lock()
			text, isSource := c.sources[id]
			c.mu.Unlock()
			if id == 1 && op == 0 {
				return
			}
			if isSource && op == 0 {
				if len(fds) == 0 {
					return
				}
				fd := fds[0]
				fds = fds[1:]
				mime, _, e := readString(b)
				if e != nil {
					unix.Close(fd)
					return
				}
				if mime != "text/plain;charset=utf-8" && mime != "text/plain" && mime != "UTF8_STRING" {
					unix.Close(fd)
				} else {
					c.sendText(fd, text)
				}
			} else if isSource && op == 1 {
				c.mu.Lock()
				delete(c.sources, id)
				c.mu.Unlock()
				c.w.send(id, 1, nil, -1)
			} else if id == c.device {
				switch op {
				case 0:
					if len(b) != 4 {
						return
					}
					c.offers[order.Uint32(b)] = ""
				case 1:
					if len(b) != 4 {
						return
					}
					offer := order.Uint32(b)
					c.receive(offer)
				case 2:
					return
				}
			} else if _, ok := c.offers[id]; ok && op == 0 {
				mime, _, e := readString(b)
				if e != nil {
					return
				}
				if mime == "text/plain;charset=utf-8" || mime == "UTF8_STRING" || (mime == "text/plain" && c.offers[id] == "") {
					c.offers[id] = mime
				}
			}
			buffer = buffer[size:]
		}
		if len(fds) > 64 {
			return
		}
	}
}
func (c *DesktopClipboard) sendText(fd int, text string) {
	select {
	case c.transfers <- struct{}{}:
	default:
		unix.Close(fd)
		return
	}
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		defer func() { <-c.transfers }()
		unix.SetNonblock(fd, true)
		f := os.NewFile(uintptr(fd), "clipboard-send")
		defer f.Close()
		f.SetWriteDeadline(time.Now().Add(2 * time.Second))
		io.WriteString(f, text)
	}()
}
func (c *DesktopClipboard) receive(offer uint32) {
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.mu.Unlock()
	mime := c.offers[offer]
	for id := range c.offers {
		if id != offer {
			c.w.send(id, 1, nil, -1)
			delete(c.offers, id)
		}
	}
	if offer == 0 || mime == "" {
		return
	}
	select {
	case c.transfers <- struct{}{}:
	default:
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		<-c.transfers
		return
	}
	err = c.w.send(offer, 0, wlString(mime), int(w.Fd()))
	w.Close()
	if err != nil {
		r.Close()
		<-c.transfers
		return
	}
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		defer func() { <-c.transfers }()
		defer r.Close()
		r.SetReadDeadline(time.Now().Add(2 * time.Second))
		data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
		if err != nil || len(data) > 1<<20 || !utf8.Valid(data) {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if generation != c.generation || string(data) == c.last {
			return
		}
		c.last = string(data)
		// run owns closing updates. Route results through a separate publish guard.
		select {
		case <-c.done:
			return
		default:
		}
		c.publish(string(data))
	}()
}
func (c *DesktopClipboard) publish(text string) {
	select {
	case c.updates <- text:
	default:
		select {
		case <-c.updates:
		default:
		}
		select {
		case c.updates <- text:
		default:
		}
	}
}
