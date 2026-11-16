//go:build windows || linux || android

package audio

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
)

// Oto owns one process-wide output context. Session players are stopped and
// discarded independently; reconnects and format switches reuse the device.
var pcmContext struct {
	mu      sync.Mutex
	once    sync.Once
	context *oto.Context
	ready   chan struct{}
	err     error
}

type Player = pcmPlayer

func NewPCM(rate, channels int) (*Player, error) {
	return newPCMPlayer(rate, channels, func(source io.Reader) (pcmDevice, error) {
		pcmContext.once.Do(func() {
			pcmContext.mu.Lock()
			defer pcmContext.mu.Unlock()
			pcmContext.context, pcmContext.ready, pcmContext.err = oto.NewContext(&oto.NewContextOptions{
				SampleRate: pcmOutputRate, ChannelCount: 2, Format: oto.FormatSignedInt16LE,
				BufferSize: 40 * time.Millisecond, ApplicationName: "Mirage",
			})
		})
		if pcmContext.err != nil {
			return nil, fmt.Errorf("audio: open output: %w", pcmContext.err)
		}
		if err := pcmContext.context.Err(); err != nil {
			return nil, fmt.Errorf("audio: output device: %w", err)
		}
		player := pcmContext.context.NewPlayer(source)
		player.SetBufferSize(pcmBlockBytes)
		return &otoPCMDevice{Player: player, context: pcmContext.context, ready: pcmContext.ready}, nil
	})
}

type otoPCMDevice struct {
	*oto.Player
	context *oto.Context
	ready   <-chan struct{}
}

func (d *otoPCMDevice) IsPlaying() bool {
	select {
	case <-d.ready:
		return d.Player.IsPlaying()
	default:
		return false
	}
}

func (d *otoPCMDevice) Err() error {
	if err := d.context.Err(); err != nil {
		return fmt.Errorf("audio: output device: %w", err)
	}
	return d.Player.Err()
}
