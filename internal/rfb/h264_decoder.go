package rfb

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dyike/mirage-server/internal/video"
)

//go:embed testdata/h264-probe.h264
var h264Probe []byte

type h264Decoder interface {
	Decode([][]byte) error
	SetCPUFrames(bool)
	Status() video.DecoderStatus
	Close()
}
type h264Factory func(func(video.Frame)) (h264Decoder, error)

func platformH264(cb func(video.Frame)) (h264Decoder, error) { return video.NewDecoder(video.H264, cb) }

type h264Result struct {
	frame video.Frame
	err   error
}
type orderedH264 struct {
	decoder         h264Decoder
	mu              sync.Mutex
	waiting, failed bool
	ready           chan h264Result
	used            uint64
	// CPU output belongs to this sequential decoder until the next decode.
	// Copy out of the platform's borrowed buffer without allocating a 4K
	// image on every frame. Native surface frames retain their own ownership.
	pixels []byte
}

func newOrderedH264(factory h264Factory) (*orderedH264, error) {
	d := &orderedH264{ready: make(chan h264Result, 1)}
	inner, err := factory(d.output)
	if err != nil {
		return nil, err
	}
	d.decoder = inner
	inner.SetCPUFrames(false)
	return d, nil
}
func (d *orderedH264) output(f video.Frame) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.waiting || d.failed {
		return
	}
	d.waiting = false
	result := h264Result{frame: f}
	if f.W <= 0 || f.H <= 0 || f.W > 16384 || f.H > 16384 || uint64(f.W)*uint64(f.H) > 64<<20 {
		result.err = errors.New("rfb: invalid H.264 picture dimensions")
	} else if f.Buffer != 0 && f.Surface != 0 {
		video.RetainBuffer(f.Buffer)
		result.frame.Pix = nil
	} else if f.Stride < f.W*4 || uint64(f.Stride)*uint64(f.H) > uint64(len(f.Pix)) {
		result.err = errors.New("rfb: invalid H.264 pixel buffer")
	} else {
		result.frame.Buffer = 0
		result.frame.Surface = 0
		size := f.Stride * f.H
		if cap(d.pixels) < size {
			d.pixels = make([]byte, size)
		}
		d.pixels = d.pixels[:size]
		copy(d.pixels, f.Pix[:size])
		result.frame.Pix = d.pixels
	}
	if result.err != nil {
		result.frame = video.Frame{}
	}
	d.ready <- result
}
func (d *orderedH264) decode(ctx context.Context, nals [][]byte) (video.Frame, error) {
	d.mu.Lock()
	if d.failed {
		d.mu.Unlock()
		return video.Frame{}, errors.New("rfb: H.264 decoder failed")
	}
	d.waiting = true
	d.mu.Unlock()
	fail := func(err error) (video.Frame, error) {
		d.mu.Lock()
		d.failed = true
		d.waiting = false
		d.mu.Unlock()
		select {
		case r := <-d.ready:
			releaseH264Frame(r.frame)
		default:
		}
		return video.Frame{}, err
	}
	if err := d.decoder.Decode(nals); err != nil {
		return fail(err)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case r := <-d.ready:
		if r.err != nil {
			return fail(r.err)
		}
		return r.frame, nil
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-timer.C:
		return fail(errors.New("rfb: timed out waiting for H.264 hardware output"))
	}
}
func (d *orderedH264) close() {
	d.mu.Lock()
	d.failed = true
	d.waiting = false
	d.mu.Unlock()
	d.decoder.Close()
	select {
	case r := <-d.ready:
		releaseH264Frame(r.frame)
	default:
	}
}
func releaseH264Frame(f video.Frame) {
	if f.Buffer != 0 {
		video.ReleaseBuffer(f.Buffer)
	}
}

// Split complete baseline access units, using AUDs when present and the first
// slice of a picture otherwise. Multi-slice pictures stay in one Decode call.
func h264AccessUnits(data []byte) ([][][]byte, error) {
	nals, err := video.AppendAnnexBBounded(nil, data, 8192)
	if err != nil {
		return nil, err
	}
	if len(nals) == 0 {
		return nil, errors.New("rfb: H.264 rectangle is not Annex B")
	}
	var units [][][]byte
	start := 0
	hasPicture := false
	for i, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		kind := nal[0] & 31
		if kind >= 2 && kind <= 4 {
			return nil, errors.New("rfb: H.264 data-partitioned slices unsupported")
		}
		first := false
		if kind == 1 || kind == 5 {
			if len(nal) < 2 {
				return nil, errors.New("rfb: truncated H.264 slice")
			}
			first = nal[1]&128 != 0 /* first_mb_in_slice == ue(0) */
		}
		boundary := kind == 9 || kind == 7 || kind == 8 || kind == 6 || ((kind == 1 || kind == 5) && first)
		if hasPicture && boundary {
			units = append(units, nals[start:i])
			start = i
			hasPicture = false
			if len(units) >= 256 {
				return nil, errors.New("rfb: too many H.264 frames in rectangle")
			}
		}
		if kind == 1 || kind == 5 {
			hasPicture = true
		}
	}
	if hasPicture {
		units = append(units, nals[start:])
	}
	return units, nil
}
func (c *Client) prepareH264() {
	found := false
	for _, enc := range c.cfg.Encodings {
		found = found || enc == EncH264
	}
	if !found {
		return
	}
	c.h264Requested = true
	factory := c.h264Factory
	if factory == nil {
		factory = platformH264
	}
	err := func() error {
		d, err := newOrderedH264(factory)
		if err != nil {
			return err
		}
		defer d.close()
		units, err := h264AccessUnits(h264Probe)
		if err != nil {
			return err
		}
		for _, au := range units {
			f, err := d.decode(c.authContext, au)
			if err != nil {
				return err
			}
			releaseH264Frame(f)
		}
		status := d.decoder.Status()
		c.h264Status.Store(&status)
		if !status.HardwareKnown || !status.Hardware {
			return errors.New("rfb: H.264 hardware decoding was not confirmed")
		}
		return nil
	}()
	if err != nil {
		status := video.DecoderStatus{Codec: "H.264", Error: fmt.Sprintf("H.264 capability probe: %v", err)}
		c.h264Status.Store(&status)
		c.cfg.Encodings = withoutH264(c.cfg.Encodings)
		c.h264Disabled = true
	}
}
func withoutH264(encodings []int32) []int32 {
	out := make([]int32, 0, len(encodings))
	for _, enc := range encodings {
		if enc != EncH264 && enc != EncMirageHEVC {
			out = append(out, enc)
		}
	}
	return out
}
