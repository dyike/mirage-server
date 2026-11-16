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

	"golang.org/x/sys/unix"
)

// ScreenCapture selects a named wl_output without a desktop Portal. It never
// creates input devices. Frames are owned by the capture and valid until Next.
type ScreenCapture struct {
	w                    *wayland
	shm, session, buffer uint32
	Width, Height        int
	format               uint32
	pixels               []byte
	constraints          bool
	dmabuf               uint32
	device               uint64
	dmaFormats           map[uint32][]uint64
	dmaMode              bool
}

func OpenScreen(ctx context.Context, name string, cursor bool) (*ScreenCapture, error) {
	path := os.Getenv("WAYLAND_DISPLAY")
	if path == "" {
		return nil, errors.New("WAYLAND_DISPLAY is not set")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), path)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	w := &wayland{c: conn.(*net.UnixConn), next: 2, start: time.Now()}
	s := &ScreenCapture{w: w, format: ^uint32(0)}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
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
		w.globals = append(w.globals, global{order.Uint32(b), order.Uint32(b[n+4:]), iface})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var manager, sources uint32
	var outputs []output
	for _, g := range w.globals {
		switch g.iface {
		case "ext_image_copy_capture_manager_v1":
			manager, err = w.bind(g, 1)
		case "ext_output_image_capture_source_manager_v1":
			sources, err = w.bind(g, 1)
		case "zwp_linux_dmabuf_v1":
			if g.version >= 3 {
				s.dmabuf, err = w.bind(g, 3)
			}
		case "wl_shm":
			s.shm, err = w.bind(g, 1)
		case "wl_output":
			if g.version >= 4 {
				var id uint32
				id, err = w.bind(g, 4)
				outputs = append(outputs, output{id: id})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if manager == 0 || sources == 0 {
		return nil, errors.New("native output capture requires ext-image-copy-capture")
	}
	err = w.roundtrip(func(id uint32, op uint16, b []byte) error {
		for i := range outputs {
			if id == outputs[i].id && op == 4 {
				n, _, e := readString(b)
				if e != nil {
					return e
				}
				outputs[i].name = n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var target uint32
	for _, o := range outputs {
		if o.name == name {
			target = o.id
		}
	}
	if target == 0 {
		return nil, fmt.Errorf("capture output %q not found", name)
	}
	source := w.id()
	s.session = w.id()
	if err = w.send(sources, 0, words(source, target), -1); err != nil {
		return nil, err
	}
	if err = w.send(manager, 0, words(s.session, source, boolWord(cursor)), -1); err != nil {
		return nil, err
	}
	for {
		id, op, b, e := w.read()
		if e != nil {
			return nil, e
		}
		if id != s.session {
			continue
		}
		if e = s.constraint(op, b); e != nil {
			return nil, e
		}
		if op == 4 {
			break
		}
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return s, nil
}

func (s *ScreenCapture) constraint(op uint16, b []byte) error {
	if op <= 3 && !s.constraints {
		s.format = ^uint32(0)
		s.constraints = true
		s.dmaFormats = make(map[uint32][]uint64)
	}
	switch op {
	case 0:
		if len(b) != 8 {
			return io.ErrUnexpectedEOF
		}
		width, height := int(order.Uint32(b)), int(order.Uint32(b[4:]))
		if width < 1 || height < 1 || width > 8192 || height > 8192 {
			return errors.New("invalid capture dimensions")
		}
		if (s.buffer != 0 || s.dmaMode) && (width != s.Width || height != s.Height) {
			return errors.New("capture output size changed; reconnect to rebuild capture")
		}
		s.Width, s.Height = width, height
	case 1:
		if len(b) != 4 {
			return io.ErrUnexpectedEOF
		}
		f := order.Uint32(b)
		if f == 0 || f == 1 {
			s.format = f
		}
	case 2:
		if len(b) != 12 || order.Uint32(b) != 8 {
			return errors.New("invalid DRM device")
		}
		s.device = order.Uint64(b[4:])
	case 3:
		if len(b) < 8 || int(order.Uint32(b[4:])) != len(b)-8 || (len(b)-8)%8 != 0 {
			return errors.New("invalid DMA modifiers")
		}
		f := order.Uint32(b)
		for i := 8; i < len(b); i += 8 {
			s.dmaFormats[f] = append(s.dmaFormats[f], order.Uint64(b[i:]))
		}
	case 4:
		s.constraints = false
		if s.Width == 0 || s.Height == 0 || (s.format == ^uint32(0) && len(s.dmaFormats) == 0) {
			return errors.New("output has no supported 32-bit shared memory format")
		}
	case 5:
		return errors.New("capture output removed or session stopped")
	}
	return nil
}

func (s *ScreenCapture) allocate() error {
	if s.shm == 0 || s.format == ^uint32(0) {
		return errors.New("shared-memory capture format unavailable")
	}
	if s.buffer != 0 {
		return nil
	}
	fd, err := unix.MemfdCreate("mirage-screen", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	size := s.Width * s.Height * 4
	if err = unix.Ftruncate(fd, int64(size)); err != nil {
		return err
	}
	s.pixels, err = unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return err
	}
	pool := s.w.id()
	s.buffer = s.w.id()
	if err = s.w.send(s.shm, 0, words(pool, uint32(size)), fd); err != nil {
		return err
	}
	if err = s.w.send(pool, 0, words(s.buffer, 0, uint32(s.Width), uint32(s.Height), uint32(s.Width*4), s.format), -1); err != nil {
		return err
	}
	return s.w.send(pool, 1, nil, -1)
}

// Next returns BGRx pixels in display orientation. Only one caller may use Next.
// The returned slice is borrowed and must be consumed before the next call.
func (s *ScreenCapture) Next(ctx context.Context) ([]byte, int, int, error) {
	stop := context.AfterFunc(ctx, func() { s.w.c.Close() })
	defer stop()
	if err := s.allocate(); err != nil {
		return nil, 0, 0, err
	}
	transform, err := s.captureBuffer(ctx, s.buffer)
	if err != nil {
		return nil, 0, 0, err
	}
	if transform == 0 {
		return s.pixels, s.Width, s.Height, nil
	}
	v := transformCursor(CursorImage{Pixels: s.pixels, Width: s.Width, Height: s.Height}, transform)
	return v.Pixels, v.Width, v.Height, nil
}
func (s *ScreenCapture) captureBuffer(ctx context.Context, buffer uint32) (uint32, error) {
	stop := context.AfterFunc(ctx, func() { s.w.c.Close() })
	defer stop()
	frame := s.w.id()
	defer s.w.send(frame, 0, nil, -1)
	for _, r := range []struct {
		id uint32
		op uint16
		b  []byte
	}{
		{s.session, 0, words(frame)}, {frame, 1, words(buffer)},
		{frame, 2, words(0, 0, uint32(s.Width), uint32(s.Height))}, {frame, 3, nil},
	} {
		if err := s.w.send(r.id, r.op, r.b, -1); err != nil {
			return 0, err
		}
	}
	var transform uint32
	for {
		id, op, b, err := s.w.read()
		if err != nil {
			return 0, err
		}
		if id == s.session {
			if err = s.constraint(op, b); err != nil {
				return 0, err
			}
			continue
		}
		if id != frame {
			continue
		}
		switch op {
		case 0:
			if len(b) != 4 {
				return 0, io.ErrUnexpectedEOF
			}
			transform = order.Uint32(b)
			if transform > 7 {
				return 0, errors.New("invalid output transform")
			}
		case 3:
			return transform, nil
		case 4:
			return 0, fmt.Errorf("native screen capture failed: %x", b)
		}
	}
}
func (s *ScreenCapture) Close() error {
	// Closing the connection releases all compositor objects before unmapping.
	err := s.w.c.Close()
	if s.pixels != nil {
		unix.Munmap(s.pixels)
		s.pixels = nil
	}
	return err
}
