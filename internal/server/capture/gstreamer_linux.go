package capture

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

type Layer struct {
	Output                    string
	SourceWidth, SourceHeight int
	Format                    string
	Node                      uint32
	FD, X, Y, Width, Height   int
}
type Options struct {
	ComposeOnly                 bool // internal GL surface output for the DMA render-target bridge
	MinQP                       int
	RepeatIdle                  bool // ARD receivers require periodic video even without Wayland damage.
	FPSNow                      func() int
	BindBitrate                 func(func(int))
	BindKeyframe                func(func() bool)
	DMA                         bool
	DMAFormat                   string
	Output                      string
	EmbeddedCursor              bool
	SourceWidth, SourceHeight   int
	Layers                      []Layer
	Test                        bool
	Node                        uint32
	FD                          int
	Codec                       string
	Width, Height, FPS, Bitrate int
}
type Frame struct {
	Data          []byte
	Width, Height int
}
type mapInfo struct {
	Memory         uintptr
	Flags          uint32
	Data           *byte
	Size, Max      uintptr
	User, Reserved [4]uintptr
}
type nativeError struct {
	Domain  uint32
	Code    int32
	Message *byte
}

var api struct {
	once        sync.Once
	err         error
	init        func(uintptr, uintptr, **nativeError) bool
	parse       func(string, **nativeError) uintptr
	setArg      func(uintptr, string, string)
	get         func(uintptr, string) uintptr
	state       func(uintptr, int32) int32
	unref       func(uintptr)
	miniUnref   func(uintptr)
	pull        func(uintptr, uint64) uintptr
	buffer      func(uintptr) uintptr
	caps        func(uintptr) uintptr
	structure   func(uintptr, uint32) uintptr
	getInt      func(uintptr, string, *int32) bool
	mapBuf      func(uintptr, *mapInfo, uint32) bool
	unmap       func(uintptr, *mapInfo)
	bus         func(uintptr) uintptr
	pop         func(uintptr, uint64, uint32) uintptr
	parseError  func(uintptr, **nativeError, *uintptr)
	freeError   func(*nativeError)
	free        func(uintptr)
	allocBuffer func(uintptr, uintptr, uintptr) uintptr
	fillBuffer  func(uintptr, uintptr, *byte, uintptr) uintptr
	pushBuffer  func(uintptr, uintptr) int32
	sendEvent   func(uintptr, uintptr) bool
	keyUnit     func(uint64, bool, uint32) uintptr
}

