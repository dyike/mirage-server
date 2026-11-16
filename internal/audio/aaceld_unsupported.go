//go:build (windows || linux) && !android && !(amd64 || arm64)

package audio

func NewAACELD(Format) (*Player, error) { return nil, ErrUnsupported }
