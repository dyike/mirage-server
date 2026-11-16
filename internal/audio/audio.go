// Package audio plays audio from a remote session through the platform's
// audio stack: AAC-ELD from macOS's high-performance mode and PCM from RDP,
// via AudioQueue on Apple devices and Oto for PCM on Windows, Linux and
// Android. Apple AudioQueue decodes AAC itself; Windows/Linux use a bundled
// native FDK AAC decoder through purego, and Android uses system MediaCodec.
package audio

import "errors"

// Format describes a stream of compressed packets.
type Format struct {
	SampleRate      float64
	Channels        int
	FramesPerPacket int
	// Cookie is the decoder configuration (an ES descriptor for AAC).
	Cookie []byte
}

// ErrUnsupported means this platform cannot play the stream.
var ErrUnsupported = errors.New("audio: no player on this platform")

// PlaybackStatus contains queue counters, never audio samples or packet payloads.
type PlaybackStatus struct {
	Underruns                             int64
	Volume                                float32
	VolumeKnown                           bool
	Decoder                               string
	Enqueued, Completed, Dropped          int64
	StartRequested, RunningKnown, Running bool
	Error                                 string
}

// OutputStatus is a read-only snapshot of the platform audio session.
type OutputStatus struct {
	Available             bool
	Category, Mode, Route string
	Volume                float32
	VolumeKnown           bool
	Error                 string
}

// AACELDCookie wraps an AAC AudioSpecificConfig in the ES descriptor
// AudioToolbox takes as the magic cookie: ES_Descriptor (tag 3) holding a
// DecoderConfigDescriptor (tag 4: MPEG-4 audio, audio stream, buffer size
// 6144) holding the DecoderSpecificInfo (tag 5).
func AACELDCookie(asc []byte) []byte {
	dsi := append([]byte{0x05, byte(len(asc))}, asc...)
	dcd := append([]byte{0x04, byte(13 + len(dsi)), 0x40, 0x15, 0x00, 0x18, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}, dsi...)
	return append([]byte{0x03, byte(3 + len(dcd)), 0x00, 0x01, 0x00}, dcd...)
}

// RemoteDesktopELD is the AudioSpecificConfig macOS's system audio stream
// uses: AAC-ELD, 48 kHz, stereo, 480-sample frames.
var RemoteDesktopELD = []byte{0xf8, 0xe6, 0x50, 0x00}
