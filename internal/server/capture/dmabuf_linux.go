package capture

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/dyike/mirage-server/internal/server/input"
	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Public GStreamer 1.x ABI prefixes (GInitiallyUnowned is GObject).
// Override only this allocator instance's public memory-map virtual methods.
// A driver import failure must fail, never silently upload CPU pixels.
type gstObjectABI struct {
	Class          uintptr
	RefCount       uint32
	QData          uintptr
	Lock           uintptr
	Name, Parent   uintptr
	Flags          uint32
	Bindings       uintptr
	Rate, LastSync uint64
	Reserved       uintptr
}
type gstAllocatorABI struct {
	Object                                              gstObjectABI
	MemoryType                                          uintptr
	Map, Unmap, Copy, Share, IsSpan, MapFull, UnmapFull uintptr
}

var dmaAPI struct {
	peekMemory                   func(uintptr, uint32) uintptr
	once                         sync.Once
	err                          error
	allocatorNew                 func() *gstAllocatorABI
	alloc                        func(uintptr, int32, uintptr) uintptr
	bufferNew                    func() uintptr
	appendMemory                 func(uintptr, uintptr)
	videoMeta                    func(uintptr, uint32, int32, uint32, uint32, uint32, *uintptr, *int32) uintptr
	videoFormat                  func(string) int32
	weakRef                      func(uintptr, uintptr, uintptr)
	ref                          func(uintptr) uintptr
	memoryMap                    func(uintptr, *mapInfo, uint32) bool
	memoryUnmap                  func(uintptr, *mapInfo)
	releaseCallback, mapCallback uintptr
}
var dmaReleases sync.Map
var dmaNext atomic.Uint64
var dmaMapAttempts atomic.Uint64

