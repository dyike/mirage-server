package rfb

import (
	"encoding/json"
	"log"

	"github.com/dyike/mirage-server/internal/audio"
	"github.com/dyike/mirage-server/internal/video"
)

// MediaDiagnostics describes transport/decoder state, never media payloads,
// authentication material, account names, or personally named output devices.
type MediaDiagnostics struct {
	State string
	// Fallback says why the client went back to RFB pictures; empty
	// while the stream runs or was never tried.
	Fallback string
	// MaxBitrate is the video bit rate the offer allows, in bit/s; 0 for
	// the server's own choice (on a local network).
	MaxBitrate                                   uint64
	Video                                        video.DecoderStatus
	VideoUnits, VideoPackets, VideoBad           int64
	VideoError                                   string
	AudioEnabled                                 bool
	AudioPort                                    uint16
	AudioState                                   string
	AudioPackets, AudioBytes, AudioUDP, AudioBad int64
	AudioError                                   string
	Playback                                     audio.PlaybackStatus
	Output                                       audio.OutputStatus
}

func (c *Client) updateMediaDiagnostics(change func(*MediaDiagnostics)) {
	c.mediaDiagnosticsMu.Lock()
	change(&c.mediaDiagnostics)
	c.mediaDiagnosticsMu.Unlock()
}

func (c *Client) MediaDiagnostics() MediaDiagnostics {
	c.mediaDiagnosticsMu.Lock()
	defer c.mediaDiagnosticsMu.Unlock()
	return c.mediaDiagnostics
}

func (c *Client) publishMediaDiagnostics(writeLog bool) {
	d := c.MediaDiagnostics()
	if cb := c.cfg.OnMediaDiagnostics; cb != nil {
		cb(d)
	}
	if c.cfg.Diagnostics && writeLog {
		if data, err := json.Marshal(d); err == nil {
			log.Printf("MIRAGE_MEDIA_DIAGNOSTICS %s", data)
		}
	}
}
