//go:build linux && !android && (amd64 || arm64)

package audio

import (
	"encoding/binary"
	"github.com/dyike/mirage-server/internal/audio/internal/eldcodec"
	"math"
	"os"
	"testing"
)

func TestELDEncoderRoundTrip(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_AUDIO") != "1" {
		t.Skip("requires system FDK encoder")
	}
	e, err := NewELDEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d, err := eldcodec.New(RemoteDesktopELD)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	energy := int64(0)
	for frame := 0; frame < 30; frame++ {
		pcm := make([]byte, 1920)
		for i := 0; i < 480; i++ {
			v := int16(8000 * math.Sin(2*math.Pi*440*float64(frame*480+i)/48000))
			for ch := 0; ch < 2; ch++ {
				binary.LittleEndian.PutUint16(pcm[(i*2+ch)*2:], uint16(v))
			}
		}
		packet, err := e.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		if len(packet) == 0 {
			continue
		}
		decoded, err := d.Decode(packet)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(decoded); i += 2 {
			v := int64(int16(binary.LittleEndian.Uint16(decoded[i:])))
			energy += v * v
		}
	}
	if energy == 0 {
		t.Fatal("decoded audio is silent")
	}
	e.Close()
	if _, err := e.Encode(make([]byte, 1920)); err == nil {
		t.Fatal("closed encoder accepted audio")
	}
}