func loadDMA() {
	for _, lib := range []struct {
		name    string
		symbols map[string]any
	}{
		{"libgstallocators-1.0.so.0", map[string]any{"gst_dmabuf_allocator_new": &dmaAPI.allocatorNew, "gst_dmabuf_allocator_alloc": &dmaAPI.alloc}},
		{"libgstreamer-1.0.so.0", map[string]any{"gst_buffer_peek_memory": &dmaAPI.peekMemory, "gst_buffer_new": &dmaAPI.bufferNew, "gst_buffer_append_memory": &dmaAPI.appendMemory, "gst_mini_object_weak_ref": &dmaAPI.weakRef, "gst_mini_object_ref": &dmaAPI.ref, "gst_memory_map": &dmaAPI.memoryMap, "gst_memory_unmap": &dmaAPI.memoryUnmap}},
		{"libgstvideo-1.0.so.0", map[string]any{"gst_buffer_add_video_meta_full": &dmaAPI.videoMeta, "gst_video_format_from_string": &dmaAPI.videoFormat}},
	} {
		h, e := purego.Dlopen(lib.name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if e != nil {
			dmaAPI.err = e
			return
		}
		for name, fn := range lib.symbols {
			p, e := purego.Dlsym(h, name)
			if e != nil {
				dmaAPI.err = e
				return
			}
			purego.RegisterFunc(fn, p)
		}
	}
	dmaAPI.releaseCallback = purego.NewCallback(func(data, object uintptr) {
		if f, ok := dmaReleases.LoadAndDelete(data); ok {
			f.(func())()
		}
	})
	dmaAPI.mapCallback = purego.NewCallback(func(memory, info, flags uintptr) uintptr { dmaMapAttempts.Add(1); return 0 })
}
func newDMAAllocator() (uintptr, error) {
	api.once.Do(load)
	if api.err != nil {
		return 0, api.err
	}
	dmaAPI.once.Do(loadDMA)
	if dmaAPI.err != nil {
		return 0, dmaAPI.err
	}
	a := dmaAPI.allocatorNew()
	if a == nil {
		return 0, errors.New("cannot create DMA allocator")
	}
	layout := a
	layout.Map = dmaAPI.mapCallback
	layout.MapFull = dmaAPI.mapCallback
	return uintptr(unsafe.Pointer(a)), nil
}
func wrapDMA(a uintptr, b *input.DMABuffer, release func()) (uintptr, error) {
	fd, err := unix.FcntlInt(uintptr(b.FD), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	mem := dmaAPI.alloc(a, int32(fd), uintptr(b.Stride*b.Height))
	if mem == 0 {
		unix.Close(fd)
		return 0, errors.New("cannot wrap DMA memory")
	}
	buffer := dmaAPI.bufferNew()
	if buffer == 0 {
		api.miniUnref(mem)
		return 0, errors.New("cannot create DMA frame")
	}
	dmaAPI.appendMemory(buffer, mem) // transfers ownership
	offsets := [4]uintptr{}
	strides := [4]int32{int32(b.Stride)}
	format := "BGRx"
	if b.Format == 0x34325241 {
		format = "BGRA"
	}
	if dmaAPI.videoMeta(buffer, 0, dmaAPI.videoFormat(format), uint32(b.Width), uint32(b.Height), 1, &offsets[0], &strides[0]) == 0 {
		api.miniUnref(buffer)
		return 0, errors.New("cannot attach DMA video layout")
	}
	token := uintptr(dmaNext.Add(1))
	dmaReleases.Store(token, release)
	// Memory can outlive its first GstBuffer (e.g. parent/import metadata).
	dmaAPI.weakRef(mem, dmaAPI.releaseCallback, token)
	return buffer, nil
}
func runDMAOutput(ctx context.Context, o Options, consume func(Frame) error) error {
	screen, err := input.OpenScreen(ctx, o.Output, o.EmbeddedCursor)
	if err != nil {
		return err
	}
	defer screen.Close()
	pool, err := screen.NewDMAPool(6)
	if err != nil {
		return fmt.Errorf("zero-copy output capture: %w", err)
	}
	defer pool.Close()
	a, err := newDMAAllocator()
	if err != nil {
		return err
	}
	defer api.unref(a)
	first := pool.Buffers[0]
	transform, err := pool.Capture(ctx, first)
	if err != nil {
		return err
	}
	if transform != 0 {
		return fmt.Errorf("zero-copy capture does not yet support output transform %d", transform)
	}
	o.SourceWidth, o.SourceHeight = first.Width, first.Height
	o.DMA = true
	o.DMAFormat = "BGRx"
	if first.Format == 0x34325241 {
		o.DMAFormat = "BGRA"
	}
	description, err := Pipeline(o)
	if err != nil {
		return err
	}
	available := make(chan *input.DMABuffer, len(pool.Buffers))
	for _, b := range pool.Buffers[1:] {
		available <- b
	}
	mapsBefore := dmaMapAttempts.Load()
	var submitted atomic.Uint64
	defer func() {
		log.Printf("native DMA-BUF output=%s submitted=%d CPU-map-attempts=%d (mapping prohibited)", o.Output, submitted.Load(), dmaMapAttempts.Load()-mapsBefore)
	}()
	log.Printf("native DMA-BUF %dx%d stride=%d modifier=%#x pool=%d", first.Width, first.Height, first.Stride, first.Modifier, len(pool.Buffers))
	return runPipelineFeed(ctx, description, func(data []byte) error { return consume(Frame{Data: data, Width: o.Width, Height: o.Height}) }, func(ctx context.Context, source uintptr) error {
		if o.RepeatIdle {
			return feedDMAContinuous(ctx, o, first, available, pool.Capture, func(b *input.DMABuffer, release func()) error {
				buffer, err := wrapDMA(a, b, release)
				if err != nil {
					release()
					return err
				}
				submitted.Add(1)
				if flow := api.pushBuffer(source, buffer); flow != 0 {
					return fmt.Errorf("DMA appsrc flow %d", flow)
				}
				return nil
			})
		}
		currentFPS := o.FPS
		ticker := time.NewTicker(time.Second / time.Duration(currentFPS))
		defer ticker.Stop()
		b := first
		for {
			captured := b
			buffer, err := wrapDMA(a, b, func() { available <- captured })
			if err != nil {
				return err
			}
			submitted.Add(1)
			if flow := api.pushBuffer(source, buffer); flow != 0 {
				return fmt.Errorf("DMA appsrc flow %d", flow)
			}
			if o.FPSNow != nil {
				if fps := max(1, min(o.FPS, o.FPSNow())); fps != currentFPS {
					currentFPS = fps
					ticker.Reset(time.Second / time.Duration(fps))
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case b = <-available:
			}
			transform, err = pool.Capture(ctx, b)
			if err != nil {
				return err
			}
			if transform != 0 {
				return fmt.Errorf("output transform changed; DMA capture stopped")
			}
		}
	}, encoderControls{o.BindBitrate, o.BindKeyframe, rawFrameBytes(o)})
}
