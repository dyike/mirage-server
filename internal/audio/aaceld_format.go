package audio

import (
	"bytes"
	"fmt"
)

func validateELD(f Format) error {
	if f.SampleRate != 48000 || f.Channels != 2 || f.FramesPerPacket != 480 || !bytes.Equal(f.Cookie, AACELDCookie(RemoteDesktopELD)) {
		return fmt.Errorf("audio: unsupported AAC-ELD stream format")
	}
	return nil
}
