package rfb

import (
	"context"
	"fmt"
	"image"
	"io"
	"log"
	"time"

	"github.com/dyike/mirage-server/internal/inputtrace"
	"github.com/dyike/mirage-server/internal/video"
)

func (c *Client) closeH264Contexts() {
	for r, d := range c.h264Contexts {
		d.close()
		delete(c.h264Contexts, r)
	}
}
func (c *Client) clearH264Picture() {
	releaseH264Frame(c.h264Picture)
	c.h264Picture = video.Frame{}
	c.h264PictureDirty = false
}
func (c *Client) h264Context(r image.Rectangle) (*orderedH264, error) {
	if d := c.h264Contexts[r]; d != nil {
		c.h264Clock++
		d.used = c.h264Clock
		return d, nil
	}
	if len(c.h264Contexts) >= 64 {
		var oldest image.Rectangle
		var age uint64 = ^uint64(0)
		for rect, d := range c.h264Contexts {
			if d.used < age {
				oldest, age = rect, d.used
			}
		}
		c.h264Contexts[oldest].close()
		delete(c.h264Contexts, oldest)
	}
	factory := c.h264Factory
	if factory == nil {
		factory = platformH264
		if c.hevcActive {
			factory = platformHEVC
		}
	}
	d, err := newOrderedH264(factory)
	if err != nil {
		return nil, err
	}
	if c.h264Contexts == nil {
		c.h264Contexts = make(map[image.Rectangle]*orderedH264)
	}
	c.h264Clock++
	d.used = c.h264Clock
	c.h264Contexts[r] = d
	return d, nil
}
func (c *Client) composeH264(r image.Rectangle, f video.Frame) error {
	if f.W < r.Dx() || f.H < r.Dy() {
		return fmt.Errorf("rfb: H.264 picture %dx%d smaller than rectangle %v", f.W, f.H, r)
	}
	pixels, stride := f.Pix, f.Stride
	if f.Buffer != 0 {
		stride = r.Dx() * 4
		pixels = make([]byte, stride*r.Dy())
		if err := video.CopyBGRA(f.Buffer, pixels, stride, r.Dx(), r.Dy()); err != nil {
			return err
		}
	}
	if stride < r.Dx()*4 || uint64(stride)*uint64(r.Dy()) > uint64(len(pixels)) {
		return fmt.Errorf("rfb: short H.264 pixel buffer")
	}
	c.lockFB()
	defer c.unlockFB()
	for y := 0; y < r.Dy(); y++ {
		src := pixels[y*stride:]
		dst := c.fb.Pix[((r.Min.Y+y)*c.fb.W+r.Min.X)*4:]
		for x := 0; x < r.Dx(); x++ {
			i := x * 4
			dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], 255
		}
	}
	c.wrote(r)
	return nil
}

// Materialize the last full-screen native image only when a subsequent CPU
// rectangle needs its unchanged pixels (including CopyRect's source).
func (c *Client) materializeH264() error {
	if c.h264Picture.Buffer == 0 {
		return nil
	}
	if err := c.composeH264(c.fb.Bounds(), c.h264Picture); err != nil {
		return err
	}
	c.clearH264Picture()
	c.dirty = append(c.dirty, c.fb.Bounds())
	return nil
}
func (c *Client) h264Fallback(reason error) error {
	codec := "H.264"
	if c.hevcActive {
		codec = "HEVC"
	}
	log.Printf("VNC %s decode failed: %v", codec, reason)
	inputtrace.Video("h264-fallback", struct {
		Error   string
		Frames  int64
		Status  video.DecoderStatus
		Desktop *DesktopLayout
	}{reason.Error(), c.h264Frames.Load(), c.VideoStatus(), c.Desktop()})
	c.closeH264Contexts()
	clear(c.videoRecovery)
	if c.hevcActive {
		c.hevcDisabled = true
		encodings := make([]int32, 0, len(c.cfg.Encodings))
		h264 := false
		for _, enc := range c.cfg.Encodings {
			if enc == EncMirageHEVC {
				continue
			}
			encodings = append(encodings, enc)
			h264 = h264 || enc == EncH264
		}
		c.cfg.Encodings = encodings
		c.h264Disabled = !h264
	} else {
		c.h264Disabled = true
		c.cfg.Encodings = withoutH264(c.cfg.Encodings)
	}
	status := video.DecoderStatus{Codec: codec, Error: reason.Error()}
	c.h264Status.Store(&status)
	if err := c.setEncodings(c.cfg.Encodings); err != nil {
		return err
	}
	return c.requestUpdate(false)
}

// A running hardware decoder can fail transiently (for example when iOS
// takes its resources away). Recover from a new independently decodable picture
// before permanently withdrawing the codec. Never feed dependent frames into a
// newly created decoder, and bound repeated recovery failures.
type videoRecovery struct {
	awaiting       bool
	started        time.Time
	attempts, good int
}

