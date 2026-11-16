package input

import (
	"errors"
	"image"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type CursorCapture struct {
	w       *wayland
	updates chan CursorImage
	done    chan struct{}
	once    sync.Once
}

func (c *CursorCapture) Updates() <-chan CursorImage { return c.updates }
func (c *CursorCapture) Close() error                { c.once.Do(func() { c.w.c.Close() }); <-c.done; return nil }
func (c *CursorCapture) publish(v CursorImage) {
	select {
	case c.updates <- v:
	default:
		select {
		case <-c.updates:
		default:
		}
		select {
		case c.updates <- v:
		default:
		}
	}
}
func OpenCursor(outputName, seatName string) (*CursorCapture, error) {
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
		name, n, e := readString(b[4:])
		if e != nil {
			return e
		}
		if len(b) < n+8 {
			return io.ErrUnexpectedEOF
		}
		w.globals = append(w.globals, global{order.Uint32(b), order.Uint32(b[n+4:]), name})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var manager, sourceManager, shm uint32
	var seats, outputs []output
	for _, g := range w.globals {
		switch g.iface {
		case "ext_image_copy_capture_manager_v1":
			manager, err = w.bind(g, 1)
		case "ext_output_image_capture_source_manager_v1":
			sourceManager, err = w.bind(g, 1)
		case "wl_shm":
			shm, err = w.bind(g, 1)
		case "wl_seat":
			id, e := w.bind(g, min(g.version, 2))
			err = e
			seats = append(seats, output{id: id})
		case "wl_output":
			if g.version >= 4 {
				id, e := w.bind(g, 4)
				err = e
				outputs = append(outputs, output{id: id})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if manager == 0 || sourceManager == 0 || shm == 0 || len(seats) == 0 {
		return nil, errors.New("cursor capture protocols unavailable")
	}
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		for i := range outputs {
			if id == outputs[i].id && op == 1 && len(b) == 16 && order.Uint32(b)&1 != 0 {
				outputs[i].width = int(order.Uint32(b[4:]))
				outputs[i].height = int(order.Uint32(b[8:]))
			}
			if id == outputs[i].id && op == 4 {
				name, _, e := readString(b)
				if e != nil {
					return e
				}
				outputs[i].name = name
			}
		}
		for i := range seats {
			if id == seats[i].id && op == 1 {
				name, _, e := readString(b)
				if e != nil {
					return e
				}
				seats[i].name = name
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var target uint32
	var sourceWidth, sourceHeight int
	for _, o := range outputs {
		if o.name == outputName {
			target = o.id
			sourceWidth, sourceHeight = o.width, o.height
		}
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
	if target == 0 || seat == 0 {
		return nil, errors.New("cursor output or seat not found")
	}
	pointer, source, cursor, session := w.id(), w.id(), w.id(), w.id()
	for _, req := range []struct {
		id   uint32
		op   uint16
		args []byte
	}{{seat, 0, words(pointer)}, {sourceManager, 0, words(source, target)}, {manager, 1, words(cursor, source, pointer)}, {cursor, 1, words(session)}} {
		if err = w.send(req.id, req.op, req.args, -1); err != nil {
			return nil, err
		}
	}
	c := &CursorCapture{w: w, updates: make(chan CursorImage, 1), done: make(chan struct{})}
	ok = true
	conn.SetReadDeadline(time.Time{})
	go c.run(shm, cursor, session, sourceWidth, sourceHeight)
	return c, nil
}
func (c *CursorCapture) run(shm, cursor, session uint32, sourceWidth, sourceHeight int) {
	defer close(c.done)
	defer close(c.updates)
	defer c.w.c.Close()
	w := c.w
	var width, height uint32
	format := uint32(0xffffffff)
	hotspot := image.Point{}
	visible := false
	constraints := false
	var cached CursorImage
	var frame, buffer uint32
	var pixels []byte
	var frameWidth, frameHeight, frameFormat, transform uint32
	cleanup := func() {
		if frame != 0 {
			w.send(frame, 0, nil, -1)
			frame = 0
		}
		if buffer != 0 {
			w.send(buffer, 0, nil, -1)
			buffer = 0
		}
		if pixels != nil {
			unix.Munmap(pixels)
			pixels = nil
		}
	}
	defer cleanup()
	capture := func() error {
		if frame != 0 || !visible || width == 0 || height == 0 || format == 0xffffffff {
			return nil
		}
		if width > 512 || height > 512 {
			return errors.New("oversized cursor")
		}
		fd, e := unix.MemfdCreate("mirage-cursor", unix.MFD_CLOEXEC)
		if e != nil {
			return e
		}
		defer unix.Close(fd)
		size := int(width * height * 4)
		if e = unix.Ftruncate(fd, int64(size)); e != nil {
			return e
		}
		pixels, e = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if e != nil {
			return e
		}
		pool := w.id()
		buffer = w.id()
		frame = w.id()
		frameWidth, frameHeight, frameFormat = width, height, format
		transform = 0
		if e = w.send(shm, 0, words(pool, uint32(size)), fd); e != nil {
			return e
		}
		for _, req := range []struct {
			id   uint32
			op   uint16
			args []byte
		}{{pool, 0, words(buffer, 0, width, height, width*4, format)}, {pool, 1, nil}, {session, 0, words(frame)}, {frame, 1, words(buffer)}, {frame, 2, words(0, 0, width, height)}, {frame, 3, nil}} {
			if e = w.send(req.id, req.op, req.args, -1); e != nil {
				return e
			}
		}
		return nil
	}
	for {
		id, op, b, err := w.read()
		if err != nil {
			return
		}
		switch id {
		case cursor:
			switch op {
			case 0:
				visible = true
				if cached.Pixels != nil {
					c.publish(cached)
				}
				if capture() != nil {
					return
				}
			case 1:
				visible = false
				c.publish(CursorImage{})
			case 3:
				if len(b) != 8 {
					return
				}
				hotspot = image.Pt(int(int32(order.Uint32(b))), int(int32(order.Uint32(b[4:]))))
			}
		case session:
			if op <= 3 && !constraints {
				format = 0xffffffff
				constraints = true
			}
			switch op {
			case 0:
				if len(b) != 8 {
					return
				}
				width, height = order.Uint32(b), order.Uint32(b[4:])
			case 1:
				if len(b) != 4 {
					return
				}
				f := order.Uint32(b)
				if f == 0 || f == 1 {
					if format != 0 {
						format = f
					}
				}
			case 4:
				constraints = false
				if format == 0xffffffff {
					return
				}
				if capture() != nil {
					return
				}
			case 5:
				return
			}
		default:
			if id != frame || frame == 0 {
				continue
			}
			switch op {
			case 0:
				if len(b) != 4 {
					return
				}
				transform = order.Uint32(b)
			case 3:
				rgba := make([]byte, len(pixels))
				for i := 0; i < len(pixels); i += 4 {
					a := pixels[i+3]
					if frameFormat == 1 {
						a = 255
					}
					rgba[i], rgba[i+1], rgba[i+2], rgba[i+3] = pixels[i+2], pixels[i+1], pixels[i], a
				}
				cached = transformCursor(CursorImage{Pixels: rgba, Width: int(frameWidth), Height: int(frameHeight), Hotspot: hotspot, SourceWidth: sourceWidth, SourceHeight: sourceHeight}, transform)
				if visible {
					c.publish(cached)
				}
				cleanup()
				if capture() != nil {
					return
				}
			case 4:
				cleanup()
				if len(b) != 4 || order.Uint32(b) != 1 {
					return
				}
				if capture() != nil {
					return
				}
			}
		}
	}
}
func transformCursor(v CursorImage, transform uint32) CursorImage {
	if transform == 0 {
		return v
	}
	if transform > 7 {
		return CursorImage{}
	}
	w, h := v.Width, v.Height
	if transform%2 == 1 {
		v.Width, v.Height = h, w
	}
	pixels := make([]byte, len(v.Pixels))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			nx, ny := TransformPoint((float64(x)+.5)/float64(w), (float64(y)+.5)/float64(h), transform)
			dx, dy := min(v.Width-1, int(nx*float64(v.Width))), min(v.Height-1, int(ny*float64(v.Height)))
			copy(pixels[(dy*v.Width+dx)*4:], v.Pixels[(y*w+x)*4:(y*w+x+1)*4])
		}
	}
	hx, hy := TransformPoint(float64(v.Hotspot.X)/float64(w), float64(v.Hotspot.Y)/float64(h), transform)
	v.Hotspot = image.Pt(min(v.Width-1, int(hx*float64(v.Width))), min(v.Height-1, int(hy*float64(v.Height))))
	v.Pixels = pixels
	return v
}

// CursorGroup keeps independent captures for all shared outputs. Leave events
// from an old output cannot hide a cursor that has already entered another one.
type CursorGroup struct {
	captures []*CursorCapture
	updates  chan CursorImage
	stop     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}

func OpenCursors(outputs []string, seat string) (*CursorGroup, error) {
	g := &CursorGroup{updates: make(chan CursorImage, 1), stop: make(chan struct{})}
	events := make(chan CursorImage, 16)
	for i, name := range outputs {
		c, err := OpenCursor(name, seat)
		if err != nil {
			g.Close()
			return nil, err
		}
		g.captures = append(g.captures, c)
		id := uint32(i + 1)
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			for {
				select {
				case <-g.stop:
					return
				case v, ok := <-c.Updates():
					if !ok {
						return
					}
					v.ScreenID = id
					select {
					case events <- v:
					case <-g.stop:
						return
					}
				}
			}
		}()
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		var active uint32
		for {
			select {
			case <-g.stop:
				return
			case v := <-events:
				if v.Width == 0 && active != 0 && active != v.ScreenID {
					continue
				}
				if v.Width > 0 {
					active = v.ScreenID
				}
				select {
				case g.updates <- v:
				default:
					select {
					case <-g.updates:
					default:
					}
					select {
					case g.updates <- v:
					default:
					}
				}
			}
		}
	}()
	return g, nil
}
func (g *CursorGroup) Updates() <-chan CursorImage { return g.updates }
func (g *CursorGroup) Close() error {
	g.once.Do(func() {
		close(g.stop)
		for _, c := range g.captures {
			c.Close()
		}
		g.wg.Wait()
		close(g.updates)
	})
	return nil
}