func cstring(p *byte) string {
	if p == nil {
		return "unknown native error"
	}
	b := make([]byte, 0, 128)
	for i := uintptr(0); i < 8192; i++ {
		v := *(*byte)(unsafe.Add(unsafe.Pointer(p), i))
		if v == 0 {
			break
		}
		b = append(b, v)
	}
	return string(b)
}
func load() {
	for _, lib := range []struct {
		name    string
		symbols map[string]any
	}{
		{"libgstreamer-1.0.so.0", map[string]any{"gst_element_send_event": &api.sendEvent, "gst_buffer_new_allocate": &api.allocBuffer, "gst_buffer_fill": &api.fillBuffer, "gst_init_check": &api.init, "gst_parse_launch": &api.parse, "gst_util_set_object_arg": &api.setArg, "gst_bin_get_by_name": &api.get, "gst_element_set_state": &api.state, "gst_object_unref": &api.unref, "gst_mini_object_unref": &api.miniUnref, "gst_sample_get_buffer": &api.buffer, "gst_sample_get_caps": &api.caps, "gst_caps_get_structure": &api.structure, "gst_structure_get_int": &api.getInt, "gst_buffer_map": &api.mapBuf, "gst_buffer_unmap": &api.unmap, "gst_element_get_bus": &api.bus, "gst_bus_timed_pop_filtered": &api.pop, "gst_message_parse_error": &api.parseError}},
		{"libgstvideo-1.0.so.0", map[string]any{"gst_video_event_new_upstream_force_key_unit": &api.keyUnit}},
		{"libgstapp-1.0.so.0", map[string]any{"gst_app_src_push_buffer": &api.pushBuffer, "gst_app_sink_try_pull_sample": &api.pull}},
		{"libglib-2.0.so.0", map[string]any{"g_error_free": &api.freeError, "g_free": &api.free}},
	} {
		h, err := purego.Dlopen(lib.name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			api.err = err
			return
		}
		for name, fn := range lib.symbols {
			p, err := purego.Dlsym(h, name)
			if err != nil {
				api.err = err
				return
			}
			purego.RegisterFunc(fn, p)
		}
	}
	var e *nativeError
	if !api.init(0, 0, &e) {
		api.err = consumeError(e)
	}
}
func consumeError(e *nativeError) error {
	if e == nil {
		return errors.New("GStreamer failed without detail")
	}
	s := cstring(e.Message)
	api.freeError(e)
	return errors.New(s)
}
func Pipeline(o Options) (string, error) {
	if o.ComposeOnly && (len(o.Layers) < 2 || o.Layers[0].Output == "" || o.Codec == "raw") {
		return "", errors.New("invalid GPU composition request")
	}
	if o.MinQP < 0 || o.MinQP > 51 || o.FPS < 1 || o.FPS > 120 || o.Bitrate < 128 || o.Bitrate > 200000 || o.Width < 64 || o.Height < 64 || o.Width > 8192 || o.Height > 8192 || o.Width%2 != 0 || o.Height%2 != 0 {
		return "", errors.New("invalid capture dimensions, FPS or bitrate")
	}
	encoder, parser, caps := "vah265enc", "h265parse", "video/x-h265"
	if o.Codec == "h264" {
		encoder, parser, caps = "vah264enc", "h264parse", "video/x-h264"
	} else if o.Codec != "hevc" && o.Codec != "raw" {
		return "", errors.New("unsupported codec")
	}
	source := fmt.Sprintf("pipewiresrc fd=%d path=%d do-timestamp=true", o.FD, o.Node)
	if o.Test {
		source = fmt.Sprintf("videotestsrc is-live=true pattern=ball ! video/x-raw,framerate=%d/1", o.FPS)
	} else if o.Output != "" {
		if o.SourceWidth < 1 || o.SourceHeight < 1 || o.SourceWidth > 8192 || o.SourceHeight > 8192 || len(o.Layers) > 0 {
			return "", errors.New("invalid native output source")
		}
		source = fmt.Sprintf("appsrc name=desktop_source is-live=true format=time do-timestamp=true block=true max-buffers=2 max-bytes=0 caps=video/x-raw,format=BGRx,width=%d,height=%d,framerate=%d/1", o.SourceWidth, o.SourceHeight, o.FPS)
		if o.DMA {
			if o.DMAFormat != "BGRx" && o.DMAFormat != "BGRA" {
				return "", errors.New("unsupported DMA pixel format")
			}
			source = strings.Replace(source, "format=BGRx", "format="+o.DMAFormat, 1)
		}
	} else if len(o.Layers) == 0 && (o.Node == 0 || o.FD < 0) {
		return "", errors.New("missing PipeWire portal stream")
	}
	branches := ""
	if len(o.Layers) > 0 {
		if len(o.Layers) > 16 {
			return "", errors.New("too many capture layers")
		}
		compositor := "vacompositor"
		nativeLayers := o.Layers[0].Output != ""
		if nativeLayers {
			compositor = "glvideomixerelement"
		}
		if o.Codec == "raw" {
			compositor = "compositor"
		}
		var layout, inputs strings.Builder
		fmt.Fprintf(&layout, "%s name=desktop", compositor)
		if nativeLayers && o.Codec == "raw" {
			layout.WriteString(" start-time-selection=first")
		}
		if nativeLayers {
			layout.WriteString(" background=black")
		}
		for i, l := range o.Layers {
			if l.X < 0 || l.Y < 0 || l.Width < 1 || l.Height < 1 || l.X+l.Width > o.Width || l.Y+l.Height > o.Height || (!o.Test && l.Output == "" && (l.Node == 0 || l.FD < 0)) {
				return "", errors.New("invalid capture layer")
			}
			fmt.Fprintf(&layout, " sink_%d::xpos=%d sink_%d::ypos=%d sink_%d::width=%d sink_%d::height=%d", i, l.X, i, l.Y, i, l.Width, i, l.Height)
			src := fmt.Sprintf("pipewiresrc fd=%d path=%d do-timestamp=true", l.FD, l.Node)
			if o.Test {
				src = fmt.Sprintf("videotestsrc is-live=true pattern=%d ! video/x-raw,width=320,height=240,framerate=%d/1", i%24, o.FPS)
			}
			if l.Output != "" {
				if l.SourceWidth < 1 || l.SourceHeight < 1 || (l.Format != "BGRx" && l.Format != "BGRA") {
					return "", errors.New("invalid native layer")
				}
				src = fmt.Sprintf("appsrc name=desktop_source_%d is-live=true format=time do-timestamp=true block=true max-buffers=2 max-bytes=0 caps=video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1", i, l.Format, l.SourceWidth, l.SourceHeight, o.FPS)
				if o.Codec == "raw" {
					// Raw layers repeat their last frame. Wait for every pad instead of
					// allowing live aggregation to emit an empty or half-empty canvas.
					src = strings.Replace(src, "is-live=true", "is-live=false", 1)
					src = strings.Replace(src, "do-timestamp=true", "do-timestamp=false", 1)
				}
				if o.Codec != "raw" {
					drm := "XR24"
					if l.Format == "BGRA" {
						drm = "AR24"
					}
					src = strings.Replace(src, "video/x-raw,format="+l.Format, "video/x-raw(memory:DMABuf),format=DMA_DRM,drm-format="+drm, 1)
				}
			}
			convert := "vapostproc ! video/x-raw(memory:VAMemory),format=NV12"
			if o.Codec == "raw" {
				convert = "videoconvert"
			}
			if nativeLayers && o.Codec != "raw" {
				convert = "glupload ! glcolorconvert ! video/x-raw(memory:GLMemory),format=RGBA"
			}
			fmt.Fprintf(&inputs, " %s ! queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream ! %s ! desktop.sink_%d", src, convert, i)
		}
		fmt.Fprintf(&layout, " ! video/x-raw(ANY),width=%d,height=%d,framerate=%d/1", o.Width, o.Height, o.FPS)
		source = layout.String()
		if nativeLayers && o.Codec != "raw" {
			if !o.ComposeOnly {
				return "", errors.New("GPU native layers require a render target bridge")
			}
		}
		branches = inputs.String()
	}
	if o.ComposeOnly {
		return source + " ! identity ! video/x-raw(memory:GLMemory),format=RGBA ! appsink name=encoded sync=false max-buffers=2 drop=true" + branches, nil
	}
	if o.Codec == "raw" {
		return fmt.Sprintf("%s ! videorate drop-only=true ! video/x-raw(ANY),framerate=%d/1 ! queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream ! videoconvertscale ! video/x-raw,format=RGBA,width=%d,height=%d ! appsink name=encoded sync=false max-buffers=2 drop=true", source, o.FPS, o.Width, o.Height) + branches, nil
	}
	// Large single-step downscales can crash Mesa VA processing. Bound each
	// native-source stage to 2x, keeping intermediate images in GPU memory.
	if o.Output != "" {
		sw, sh := o.SourceWidth, o.SourceHeight
		for sw > o.Width*2 || sh > o.Height*2 {
			sw = max(o.Width, ((sw+3)/4)*2)
			sh = max(o.Height, ((sh+3)/4)*2)
			source += fmt.Sprintf(" ! vapostproc ! video/x-raw(memory:VAMemory),format=NV12,width=%d,height=%d", sw, sh)
		}
	}
	// Drop raw frames before encoding; never drop reference pictures after encoding.
	// Keep only one encoded frame queued behind the network sender.
	return fmt.Sprintf("%s ! videorate drop-only=true ! video/x-raw(ANY),framerate=%d/1 ! queue max-size-buffers=2 max-size-bytes=0 max-size-time=0 leaky=downstream ! vapostproc ! video/x-raw(memory:VAMemory),format=NV12,width=%d,height=%d ! %s name=video_encoder min-qp=%d rate-control=vbr target-percentage=75 bitrate=%d cpb-size=%d key-int-max=%d b-frames=0 ! %s config-interval=-1 ! %s,stream-format=byte-stream,alignment=au ! appsink name=encoded sync=false max-buffers=1 drop=false", source, o.FPS, o.Width, o.Height, encoder, max(1, o.MinQP), o.Bitrate, max(128, o.Bitrate/4), o.FPS*2, parser, caps) + branches, nil
}
func Run(ctx context.Context, o Options, consume func(Frame) error) error {
	if len(o.Layers) > 0 && o.Layers[0].Output != "" {
		return runNativeLayers(ctx, o, consume)
	}
	if o.Output != "" && !o.Test {
		return runOutput(ctx, o, consume)
	}
	description, err := Pipeline(o)
	if err != nil {
		return err
	}
	return runPipelineFeed(ctx, description, func(data []byte) error { return consume(Frame{Data: data, Width: o.Width, Height: o.Height}) }, nil, encoderControls{o.BindBitrate, o.BindKeyframe, rawFrameBytes(o)})
}
func runPipeline(ctx context.Context, description string, consume func([]byte) error) error {
	return runPipelineFeed(ctx, description, consume, nil)
}

