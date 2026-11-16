package rfb

import (
	"image"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/pion/rtp"

	"github.com/dyike/mirage-server/internal/video"
)

// The video leg carries HEVC (payload type 100) or H.264 (123) in the
// standard RTP payload formats: RFC 7798 single NAL units, aggregation
// packets and fragmentation units for HEVC, RFC 6184 for H.264. The marker
// bit ends an access unit.

type auAssembler struct {
	codec   video.Codec
	nals    [][]byte
	frag    []byte // NAL being reassembled from fragments
	lastSeq uint16
	started bool
	broken  bool // a packet of this access unit was lost
	synced  bool // a keyframe has been decoded since the last loss
}

// push adds one RTP payload; it returns a complete access unit at the
// marker bit, or nil. A sequence gap drops the access unit in progress.
func (a *auAssembler) push(h *rtp.Header, p []byte) (au [][]byte, lost bool) {
	if a.started && h.SequenceNumber != a.lastSeq+1 {
		a.broken, a.frag, lost = true, nil, true
	}
	a.started, a.lastSeq = true, h.SequenceNumber
	if a.codec == video.HEVC {
		a.hevc(p)
	} else {
		a.h264(p)
	}
	if !h.Marker {
		return nil, lost
	}
	au, broken := a.nals, a.broken
	a.nals, a.frag, a.broken = nil, nil, false
	if broken {
		return nil, true
	}
	return au, lost
}

func (a *auAssembler) hevc(p []byte) {
	if len(p) < 3 {
		return
	}
	switch t := (p[0] >> 1) & 0x3f; t {
	case 48: // aggregation packet: (u16 size, NAL)*
		for q := p[2:]; len(q) >= 2; {
			n := int(be.Uint16(q))
			if 2+n > len(q) {
				a.broken = true
				return
			}
			a.nals = append(a.nals, append([]byte(nil), q[2:2+n]...))
			q = q[2+n:]
		}
	case 49: // fragmentation unit
		fu := p[2]
		if fu&0x80 != 0 { // start: rebuild the NAL header with the real type
			a.frag = append(a.frag[:0], p[0]&0x81|(fu&0x3f)<<1, p[1])
		} else if a.frag == nil {
			a.broken = true
			return
		}
		a.frag = append(a.frag, p[3:]...)
		if fu&0x40 != 0 { // end
			a.nals = append(a.nals, a.frag)
			a.frag = nil
		}
	default: // single NAL unit
		a.nals = append(a.nals, append([]byte(nil), p...))
	}
}

func (a *auAssembler) h264(p []byte) {
	if len(p) < 2 {
		return
	}
	switch t := p[0] & 0x1f; t {
	case 24: // STAP-A
		for q := p[1:]; len(q) >= 2; {
			n := int(be.Uint16(q))
			if 2+n > len(q) {
				a.broken = true
				return
			}
			a.nals = append(a.nals, append([]byte(nil), q[2:2+n]...))
			q = q[2+n:]
		}
	case 28: // FU-A
		fu := p[1]
		if fu&0x80 != 0 {
			a.frag = append(a.frag[:0], p[0]&0xe0|fu&0x1f)
		} else if a.frag == nil {
			a.broken = true
			return
		}
		a.frag = append(a.frag, p[2:]...)
		if fu&0x40 != 0 {
			a.nals = append(a.nals, a.frag)
			a.frag = nil
		}
	default:
		a.nals = append(a.nals, append([]byte(nil), p...))
	}
}

// isKeyframe reports whether an access unit can start decoding.
func isKeyframe(c video.Codec, au [][]byte) bool {
	for _, n := range au {
		t := video.NALType(c, n)
		if c == video.HEVC && t >= 16 && t <= 21 || c == video.H264 && t == 5 {
			return true
		}
	}
	return false
}

// showFrame copies a decoded BGRA picture into the framebuffer and reports
// it. It runs on the decoder's thread.
func (c *Client) showFrame(f video.Frame) {
	c.pictureFrames.Add(1)
	c.pictureWatch.frame(time.Now())
	c.pictureMu.Lock()
	defer c.pictureMu.Unlock()
	if c.cfg.OnVideoFrame != nil && f.Surface != 0 {
		// The viewer draws the decoder's surface: only the bookkeeping that
		// sizes and pointer mapping depend on.
		resized := c.size.Load() != uint64(f.W)<<32|uint64(f.H)
		if resized {
			c.size.Store(uint64(f.W)<<32 | uint64(f.H))
			c.cov.reset(f.W, f.H)
		}
		c.cov.mark(image.Rect(0, 0, f.W, f.H))
		c.noteCoverage(resized)
		if resized && c.cfg.OnResize != nil {
			c.cfg.OnResize(f.W, f.H)
		}
		c.cfg.OnVideoFrame(f)
		return
	}
	c.lockFB()
	resized := f.W != c.fb.W || f.H != c.fb.H
	if resized {
		c.fb.Resize(f.W, f.H)
		c.size.Store(uint64(f.W)<<32 | uint64(f.H))
	}
	bgraToRGBA(c.pix32(), f)
	c.wrote(c.fb.Bounds())
	c.unlockFB()
	if resized {
		c.cov.reset(f.W, f.H)
	}
	c.cov.mark(c.fb.Bounds()) // no need to ask for RFB pictures
	c.noteCoverage(resized)
	if resized && c.cfg.OnResize != nil {
		c.cfg.OnResize(f.W, f.H)
	}
	if c.cfg.OnUpdate != nil {
		c.fb.Timing = f.Timing
		c.cfg.OnUpdate(&c.fb, []image.Rectangle{c.fb.Bounds()})
	}
}

// bgraToRGBA swaps red and blue into dst (stride f.W), across cores.
func bgraToRGBA(dst []uint32, f video.Frame) {
	rows := func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			src := unsafe.Slice((*uint32)(unsafe.Pointer(&f.Pix[y*f.Stride])), f.W)
			out := dst[y*f.W : (y+1)*f.W]
			for x, v := range src {
				out[x] = v&0xff00ff00 | v>>16&0xff | v&0xff<<16
			}
		}
	}
	n := min(runtime.GOMAXPROCS(0), max(1, f.H/128))
	var wg sync.WaitGroup
	for i := range n {
		y0, y1 := f.H*i/n, f.H*(i+1)/n
		wg.Go(func() { rows(y0, y1) })
	}
	wg.Wait()
}
