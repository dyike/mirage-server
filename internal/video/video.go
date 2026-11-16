// Package video decodes H.264 and HEVC access units with the platform's
// hardware decoder (VideoToolbox on Apple devices, MediaCodec on Android,
// Media Foundation/D3D11 on Windows, GStreamer hardware plugins on Linux
// and Intel Mac), for macOS Screen
// Sharing's high-performance mode and RDP's H.264 graphics.
package video

import (
	"errors"
	"image"

	"github.com/dyike/mirage-server/internal/frame"
)

// Codec is a video compression format.
type Codec int

const (
	H264 Codec = iota
	HEVC
)

func (c Codec) String() string {
	if c == HEVC {
		return "HEVC"
	}
	return "H.264"
}

// Frame is one decoded picture in BGRA. Pix holds its rows when the decoder
// gives CPU frames, and is only valid during the callback that receives it.
// Buffer and Surface let a renderer draw the picture without copying it.
// They identify a CVPixelBuffer/IOSurface on Apple devices or a registered
// DMA-BUF sample on Linux. RetainBuffer keeps them valid after the callback,
// until ReleaseBuffer; a GPU consumer also retains the producer until its
// work completes.
type Frame struct {
	W, H    int
	Stride  int
	Pix     []byte
	Buffer  uintptr
	Surface uintptr
	// SurfaceSize, when nonzero, is the backing texture size. W/H then
	// describe the visible top-left crop, e.g. an RDP image padded to a
	// 16-pixel macroblock boundary. Zero retains the usual W/H size.
	SurfaceSize image.Point
	// Timing follows the exact submitted access unit through decoding.
	Timing frame.Timing
}

// ErrUnsupported means this platform has no decoder.
var ErrUnsupported = errors.New("video: no hardware decoder on this platform")

// DecoderStatus distinguishes an observed software decoder from an unknown
// hardware state. Codec names or IOSurface output alone do not prove hardware use.
type DecoderStatus struct {
	Codec            string
	Backend          string
	Name             string
	HardwareKnown    bool
	Hardware         bool
	Error            string
	Conversion       string  `json:",omitempty"`
	OutputTimeMS     float64 `json:",omitempty"`
	ConversionTimeMS float64 `json:",omitempty"` // Android YUV conversion; excludes readback and decode
	ZeroCopy         bool    `json:",omitempty"`
	NativeFrames     uint64  `json:",omitempty"`
	ReadbackFrames   uint64  `json:",omitempty"`
}

// NALType returns the type of a NAL unit (without start code).
func NALType(c Codec, nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	if c == HEVC {
		return int(nal[0]>>1) & 0x3f
	}
	return int(nal[0]) & 0x1f
}

// IsParameterSet reports whether nal is an SPS, PPS or (HEVC) VPS.
func IsParameterSet(c Codec, nal []byte) bool {
	t := NALType(c, nal)
	if c == HEVC {
		return t >= 32 && t <= 34 // VPS, SPS, PPS
	}
	return t == 7 || t == 8
}

// SplitAnnexB splits a byte stream at 3- and 4-byte start codes.
func SplitAnnexB(b []byte) [][]byte {
	return AppendAnnexB(nil, b)
}

// AppendAnnexB appends borrowed NAL slices to reusable descriptor storage.
// The descriptors and input bytes must remain valid until Decode returns.
func AppendAnnexB(nals [][]byte, b []byte) [][]byte {
	nals, _ = appendAnnexB(nals, b, 0)
	return nals
}

var ErrNALCount = errors.New("video: access unit contains too many NAL units")

// AppendAnnexBBounded limits descriptor growth for untrusted access units.
// On error, the returned descriptors still borrow b and must be cleared by
// callers that retain their backing storage.
func AppendAnnexBBounded(nals [][]byte, b []byte, limit int) ([][]byte, error) {
	if limit <= 0 || len(nals) > limit {
		return nals, ErrNALCount
	}
	return appendAnnexB(nals, b, limit)
}

func appendAnnexB(nals [][]byte, b []byte, limit int) ([][]byte, error) {
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				if limit > 0 && len(nals) == limit {
					return nals, ErrNALCount
				}
				end := i
				if end > start && b[end-1] == 0 {
					end--
				}
				nals = append(nals, b[start:end])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		if limit > 0 && len(nals) == limit {
			return nals, ErrNALCount
		}
		nals = append(nals, b[start:])
	}
	return nals, nil
}
