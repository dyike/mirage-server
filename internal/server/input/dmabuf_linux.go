package input

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

var gbm struct {
	once          sync.Once
	err           error
	createDevice  func(int32) uintptr
	destroyDevice func(uintptr)
	createBO      func(uintptr, uint32, uint32, uint32, *uint64, uint32, uint32) uintptr
	destroyBO     func(uintptr)
	fd            func(uintptr) int32
	stride        func(uintptr) uint32
	modifier      func(uintptr) uint64
	planes        func(uintptr) int32
	offset        func(uintptr, int32) uint32
}

func loadGBM() {
	h, err := purego.Dlopen("libgbm.so.1", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		gbm.err = err
		return
	}
	for name, fn := range map[string]any{
		"gbm_create_device": &gbm.createDevice, "gbm_device_destroy": &gbm.destroyDevice,
		"gbm_bo_create_with_modifiers2": &gbm.createBO, "gbm_bo_destroy": &gbm.destroyBO,
		"gbm_bo_get_fd": &gbm.fd, "gbm_bo_get_stride": &gbm.stride, "gbm_bo_get_modifier": &gbm.modifier,
		"gbm_bo_get_plane_count": &gbm.planes, "gbm_bo_get_offset": &gbm.offset,
	} {
		p, e := purego.Dlsym(h, name)
		if e != nil {
			gbm.err = e
			return
		}
		purego.RegisterFunc(fn, p)
	}
}

// DMABuffer owns a GPU allocation. It must not be captured into again while
// an encoder holds a reference. No method maps the allocation into CPU memory.
type DMABuffer struct {
	FD                    int
	Width, Height, Stride int
	Format                uint32
	Modifier              uint64
	bo                    uintptr
	wl                    uint32
}
type DMAPool struct {
	screen        *ScreenCapture
	width, height int
	device        uintptr
	fd            int
	Buffers       []*DMABuffer
}

func renderNode(device uint64) (string, error) {
	target, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/char/%d:%d/device", unix.Major(device), unix.Minor(device)))
	if err != nil {
		return "", err
	}
	paths, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		var st unix.Stat_t
		if unix.Stat(path, &st) != nil {
			continue
		}
		candidate, e := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/char/%d:%d/device", unix.Major(st.Rdev), unix.Minor(st.Rdev)))
		if e == nil && candidate == target {
			return path, nil
		}
	}
	return "", errors.New("no render node matching compositor capture device")
}
func (s *ScreenCapture) NewDMAPool(count int) (*DMAPool, error) {
	if count < 2 || count > 16 || s.dmabuf == 0 || s.device == 0 {
		return nil, errors.New("DMA-BUF capture unavailable")
	}
	gbm.once.Do(loadGBM)
	if gbm.err != nil {
		return nil, gbm.err
	}
	path, err := renderNode(s.device)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	p := &DMAPool{screen: s, fd: fd, device: gbm.createDevice(int32(fd))}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	if p.device == 0 {
		return nil, errors.New("cannot create GBM device")
	}
	// Prefer linear XRGB/ARGB: directly importable by VA-API across GPUs without
	// interpreting vendor tiling. Never silently fall back to shared memory.
	for _, format := range []uint32{0x34325258, 0x34325241} {
		for _, mod := range s.dmaFormats[format] {
			if mod != 0 {
				continue
			}
			bo := gbm.createBO(p.device, uint32(s.Width), uint32(s.Height), format, &mod, 1, 4)
			if bo == 0 {
				continue
			}
			b, e := p.add(bo, format)
			if e != nil {
				return nil, e
			}
			for len(p.Buffers) < count {
				bo = gbm.createBO(p.device, uint32(s.Width), uint32(s.Height), format, &mod, 1, 4)
				if bo == 0 {
					return nil, errors.New("cannot allocate DMA capture pool")
				}
				if _, e = p.add(bo, format); e != nil {
					return nil, e
				}
			}
			if b.Modifier != 0 {
				return nil, errors.New("GBM did not honor linear modifier")
			}
			s.dmaMode = true
			ok = true
			return p, nil
		}
	}
	return nil, fmt.Errorf("no allocatable linear XRGB/ARGB DMA format (offered %v)", s.dmaFormats)
}
func (p *DMAPool) add(bo uintptr, format uint32) (*DMABuffer, error) {
	w, h := p.width, p.height
	if p.screen != nil {
		w, h = p.screen.Width, p.screen.Height
	}
	b := &DMABuffer{FD: -1, bo: bo, Width: w, Height: h, Format: format, Modifier: gbm.modifier(bo), Stride: int(gbm.stride(bo))}
	p.Buffers = append(p.Buffers, b)
	if gbm.planes(bo) != 1 || gbm.offset(bo, 0) != 0 || b.Modifier != 0 || b.Stride < b.Width*4 {
		return nil, errors.New("unsupported DMA plane layout")
	}
	b.FD = int(gbm.fd(bo))
	if b.FD < 0 {
		return nil, errors.New("cannot export DMA fd")
	}
	unix.CloseOnExec(b.FD)
	if p.screen == nil {
		return b, nil
	}
	wl := p.screen.w
	params := wl.id()
	b.wl = wl.id()
	if err := wl.send(p.screen.dmabuf, 1, words(params), -1); err != nil {
		return nil, err
	}
	if err := wl.send(params, 1, words(0, 0, uint32(b.Stride), uint32(b.Modifier>>32), uint32(b.Modifier)), b.FD); err != nil {
		return nil, err
	}
	if err := wl.send(params, 3, words(b.wl, uint32(b.Width), uint32(b.Height), format, 0), -1); err != nil {
		return nil, err
	}
	if err := wl.send(params, 0, nil, -1); err != nil {
		return nil, err
	}
	return b, nil
}
func (p *DMAPool) Capture(ctx context.Context, b *DMABuffer) (uint32, error) {
	return p.screen.captureBuffer(ctx, b.wl)
}

// Close is called only after GStreamer has released every borrowed allocation.
func (p *DMAPool) Close() {
	for _, b := range p.Buffers {
		if b.wl != 0 {
			p.screen.w.send(b.wl, 0, nil, -1)
		}
		if b.FD >= 0 {
			unix.Close(b.FD)
		}
		if b.bo != 0 {
			gbm.destroyBO(b.bo)
		}
	}
	p.Buffers = nil
	if p.device != 0 {
		gbm.destroyDevice(p.device)
		p.device = 0
	}
	if p.fd >= 0 {
		unix.Close(p.fd)
		p.fd = -1
	}
}

// NewRenderPool allocates linear GPU render targets on the capture device.
// These buffers are never attached to Wayland or mapped into CPU memory.
func (s *ScreenCapture) NewRenderPool(width, height, count int) (*DMAPool, error) {
	if width < 64 || height < 64 || width > 8192 || height > 8192 || count < 2 || count > 16 {
		return nil, errors.New("invalid render pool dimensions")
	}
	gbm.once.Do(loadGBM)
	if gbm.err != nil {
		return nil, gbm.err
	}
	path, err := renderNode(s.device)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	p := &DMAPool{fd: fd, device: gbm.createDevice(int32(fd)), width: width, height: height}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	if p.device == 0 {
		return nil, errors.New("cannot create render device")
	}
	var mod uint64
	for range count {
		bo := gbm.createBO(p.device, uint32(width), uint32(height), 0x34325241, &mod, 1, 4)
		if bo == 0 {
			return nil, errors.New("cannot allocate linear GPU render target")
		}
		if _, err = p.add(bo, 0x34325241); err != nil {
			return nil, err
		}
	}
	ok = true
	return p, nil
}
