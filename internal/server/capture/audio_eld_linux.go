package capture

import (
	"context"
	"github.com/dyike/mirage-server/internal/audio"
)

func AudioELDAvailable() error {
	e, err := audio.NewELDEncoder()
	if err != nil {
		return err
	}
	e.Close()
	return nil
}

// AudioELD captures the default output monitor, never a microphone.
func AudioELD(ctx context.Context, test bool, send func([]byte) error) error {
	e, err := audio.NewELDEncoder()
	if err != nil {
		return err
	}
	defer e.Close()
	pending := make([]byte, 0, 1920)
	return Audio(ctx, 3, 2, 48000, test, func(data []byte) error {
		for len(data) > 0 {
			n := min(1920-len(pending), len(data))
			pending = append(pending, data[:n]...)
			data = data[n:]
			if len(pending) == 1920 {
				packet, err := e.Encode(pending)
				pending = pending[:0]
				if err != nil {
					return err
				}
				if len(packet) > 0 {
					if err := send(packet); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