func (c *Client) recoverVideo(r image.Rectangle, reason error) error {
	if c.h264Frames.Load() == 0 {
		return c.h264Fallback(reason)
	}
	if c.videoRecovery == nil {
		c.videoRecovery = make(map[image.Rectangle]*videoRecovery)
	}
	state := c.videoRecovery[r]
	if state == nil {
		state = &videoRecovery{}
		c.videoRecovery[r] = state
	}
	if state.attempts >= 2 {
		return c.h264Fallback(reason)
	}
	state.attempts++
	state.good = 0
	state.awaiting, state.started = true, time.Now()
	if d := c.h264Contexts[r]; d != nil {
		d.close()
		delete(c.h264Contexts, r)
	}
	codec := "H.264"
	if c.hevcActive {
		codec = "HEVC"
	}
	status := video.DecoderStatus{Codec: codec, Error: "Recovering hardware decoder: " + reason.Error()}
	c.h264Status.Store(&status)
	inputtrace.Video("video-recovery", status)
	log.Printf("VNC %s restarting decoder for %v (attempt %d): %v", codec, r, state.attempts, reason)
	return c.requestUpdate(false)
}

func videoRestartPicture(units [][][]byte, hevc bool) bool {
	var sets uint8
	picture := false
	for _, unit := range units {
		for _, nal := range unit {
			if len(nal) == 0 {
				continue
			}
			if hevc {
				kind := (nal[0] >> 1) & 63
				if kind >= 32 && kind <= 34 {
					sets |= 1 << (kind - 32)
				}
				picture = picture || kind >= 16 && kind <= 21
			} else {
				kind := nal[0] & 31
				if kind == 7 {
					sets |= 1
				}
				if kind == 8 {
					sets |= 2
				}
				picture = picture || kind == 5
			}
		}
	}
	if hevc {
		return picture && sets == 7
	}
	return picture && sets == 3
}

func (c *Client) decodeH264(r image.Rectangle) (bool, error) {
	var header [8]byte
	if _, err := io.ReadFull(c.r, header[:]); err != nil {
		return false, err
	}
	length, flags := be.Uint32(header[:]), be.Uint32(header[4:])
	if length > 32<<20 {
		return false, protoErr("H.264 rectangle too large: %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return false, err
	}
	if flags&2 != 0 {
		c.closeH264Contexts()
	}
	if flags&1 != 0 {
		if d := c.h264Contexts[r]; d != nil {
			d.close()
			delete(c.h264Contexts, r)
		}
	}
	if length == 0 || c.h264Disabled || c.hevcActive && c.hevcDisabled {
		return false, nil
	}
	if r.Empty() {
		return false, protoErr("H.264 picture has empty rectangle")
	}
	split := h264AccessUnits
	if c.hevcActive {
		split = hevcAccessUnits
	}
	units, err := split(payload)
	if err != nil {
		return false, c.h264Fallback(fmt.Errorf("rectangle %v: %w", r, err))
	}
	if len(units) == 0 {
		return false, c.h264Fallback(fmt.Errorf("rfb: H.264 packet contains no complete picture"))
	}
	if state := c.videoRecovery[r]; state != nil && state.awaiting {
		if !videoRestartPicture(units, c.hevcActive) {
			if time.Since(state.started) > 5*time.Second {
				return false, c.h264Fallback(fmt.Errorf("%s recovery: no independent picture within 5 seconds", c.VideoStatus().Codec))
			}
			return false, nil
		}
		state.awaiting = false
	}
	d, err := c.h264Context(r)
	if err != nil {
		return false, c.h264Fallback(fmt.Errorf("rectangle %v: %w", r, err))
	}
	ctx := c.decodeContext
	if ctx == nil {
		ctx = context.Background()
	}
	var last video.Frame
	for _, unit := range units {
		decodeStart := time.Now()
		f, err := d.decode(ctx, unit)
		c.feedback.decode += time.Since(decodeStart)
		c.feedback.decoded++
		releaseH264Frame(last)
		last = video.Frame{}
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, c.recoverVideo(r, fmt.Errorf("rectangle %v: %w", r, err))
		}
		last = f
	}
	defer func() { releaseH264Frame(last) }()
	status := d.decoder.Status()
	if !status.HardwareKnown || !status.Hardware {
		return false, c.h264Fallback(fmt.Errorf("rfb: H.264 decoder did not confirm hardware use"))
	}
	if last.W < r.Dx() || last.H < r.Dy() {
		return false, c.h264Fallback(fmt.Errorf("rfb: undersized H.264 picture"))
	}
	if state := c.videoRecovery[r]; state != nil {
		state.good += len(units)
		if state.good >= 120 {
			delete(c.videoRecovery, r)
		}
	}
	c.h264Frames.Add(int64(len(units)))
	if r == c.fb.Bounds() && last.Buffer != 0 && last.Surface != 0 && c.cfg.OnVideoFrame != nil {
		c.clearH264Picture()
		last.SurfaceSize = image.Pt(last.W, last.H)
		last.W, last.H = r.Dx(), r.Dy()
		c.h264Picture = last
		last = video.Frame{}
		c.h264PictureDirty = true
	} else {
		if err := c.materializeH264(); err != nil {
			return false, err
		}
		if err := c.composeH264(r, last); err != nil {
			return false, c.h264Fallback(fmt.Errorf("rectangle %v: %w", r, err))
		}
		status.ZeroCopy = false
		status.Conversion = "BGRA rectangle composition"
	}
	c.h264Status.Store(&status)
	if c.h264Frames.Load() < 5 || c.h264Frames.Load()%120 == 0 {
		inputtrace.Video("h264-active", struct {
			Frames  int64
			Status  video.DecoderStatus
			Desktop *DesktopLayout
		}{c.h264Frames.Load(), status, c.Desktop()})
	}
	return true, nil
}
