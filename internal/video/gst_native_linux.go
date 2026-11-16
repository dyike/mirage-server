//go:build linux && !android && (amd64 || arm64)

package video

import (
	"errors"
	"fmt"
	"image"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/dyike/keel/gio/op/paint"
	"github.com/ebitengine/purego"
)

// Only metadata is inspected. GstVideoMeta's plane offsets/strides describe
// the actual allocation, including padding and DRM modifiers, without mapping
// the (potentially tiled) video memory.
type gstVideoMeta struct {
	MetaFlags, _          uint32
	MetaInfo, Buffer      uintptr
	Flags, Format         uint32
	ID                    int32
	Width, Height, Planes uint32
	Offset                [4]uintptr
	Stride                [4]int32
}

var gstDMA struct {
	sync.Once
	err                                               error
	getString                                         func(uintptr, string) unsafe.Pointer
	peekMemory                                        func(uintptr, uint32) uintptr
	findMemory                                        func(uintptr, uintptr, uintptr, *uint32, *uint32, *uintptr) bool
	memorySizes                                       func(uintptr, *uintptr, *uintptr) uintptr
	isDMA                                             func(uintptr) bool
	fd                                                func(uintptr) int32
	ref                                               func(uintptr) uintptr
	pushSample                                        func(uintptr, uintptr) int32
	setCaps                                           func(uintptr, uintptr)
	colorimetry                                       func(*[4]int32, string) bool
	version                                           func(*uint32, *uint32, *uint32, *uint32)
	connect                                           func(uintptr, string, uintptr, uintptr, uintptr, uint32) uint64
	allocationMeta, videoMetaType, allocationCallback uintptr
}

var gstNativeSamples = struct {
	sync.Mutex
	all map[uintptr]uintptr
}{all: make(map[uintptr]uintptr)}

func loadGStreamerDMA() {
	for _, lib := range []struct {
		name    string
		symbols map[string]any
	}{
		{"libgstreamer-1.0.so.0", map[string]any{
			"gst_structure_get_string": &gstDMA.getString,
			"gst_buffer_peek_memory":   &gstDMA.peekMemory,
			"gst_buffer_find_memory":   &gstDMA.findMemory,
			"gst_memory_get_sizes":     &gstDMA.memorySizes,
			"gst_mini_object_ref":      &gstDMA.ref,
			"gst_version":              &gstDMA.version,
		}},
		{"libgstallocators-1.0.so.0", map[string]any{"gst_is_dmabuf_memory": &gstDMA.isDMA, "gst_dmabuf_memory_get_fd": &gstDMA.fd}},
		{"libgobject-2.0.so.0", map[string]any{"g_signal_connect_data": &gstDMA.connect}},
		{"libgstapp-1.0.so.0", map[string]any{"gst_app_src_push_sample": &gstDMA.pushSample, "gst_app_src_set_caps": &gstDMA.setCaps}},
		{"libgstvideo-1.0.so.0", map[string]any{"gst_video_colorimetry_from_string": &gstDMA.colorimetry}},
	} {
		h, err := gstLibrary(lib.name)
		if err != nil {
			gstDMA.err = err
			return
		}
		for name, target := range lib.symbols {
			address, err := purego.Dlsym(h, name)
			if err != nil {
				gstDMA.err = err
				return
			}
			purego.RegisterFunc(target, address)
		}
	}
	var major, minor, micro, nano uint32
	gstDMA.version(&major, &minor, &micro, &nano)
	if major != 1 || minor < 24 {
		gstDMA.err = errors.New("video: DMA-BUF output requires GStreamer 1.24 or later")
		return
	}
	core, _ := gstLibrary("libgstreamer-1.0.so.0")
	video, _ := gstLibrary("libgstvideo-1.0.so.0")
	gstDMA.allocationMeta, gstDMA.err = purego.Dlsym(core, "gst_query_add_allocation_meta")
	if gstDMA.err != nil {
		return
	}
	getType, err := purego.Dlsym(video, "gst_video_meta_api_get_type")
	if err != nil {
		gstDMA.err = err
		return
	}
	gstDMA.videoMetaType, _, _ = purego.SyscallN(getType)
	gstDMA.allocationCallback = purego.NewCallback(func(sink, query, data uintptr) uint32 {
		purego.SyscallN(gstDMA.allocationMeta, query, gstDMA.videoMetaType, 0)
		return 1
	})
}

