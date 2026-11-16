//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && amd64)

package video

// Explicit GStreamer hardware elements support VA-API, NVDEC and V4L2 on
// Linux without a cgo/FFmpeg ABI dependency. Never use decodebin: it could
// silently substitute a software decoder while reporting a hardware backend.
import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/ebitengine/purego"
)

var ErrNoParameterSets = errors.New("video: no parameter sets yet")

// Public GStreamer 1.x GstMiniObject/GstBuffer layout on supported 64-bit hosts.
// PTS is a macro field, not an exported setter; buffers are writable here only
// before appsrc takes ownership. Output PTS identifies reordered/dropped frames.
type gstBufferHeader struct {
	Type                                  uintptr
	RefCount, LockState                   int32
	Flags                                 uint32
	Copy, Dispose, Free                   uintptr
	PrivateUint                           uint32
	PrivatePointer, Pool                  uintptr
	PTS, DTS, Duration, Offset, OffsetEnd uint64
}

type gstMapInfo struct {
	Memory         uintptr
	Flags          uint32
	Data           *byte
	Size, MaxSize  uintptr
	User, Reserved [4]uintptr
}
type gstError struct {
	Domain  uint32
	Code    int32
	Message *byte
}

var gst struct {
	sync.Once
	err                      error
	init                     func(uintptr, uintptr, **gstError) bool
	parse                    func(string, **gstError) uintptr
	factory                  func(string) uintptr
	get                      func(uintptr, string) uintptr
	state                    func(uintptr, int32) int32
	bus                      func(uintptr) uintptr
	errorMessage             func(uintptr, uint64, uint32) uintptr
	parseError               func(uintptr, **gstError, *uintptr)
	objectUnref, miniUnref   func(uintptr)
	errorFree                func(*gstError)
	free                     func(uintptr)
	allocate                 func(uintptr, uintptr, uintptr) uintptr
	fill                     func(uintptr, uintptr, *byte, uintptr) uintptr
	push                     func(uintptr, uintptr) int32
	queued                   func(uintptr) uint64
	pull                     func(uintptr, uint64) uintptr
	sampleBuffer, sampleCaps func(uintptr) uintptr
	structure                func(uintptr, uint32) uintptr
	getInt                   func(uintptr, string, *int32) bool
	mapBuffer                func(uintptr, *gstMapInfo, uint32) bool
	unmapBuffer              func(uintptr, *gstMapInfo)
	videoMeta                func(uintptr) unsafe.Pointer
	mapMeta                  func(uintptr, uint32, *gstMapInfo, **byte, *int32, uint32) bool
	unmapMeta                func(uintptr, uint32, *gstMapInfo) bool
}

func loadGStreamer() {
	libraries := []struct {
		name      string
		functions map[string]any
	}{
		{"libgstreamer-1.0.so.0", map[string]any{
			"gst_init_check": &gst.init, "gst_parse_launch": &gst.parse, "gst_element_factory_find": &gst.factory, "gst_bin_get_by_name": &gst.get, "gst_element_set_state": &gst.state, "gst_element_get_bus": &gst.bus,
			"gst_bus_timed_pop_filtered": &gst.errorMessage, "gst_message_parse_error": &gst.parseError, "gst_object_unref": &gst.objectUnref, "gst_mini_object_unref": &gst.miniUnref,
			"gst_buffer_new_allocate": &gst.allocate, "gst_buffer_fill": &gst.fill, "gst_sample_get_buffer": &gst.sampleBuffer, "gst_sample_get_caps": &gst.sampleCaps,
			"gst_caps_get_structure": &gst.structure, "gst_structure_get_int": &gst.getInt, "gst_buffer_map": &gst.mapBuffer, "gst_buffer_unmap": &gst.unmapBuffer,
		}},
		{"libgstapp-1.0.so.0", map[string]any{"gst_app_src_push_buffer": &gst.push, "gst_app_src_get_current_level_bytes": &gst.queued, "gst_app_sink_try_pull_sample": &gst.pull}},
		{"libgstvideo-1.0.so.0", map[string]any{"gst_buffer_get_video_meta": &gst.videoMeta, "gst_video_meta_map": &gst.mapMeta, "gst_video_meta_unmap": &gst.unmapMeta}},
		{"libglib-2.0.so.0", map[string]any{"g_error_free": &gst.errorFree, "g_free": &gst.free}},
	}
	for _, lib := range libraries {
		h, err := gstLibrary(lib.name)
		if err != nil {
			gst.err = fmt.Errorf("%w: %s: %v", ErrUnsupported, lib.name, err)
			return
		}
		for name, target := range lib.functions {
			address, err := purego.Dlsym(h, name)
			if err != nil {
				gst.err = err
				return
			}
			purego.RegisterFunc(target, address)
		}
	}
	var detail *gstError
	if !gst.init(0, 0, &detail) {
		gst.err = gstFailure("initialize", detail)
	}
}