type encoderControls struct {
	bitrate  func(func(int))
	keyframe func(func() bool)
	rawBytes int
}

func runPipelineFeed(ctx context.Context, description string, consume func([]byte) error, feed func(context.Context, uintptr) error, bind ...encoderControls) error {
	return runPipelineSources(ctx, description, consume, []string{"desktop_source"}, []func(context.Context, uintptr) error{feed}, bind...)
}
func runPipelineSources(ctx context.Context, description string, consume func([]byte) error, names []string, feeds []func(context.Context, uintptr) error, bind ...encoderControls) error {
	return runPipelineBuffers(ctx, description, func(buf uintptr) error {
		var m mapInfo
		if buf == 0 || !api.mapBuf(buf, &m, 1) {
			return errors.New("cannot map encoded packet")
		}
		defer api.unmap(buf, &m)
		rawBytes := 0
		if len(bind) > 0 {
			rawBytes = bind[0].rawBytes
		}
		if m.Data == nil || !validCapturePacketSize(uint64(m.Size), rawBytes) {
			return errors.New("invalid encoded packet size")
		}
		return consume(append([]byte(nil), unsafe.Slice(m.Data, int(m.Size))...))
	}, names, feeds, bind...)
}
func runPipelineBuffers(ctx context.Context, description string, consume func(uintptr) error, names []string, feeds []func(context.Context, uintptr) error, bind ...encoderControls) error {
	api.once.Do(load)
	if api.err != nil {
		return api.err
	}
	var native *nativeError
	p := api.parse(description, &native)
	if native != nil {
		if p != 0 {
			api.unref(p)
		}
		return consumeError(native)
	}
	if p == 0 {
		return errors.New("empty GStreamer pipeline")
	}
	defer api.unref(p)
	sink := api.get(p, "encoded")
	if sink == 0 {
		return errors.New("missing encoded sink")
	}
	defer api.unref(sink)
	if len(bind) > 0 && (bind[0].bitrate != nil || bind[0].keyframe != nil) {
		encoder := api.get(p, "video_encoder")
		if encoder != 0 {
			defer api.unref(encoder)
			if bind[0].bitrate != nil {
				bind[0].bitrate(func(rate int) {
					if rate <= 0 {
						return
					}
					api.setArg(encoder, "bitrate", strconv.Itoa(rate))
					api.setArg(encoder, "cpb-size", strconv.Itoa(max(128, rate/4)))
				})
				defer bind[0].bitrate(nil)
			}
			if bind[0].keyframe != nil {
				bind[0].keyframe(func() bool {
					event := api.keyUnit(^uint64(0), true, 0)
					return event != 0 && api.sendEvent(encoder, event)
				})
				defer bind[0].keyframe(nil)
			}
		}
	}
	bus := api.bus(p)
	defer api.unref(bus)
	defer api.state(p, 1)
	if api.state(p, 4) == 0 {
		return errors.New("cannot start capture pipeline")
	}
	feedDone := make(chan error, len(feeds))
	feedCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); api.state(p, 1); wg.Wait() }()
	for i, feed := range feeds {
		if feed == nil {
			continue
		}
		source := api.get(p, names[i])
		if source == 0 {
			return errors.New("missing native appsrc")
		}
		wg.Add(1)
		go func(feed func(context.Context, uintptr) error, source uintptr) {
			defer wg.Done()
			defer api.unref(source)
			feedDone <- feed(feedCtx, source)
		}(feed, source)
	}
	for ctx.Err() == nil {
		if feedDone != nil {
			select {
			case e := <-feedDone:
				feedDone <- e
				return e
			default:
			}
		}
		msg := api.pop(bus, 0, 2)
		if msg != 0 {
			var e *nativeError
			var debug uintptr
			api.parseError(msg, &e, &debug)
			if debug != 0 {
				api.free(debug)
			}
			api.miniUnref(msg)
			return consumeError(e)
		}
		if eos := api.pop(bus, 0, 1); eos != 0 {
			api.miniUnref(eos)
			return errors.New("capture stream ended")
		}
		sample := api.pull(sink, 100000000)
		if sample == 0 {
			continue
		}
		err := func() error {
			defer api.miniUnref(sample)
			buf := api.buffer(sample)
			return consume(buf)
		}()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Audio captures the default output monitor, never the microphone.
func Audio(ctx context.Context, format, channels byte, rate uint32, test bool, send func([]byte) error) error {
	return AudioFrom(ctx, format, channels, rate, test, "", send)
}

// AudioFrom captures an explicitly selected sink when target is nonempty.
func AudioFrom(ctx context.Context, format, channels byte, rate uint32, test bool, target string, send func([]byte) error) error {
	formats := []string{"U8", "S8", "U16LE", "S16LE", "U32LE", "S32LE"}
	if format > 5 || channels < 1 || channels > 2 || rate < 8000 || rate > 192000 {
		return errors.New("invalid audio format")
	}
	source := `pipewiresrc client-name="Mirage desktop audio" stream-properties="props,stream.capture.sink=(boolean)true,media.type=(string)Audio,media.category=(string)Capture,media.role=(string)Music"`
	if target != "" {
		source += fmt.Sprintf(" target-object=%q", target)
	}
	if test {
		source = "audiotestsrc is-live=true wave=sine samplesperbuffer=480"
	}
	return runPipeline(ctx, fmt.Sprintf("%s ! audioconvert ! audioresample ! audio/x-raw,format=%s,channels=%d,rate=%d,layout=interleaved ! appsink name=encoded sync=false max-buffers=4 drop=true", source, formats[format], channels, rate), send)
}

// Raw RGBA frames are dimension-bounded, not compressed-packet-bounded.
func rawFrameBytes(o Options) int {
	if o.Codec != "raw" || o.Width < 1 || o.Width > 8192 || o.Height < 1 || o.Height > 8192 {
		return 0
	}
	return o.Width * o.Height * 4
}
func validCapturePacketSize(size uint64, rawBytes int) bool {
	if rawBytes > 0 {
		return rawBytes <= 8192*8192*4 && size == uint64(rawBytes)
	}
	return size > 0 && size <= 32<<20
}
