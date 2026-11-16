package rfb

import (
	"io"
	"sync/atomic"
)

// QEMU Audio is negotiated explicitly; ordinary servers ignore this encoding.
const EncPseudoQEMUAudio int32 = -259
const audioRate = 48000
const audioChannels = 2

type AudioStatus struct {
	Supported, Active bool
	ReceivedBytes     int64
}

type audioState struct {
	supported, active atomic.Bool
	received          atomic.Int64
}

func (c *Client) AudioStatus() AudioStatus {
	return AudioStatus{c.audio.supported.Load(), c.audio.active.Load(), c.audio.received.Load()}
}

func (c *Client) enableAudio() error {
	if c.cfg.OnAudio == nil || c.audio.supported.Swap(true) {
		return nil
	}
	// QEMU's PCM stream is little endian. Select signed 16-bit stereo before
	// enabling capture; never send extension messages before server confirmation.
	msg := []byte{255, 1, 0, 2, 3, audioChannels, 0, 0, 0, 0, 255, 1, 0, 0}
	be.PutUint32(msg[6:10], audioRate)
	return c.send(msg)
}

func (c *Client) readAudio() error {
	var h [3]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return err
	}
	if h[0] != 1 || !c.audio.supported.Load() {
		return protoErr("unnegotiated QEMU audio message")
	}
	switch be.Uint16(h[1:]) {
	case 0:
		c.audio.active.Store(false)
	case 1:
		c.audio.active.Store(true)
	case 2:
		var size [4]byte
		if _, err := io.ReadFull(c.r, size[:]); err != nil {
			return err
		}
		n := be.Uint32(size[:])
		if n > 256<<10 || n%4 != 0 {
			return protoErr("invalid QEMU audio buffer size %d", n)
		}
		data := make([]byte, int(n))
		if _, err := io.ReadFull(c.r, data); err != nil {
			return err
		}
		c.audio.received.Add(int64(n))
		if c.audio.active.Load() && c.cfg.OnAudio != nil {
			c.cfg.OnAudio(audioRate, audioChannels, data)
		}
	default:
		return protoErr("unknown QEMU audio operation %d", be.Uint16(h[1:]))
	}
	return nil
}