func gstLibrary(name string) (uintptr, error) {
	paths := []string{name}
	if runtime.GOOS == "darwin" {
		name = strings.TrimSuffix(name, ".so.0") + ".0.dylib"
		paths = []string{name, filepath.Join("/usr/local/lib", name), filepath.Join("/Library/Frameworks/GStreamer.framework/Versions/1.0/lib", name)}
	}
	var err error
	for _, path := range paths {
		var handle uintptr
		handle, err = purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			return handle, nil
		}
	}
	return 0, err
}

func gstFailure(action string, detail *gstError) error {
	message := "unknown native error"
	if detail != nil {
		if detail.Message != nil {
			var text []byte
			for i := 0; i < 4096; i++ {
				b := *(*byte)(unsafe.Add(unsafe.Pointer(detail.Message), i))
				if b == 0 {
					break
				}
				text = append(text, b)
			}
			message = string(text)
		}
		gst.errorFree(detail)
	}
	return fmt.Errorf("video: GStreamer %s: %s", action, message)
}

type Decoder struct {
	pts                         uint64
	timing                      frameTimes
	codec                       Codec
	onFrame                     func(Frame)
	pipeline, source, sink, bus uintptr
	stop, exited                chan struct{}
	once                        sync.Once
	mu                          sync.Mutex
	status                      DecoderStatus
	native                      bool
}

func NewDecoder(codec Codec, onFrame func(Frame)) (*Decoder, error) {
	return newGStreamerDecoder(codec, onFrame, false)
}

func newGStreamerDecoder(codec Codec, onFrame func(Frame), native bool) (*Decoder, error) {
	if codec != H264 && codec != HEVC {
		return nil, ErrUnsupported
	}
	gst.Do(loadGStreamer)
	if gst.err != nil {
		return nil, gst.err
	}
	name, parser := "h264", "h264parse"
	if codec == HEVC {
		name, parser = "h265", "h265parse"
	}
	var last error
	candidates := []struct{ prefix, backend, post string }{
		{"va", "VA-API", ""}, {"vaapi", "VA-API", " ! vaapipostproc"}, {"nv", "NVDEC", ""}, {"v4l2sl", "V4L2", ""}, {"v4l2", "V4L2", ""},
	}
	if runtime.GOOS == "darwin" {
		candidates = []struct{ prefix, backend, post string }{{"vtdec_hw", "VideoToolbox", ""}}
	}
	for _, candidate := range candidates {
		factory := candidate.prefix + name + "dec"
		if candidate.prefix == "vtdec_hw" {
			factory = "vtdec_hw"
		}
		if native && !nativeGStreamerFactory(factory) {
			continue
		}
		available := gst.factory(factory)
		if available == 0 {
			continue
		}
		gst.objectUnref(available)
		d := &Decoder{native: native, codec: codec, onFrame: onFrame, stop: make(chan struct{}), exited: make(chan struct{}), status: DecoderStatus{Codec: codec.String(), Backend: "GStreamer / " + candidate.backend, Name: factory}}
		// DecodeTimed supplies a PTS token. appsrc auto-timestamping rewrites it
		// after startup and disconnects decoded frames from their source timing.
		pipeline := "appsrc name=input is-live=true format=time do-timestamp=false block=false caps=\"video/x-" + name + ",stream-format=(string)byte-stream,alignment=(string)au\" ! " + parser + " ! " + factory + candidate.post + " ! videoconvert ! video/x-raw,format=BGRA ! appsink name=output sync=false max-buffers=2 drop=true"
		if native {
			pipeline = "appsrc name=input is-live=true format=time do-timestamp=false block=false caps=\"video/x-" + name + ",stream-format=(string)byte-stream,alignment=(string)au\" ! " + parser + " ! " + factory + " ! video/x-raw(memory:DMABuf),format=DMA_DRM ! appsink name=output emit-signals=true sync=false max-buffers=2 drop=true"
		}
		var detail *gstError
		d.pipeline = gst.parse(pipeline, &detail)
		if detail != nil {
			last = gstFailure("create pipeline", detail)
			if d.pipeline != 0 {
				gst.objectUnref(d.pipeline)
			}
			continue
		}
		if d.pipeline == 0 {
			continue
		}
		d.source, d.sink, d.bus = gst.get(d.pipeline, "input"), gst.get(d.pipeline, "output"), gst.bus(d.pipeline)
		if d.source == 0 || d.sink == 0 || d.bus == 0 {
			last = errors.New("video: incomplete GStreamer pipeline")
			d.release()
			continue
		}
		if native {
			configureNativeSink(d.sink)
		}
		if gst.state(d.pipeline, 4) == 0 {
			last = d.busError()
			if last == nil {
				last = errors.New("video: hardware decoder failed to start")
			}
			gst.state(d.pipeline, 1)
			d.release()
			continue
		}
		go d.output()
		return d, nil
	}
	if last != nil {
		return nil, last
	}
	return nil, fmt.Errorf("%w: install the GStreamer %s hardware decoder plugin and its GPU driver", ErrUnsupported, strings.ToUpper(name))
}

