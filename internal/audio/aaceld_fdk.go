//go:build (windows || linux) && !android && (amd64 || arm64)

package audio

import "github.com/dyike/mirage-server/internal/audio/internal/eldcodec"

func NewAACELD(f Format) (*Player, error) {
	if err := validateELD(f); err != nil {
		return nil, err
	}
	decoder, err := eldcodec.New(RemoteDesktopELD)
	if err != nil {
		return nil, err
	}
	p, err := NewPCM(48000, 2)
	if err != nil {
		decoder.Close()
		return nil, err
	}
	p.decoder = decoder
	p.decoderName = "FDK AAC"
	return p, nil
}
