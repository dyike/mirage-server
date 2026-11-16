package rfb

import (
	"time"

	"github.com/dyike/mirage-server/internal/netstats"

	"github.com/dyike/mirage-server/internal/video"
)

// Encoding reports the last successfully decoded picture encoding. CopyRect
// and cursor/resize messages do not replace the primary picture encoding.
func (c *Client) Encoding() string {
	switch c.encoding.Load() {
	case int64(EncRaw) + 1:
		return "Raw"
	case int64(EncMirageHEVC) + 1:
		return "HEVC"
	case int64(EncH264) + 1:
		return "H.264"
	case int64(EncTight) + 1:
		return "Tight"
	case int64(EncZRLE) + 1:
		return "ZRLE"
	case int64(EncAppleRGB555) + 1:
		return "Apple RGB555"
	}
	return ""
}

func (c *Client) NetworkRTT() (time.Duration, bool) { return netstats.RTT(c.conn) }

// VideoStatus reports the last ordinary VNC H.264 decoder observation.
func (c *Client) VideoStatus() video.DecoderStatus {
	if status := c.h264Status.Load(); status != nil {
		return *status
	}
	return video.DecoderStatus{}
}

// VideoFrames counts decoded access units before display coalescing.
func (c *Client) VideoFrames() (int64, bool) {
	if frames := c.h264Frames.Load(); frames > 0 {
		return frames, true
	}
	m := c.media.Load()
	if m == nil || m.decoder.Load() == nil {
		return 0, false
	}
	return m.submitted.Load(), true
}

// H264Requested reports intent independently of a later decoder fallback.
func (c *Client) H264Requested() bool { return c.h264Requested }