func (d *Decoder) Status() DecoderStatus { d.mu.Lock(); defer d.mu.Unlock(); return d.status }
func (d *Decoder) fail(err error)        { d.mu.Lock(); d.status.Error = err.Error(); d.mu.Unlock() }
func (d *Decoder) busError() error {
	message := gst.errorMessage(d.bus, 0, 2)
	if message == 0 {
		return nil
	}
	defer gst.miniUnref(message)
	var detail *gstError
	var debug uintptr
	gst.parseError(message, &detail, &debug)
	if debug != 0 {
		gst.free(debug)
	}
	return gstFailure("decode", detail)
}

func (d *Decoder) Decode(nals [][]byte) error { return d.DecodeTimed(nals, frame.Timing{}) }
func (d *Decoder) DecodeTimed(nals [][]byte, timing frame.Timing) error {
	select {
	case <-d.stop:
		return errors.New("video: decoder is closed")
	default:
	}
	if message := d.Status().Error; message != "" {
		return errors.New(message)
	}
	if err := d.busError(); err != nil {
		d.fail(err)
		return err
	}
	if gst.queued(d.source) > 8*1024*1024 {
		return errors.New("video: Linux decoder input backlog exceeded")
	}
	if d.codec == H264 && d.Status().Name == "vah264dec" {
		for i, nal := range nals {
			if len(nal) > 0 && nal[0]&31 == 7 {
				// Do not mutate the caller's NAL descriptors or stream bytes.
				nals = append([][]byte(nil), nals...)
				nals[i] = h264LowLatencySPS(nal)
			}
		}
	}
	total := 0
	for _, nal := range nals {
		if len(nal) > 64*1024*1024-total-4 {
			return errors.New("video: access unit is too large")
		}
		total += 4 + len(nal)
	}
	if total == 0 {
		return nil
	}
	buffer := gst.allocate(0, uintptr(total), 0)
	if buffer == 0 {
		return errors.New("video: could not allocate GStreamer buffer")
	}
	var start = [4]byte{0, 0, 0, 1}
	offset := uintptr(0)
	for _, nal := range nals {
		if gst.fill(buffer, offset, &start[0], 4) != 4 {
			gst.miniUnref(buffer)
			return errors.New("video: failed to write NAL start code")
		}
		offset += 4
		if len(nal) > 0 {
			if gst.fill(buffer, offset, &nal[0], uintptr(len(nal))) != uintptr(len(nal)) {
				gst.miniUnref(buffer)
				return errors.New("video: failed to write NAL unit")
			}
			offset += uintptr(len(nal))
		}
	}
	runtime.KeepAlive(nals)
	d.pts += 16_666_667
	token := d.pts
	(*gstBufferHeader)(unsafe.Pointer(buffer)).PTS = token
	d.timing.put(token, timing)
	if result := gst.push(d.source, buffer); result != 0 {
		d.timing.take(token)
		return fmt.Errorf("video: GStreamer input flow %d", result)
	} // push takes ownership
	return nil
}