func configureNativeSink(sink uintptr) {
	gstDMA.connect(sink, "propose-allocation", gstDMA.allocationCallback, 0, 0, 0)
}

func gstCString(ptr unsafe.Pointer) string {
	if ptr == nil {
		return ""
	}
	var b []byte
	for i := 0; i < 4096; i++ {
		c := *(*byte)(unsafe.Add(ptr, i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

func nativeGStreamerAvailable() bool {
	if os.Getenv("MIRAGE_LINUX_ZERO_COPY") == "0" || !paint.DMABufSupported() {
		return false
	}
	gstDMA.Do(loadGStreamerDMA)
	return gstDMA.err == nil
}

func nativeGStreamerFactory(factory string) bool {
	// Modern VA decoders negotiate DRM modifiers with their allocator. Other
	// hardware backends keep their existing, working output path.
	return factory == "vah264dec" || factory == "vah265dec"
}

func parseDRMFormat(value string) (uint32, uint64, error) {
	parts := strings.Split(value, ":")
	if len(parts) > 2 || len(parts[0]) != 4 {
		return 0, 0, fmt.Errorf("video: invalid DRM format %q", value)
	}
	fourcc := uint32(parts[0][0]) | uint32(parts[0][1])<<8 | uint32(parts[0][2])<<16 | uint32(parts[0][3])<<24
	var modifier uint64
	if len(parts) == 2 {
		var err error
		modifier, err = strconv.ParseUint(parts[1], 0, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("video: invalid DRM modifier: %w", err)
		}
	}
	return fourcc, modifier, nil
}

func (d *Decoder) nativePicture(sample uintptr) error {
	buffer, caps := gst.sampleBuffer(sample), gst.sampleCaps(sample)
	if buffer == 0 || caps == 0 {
		return errors.New("video: missing DMA-BUF sample")
	}
	structure := gst.structure(caps, 0)
	var width, height int32
	if !gst.getInt(structure, "width", &width) || !gst.getInt(structure, "height", &height) || width <= 0 || height <= 0 || width > 16384 || height > 16384 || int64(width)*int64(height) > 64*1024*1024 {
		return errors.New("video: invalid DMA-BUF dimensions")
	}
	fourcc, modifier, err := parseDRMFormat(gstCString(gstDMA.getString(structure, "drm-format")))
	if err != nil {
		return err
	}
	if fourcc != 0x3231564e && fourcc != 0x30313050 {
		return fmt.Errorf("video: unsupported zero-copy DRM format %#x", fourcc)
	}
	metaPtr := gst.videoMeta(buffer)
	if metaPtr == nil {
		return errors.New("video: DMA-BUF is missing plane metadata")
	}
	meta := (*gstVideoMeta)(metaPtr)
	if meta.Planes != 2 || meta.Width < uint32(width) || meta.Height < uint32(height) {
		return errors.New("video: invalid DMA-BUF plane geometry")
	}
	img := paint.DMABuf{Width: int(width), Height: int(height), FourCC: fourcc, OnError: d.fail}
	for i := uint32(0); i < meta.Planes; i++ {
		var index, count uint32
		var skip uintptr
		if meta.Stride[i] <= 0 || !gstDMA.findMemory(buffer, meta.Offset[i], 1, &index, &count, &skip) {
			return errors.New("video: invalid DMA-BUF plane offset")
		}
		memory := gstDMA.peekMemory(buffer, index)
		if memory == 0 || !gstDMA.isDMA(memory) {
			return errors.New("video: decoder output is not DMA-BUF memory")
		}
		var offset, maxSize uintptr
		size := gstDMA.memorySizes(memory, &offset, &maxSize)
		if skip >= size || offset+skip > 1<<31-1 {
			return errors.New("video: DMA-BUF offset exceeds allocation")
		}
		img.Planes = append(img.Planes, paint.DMABufPlane{FD: int(gstDMA.fd(memory)), Offset: uint32(offset + skip), Pitch: uint32(meta.Stride[i]), Modifier: modifier})
	}
	var color [4]int32
	if gstDMA.colorimetry(&color, gstCString(gstDMA.getString(structure, "colorimetry"))) {
		img.FullRange = color[0] == 1
		switch color[1] {
		case 3:
			img.ColorSpace = 709
		case 6:
			img.ColorSpace = 2020
		default:
			img.ColorSpace = 601
		}
	} else if height > 576 {
		img.ColorSpace = 709
	}
	// Hold the GstSample, not just duplicated FDs: its VA buffer-pool slot
	// must not be reused until the renderer's fence signals.
	gstDMA.ref(sample)
	var handle uintptr
	handle, err = paint.RegisterDMABuf(img, func() {
		gstNativeSamples.Lock()
		delete(gstNativeSamples.all, handle)
		gstNativeSamples.Unlock()
		gst.miniUnref(sample)
	})
	if err != nil {
		gst.miniUnref(sample)
		return err
	}
	gstNativeSamples.Lock()
	gstNativeSamples.all[handle] = sample
	gstNativeSamples.Unlock()
	defer paint.ReleaseDMABuf(handle)
	d.mu.Lock()
	d.status.HardwareKnown, d.status.Hardware, d.status.ZeroCopy = true, true, true
	d.status.Conversion = "DMA-BUF / EGL external texture (zero-copy)"
	d.status.NativeFrames++
	d.mu.Unlock()
	if d.onFrame != nil {
		d.onFrame(Frame{Timing: d.timing.take((*gstBufferHeader)(unsafe.Pointer(buffer)).PTS), W: int(width), H: int(height), Buffer: handle, Surface: handle})
	}
	return nil
}

func RetainBuffer(handle uintptr)  { paint.RetainDMABuf(handle) }
func ReleaseBuffer(handle uintptr) { paint.ReleaseDMABuf(handle) }

// Snapshots deliberately read back only on request; continuous rendering never
// calls this path. A separate pipeline leaves the live decoder untouched.
func Snapshot(handle uintptr) *image.RGBA {
	gstNativeSamples.Lock()
	sample := gstNativeSamples.all[handle]
	if sample != 0 {
		gstDMA.ref(sample)
	}
	gstNativeSamples.Unlock()
	if sample == 0 {
		return nil
	}
	defer gst.miniUnref(sample)
	var detail *gstError
	pipeline := gst.parse("appsrc name=input format=time ! vapostproc ! video/x-raw,format=BGRA ! appsink name=output sync=false max-buffers=1", &detail)
	if detail != nil {
		_ = gstFailure("snapshot pipeline", detail)
		if pipeline != 0 {
			gst.objectUnref(pipeline)
		}
		return nil
	}
	if pipeline == 0 {
		return nil
	}
	defer gst.objectUnref(pipeline)
	defer gst.state(pipeline, 1)
	source, sink := gst.get(pipeline, "input"), gst.get(pipeline, "output")
	if source != 0 {
		defer gst.objectUnref(source)
	}
	if sink != 0 {
		defer gst.objectUnref(sink)
	}
	if source == 0 || sink == 0 {
		return nil
	}
	gstDMA.setCaps(source, gst.sampleCaps(sample))
	if gst.state(pipeline, 4) == 0 || gstDMA.pushSample(source, sample) != 0 {
		return nil
	}
	out := gst.pull(sink, 2_000_000_000)
	if out == 0 {
		return nil
	}
	defer gst.miniUnref(out)
	var img *image.RGBA
	d := &Decoder{onFrame: func(f Frame) {
		img = image.NewRGBA(image.Rect(0, 0, f.W, f.H))
		for y := 0; y < f.H; y++ {
			for x := 0; x < f.W; x++ {
				src, dst := y*f.Stride+x*4, y*img.Stride+x*4
				img.Pix[dst], img.Pix[dst+1], img.Pix[dst+2], img.Pix[dst+3] = f.Pix[src+2], f.Pix[src+1], f.Pix[src], f.Pix[src+3]
			}
		}
	}}
	if d.picture(out) != nil {
		return nil
	}
	return img
}

func CopyBGRA(handle uintptr, dst []byte, stride, width, height int) error {
	img := Snapshot(handle)
	if img == nil || width <= 0 || height <= 0 || width > img.Bounds().Dx() || height > img.Bounds().Dy() || stride < width*4 || len(dst) < (height-1)*stride+width*4 {
		return ErrUnsupported
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src, out := y*img.Stride+x*4, y*stride+x*4
			dst[out], dst[out+1], dst[out+2], dst[out+3] = img.Pix[src+2], img.Pix[src+1], img.Pix[src], img.Pix[src+3]
		}
	}
	return nil
}
