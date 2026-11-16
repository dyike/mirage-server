//go:build !(darwin && arm64) && !(darwin && !ios && amd64) && !(android && cgo) && !(windows && (amd64 || arm64)) && !(linux && !android && (amd64 || arm64))

package video

import (
	"errors"
	"image"
)

// Decoder is unavailable here; NewDecoder always fails.
type Decoder struct{}

// ErrNoParameterSets means a picture came before the stream's SPS/PPS.
var ErrNoParameterSets = errors.New("video: no parameter sets yet")

func NewDecoder(Codec, func(Frame)) (*Decoder, error) { return nil, ErrUnsupported }
func (*Decoder) Decode([][]byte) error                { return ErrUnsupported }
func (*Decoder) Close()                               {}
func (*Decoder) Status() DecoderStatus                { return DecoderStatus{Error: ErrUnsupported.Error()} }

func (*Decoder) SetCPUFrames(bool)                  {}
func RetainBuffer(uintptr)                          {}
func ReleaseBuffer(uintptr)                         {}
func Snapshot(uintptr) *image.RGBA                  { return nil }
func CopyBGRA(uintptr, []byte, int, int, int) error { return ErrUnsupported }