func (d *Decoder) output() {
	defer close(d.exited)
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		sample := gst.pull(d.sink, 10_000_000)
		if sample != 0 {
			err := d.picture(sample)
			gst.miniUnref(sample)
			if err != nil {
				d.fail(err)
				return
			}
		}
		if err := d.busError(); err != nil {
			d.fail(err)
			return
		}
	}
}

func (d *Decoder) picture(sample uintptr) error {
	if d.native {
		return d.nativePicture(sample)
	}
	buffer, caps := gst.sampleBuffer(sample), gst.sampleCaps(sample)
	if buffer == 0 || caps == 0 {
		return errors.New("video: missing Linux decoder output")
	}
	structure := gst.structure(caps, 0)
	var width, height int32
	if !gst.getInt(structure, "width", &width) || !gst.getInt(structure, "height", &height) || width <= 0 || height <= 0 || width > 16384 || height > 16384 || int64(width)*int64(height) > 64*1024*1024 {
		return errors.New("video: invalid Linux decoded dimensions")
	}
	var info gstMapInfo
	var pixels *byte
	stride := width * 4
	meta := gst.videoMeta(buffer)
	if meta != nil {
		if !gst.mapMeta(uintptr(meta), 0, &info, &pixels, &stride, 1) {
			return errors.New("video: could not map Linux video plane")
		}
		defer gst.unmapMeta(uintptr(meta), 0, &info)
	} else {
		if !gst.mapBuffer(buffer, &info, 1) {
			return errors.New("video: could not map Linux video buffer")
		}
		defer gst.unmapBuffer(buffer, &info)
		pixels = info.Data
	}
	if pixels == nil || info.Data == nil || stride < width*4 {
		return errors.New("video: invalid Linux BGRA plane")
	}
	offset := uintptr(unsafe.Pointer(pixels)) - uintptr(unsafe.Pointer(info.Data))
	length := uint64(height-1)*uint64(stride) + uint64(width)*4
	if uint64(offset) > uint64(info.Size) || length > uint64(info.Size)-uint64(offset) {
		return errors.New("video: Linux BGRA plane exceeds mapped memory")
	}
	d.mu.Lock()
	d.status.HardwareKnown, d.status.Hardware = true, true
	d.status.Conversion = "GStreamer BGRA readback"
	d.status.ReadbackFrames++
	d.mu.Unlock()
	if d.onFrame != nil {
		d.onFrame(Frame{Timing: d.timing.take((*gstBufferHeader)(unsafe.Pointer(buffer)).PTS), W: int(width), H: int(height), Stride: int(stride), Pix: unsafe.Slice(pixels, int(length))})
	}
	return nil
}

func (d *Decoder) release() {
	for _, p := range []uintptr{d.source, d.sink, d.bus, d.pipeline} {
		if p != 0 {
			gst.objectUnref(p)
		}
	}
}
func (d *Decoder) Close() {
	d.once.Do(func() { close(d.stop); gst.state(d.pipeline, 1); <-d.exited; d.release() })
}

// SetCPUFrames is configured before Decode, as with the other platform decoders.
func (d *Decoder) SetCPUFrames(cpu bool) {
	if cpu || d.native || !nativeGStreamerAvailable() {
		return
	}
	next, err := newGStreamerDecoder(d.codec, d.onFrame, true)
	if err != nil {
		return
	} // Preserve the working CPU-output pipeline.
	close(d.stop)
	gst.state(d.pipeline, 1)
	<-d.exited
	d.release()
	d.pipeline, d.source, d.sink, d.bus = next.pipeline, next.source, next.sink, next.bus
	// next.output owns next; redirect its callback/status through d by stopping
	// it before transferring ownership and starting the replacement reader.
	close(next.stop)
	<-next.exited
	d.stop, d.exited = make(chan struct{}), make(chan struct{})
	d.native, d.status = true, next.status
	go d.output()
}
