package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

func pcmSamples(values ...int16) []byte {
	var b []byte
	for _, v := range values {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

func TestPCMResamplerStereoAndMono(t *testing.T) {
	stereo := pcmSamples(-32768, 32767, -100, 100, 0, 1)
	r := pcmResampler{rate: 48000, channels: 2}
	got, err := r.convert(stereo)
	if err != nil || !bytes.Equal(got, stereo) {
		t.Fatalf("stereo changed: %x %v", got, err)
	}
	r = pcmResampler{rate: 24000, channels: 1}
	got, err = r.convert(pcmSamples(-1000, 1000, 3000))
	want := pcmSamples(-1000, -1000, 0, 0, 1000, 1000, 2000, 2000, 3000, 3000)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("mono interpolation: %x want %x (%v)", got, want, err)
	}
	before := r
	if _, err = r.convert([]byte{1}); err == nil || r != before {
		t.Fatal("malformed frame changed resampler state")
	}
}

func TestPCMResamplingIsIndependentOfRDPPacketBoundaries(t *testing.T) {
	for _, rate := range []int{8000, 16000, 22050, 44100, 48000, 96000, 192000} {
		for _, channels := range []int{1, 2} {
			t.Run(fmt.Sprintf("%d/%d", rate, channels), func(t *testing.T) {
				var source []byte
				for frame := range rate {
					for ch := range channels {
						source = append(source, pcmSamples(int16((frame+ch*1000)%20000-10000))...)
					}
				}
				whole := pcmResampler{rate: rate, channels: channels}
				want, _ := whole.convert(source)
				streamed := pcmResampler{rate: rate, channels: channels}
				var got []byte
				for len(source) > 0 {
					n := min(len(source), 137*channels*2)
					part, err := streamed.convert(source[:n])
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, part...)
					source = source[n:]
				}
				frames := (rate-1)*pcmOutputRate/rate + 1
				if len(got) != frames*pcmFrameBytes || !bytes.Equal(got, want) {
					t.Fatalf("packet boundary caused drift or discontinuity: %d frames want %d", len(got)/4, frames)
				}
			})
		}
	}
}

func TestPCMStreamUnderrunOverflowAndClose(t *testing.T) {
	s := &pcmStream{}
	buf := bytes.Repeat([]byte{0xff}, pcmBlockBytes)
	if n, err := s.Read(buf); n != len(buf) || err != nil || !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatal("underrun is not silence")
	}
	for i := range pcmQueueBlocks + 3 {
		s.push(bytes.Repeat([]byte{byte(i)}, pcmBlockBytes))
	}
	if status := s.status(); status.Dropped != 3 || status.Enqueued != 13 {
		t.Fatal(status)
	}
	for i := 3; i < 13; i++ {
		if _, err := io.ReadFull(s, buf); err != nil || !bytes.Equal(buf, bytes.Repeat([]byte{byte(i)}, pcmBlockBytes)) {
			t.Fatal("overflow did not retain newest sound", i, err)
		}
	}
	if s.status().Completed != 10 {
		t.Fatal(s.status())
	}
	// Tiny reads preserve complete frames and do not expose borrowed packet memory.
	packet := pcmSamples(11, 22, 33, 44)
	s.push(packet)
	clear(packet)
	for _, want := range pcmSamples(11, 22, 33, 44) {
		b := make([]byte, 1)
		s.Read(b)
		if b[0] != want {
			t.Fatal("partial read or source ownership changed")
		}
	}
	s.close()
	s.close()
	s.push(pcmSamples(1, 2))
	if _, err := s.Read(buf); err != io.EOF || s.count != 0 {
		t.Fatal("closed stream retained sound", err)
	}
}

type memoryPCMDevice struct {
	source  io.Reader
	playing bool
	err     error
}

func (d *memoryPCMDevice) Play()                { d.playing = true }
func (d *memoryPCMDevice) PauseAndStopReading() { d.playing = false }
func (d *memoryPCMDevice) IsPlaying() bool      { return d.playing }
func (d *memoryPCMDevice) Err() error           { return d.err }

func TestPCMPlayerStartsShortSoundsAndReportsDeviceFailure(t *testing.T) {
	d := &memoryPCMDevice{}
	p, err := newPCMPlayer(48000, 2, func(r io.Reader) (pcmDevice, error) { d.source = r; return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Push(pcmSamples(100, -100)); err != nil {
		t.Fatal(err)
	}
	if !d.playing || !p.Status().StartRequested {
		t.Fatal("one-packet sound did not start")
	}
	b := make([]byte, 4)
	d.source.Read(b)
	if !bytes.Equal(b, pcmSamples(100, -100)) {
		t.Fatal("sound not delivered")
	}
	d.err = errors.New("device disconnected")
	if err = p.Push(pcmSamples(1, 2)); !errors.Is(err, d.err) || p.Status().Error == "" || p.Status().Running {
		t.Fatal("device error swallowed")
	}
	p.Close()
	p.Close()
	if d.playing {
		t.Fatal("close left device playing")
	}
	if err = p.Push(pcmSamples(1, 2)); err != io.ErrClosedPipe {
		t.Fatal(err)
	}
	if _, err = d.source.Read(b); err != io.EOF {
		t.Fatal("backend retained source after close")
	}
}

func TestPCMStreamConcurrentReadWriteClose(t *testing.T) {
	s := &pcmStream{}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			s.push(make([]byte, pcmBlockBytes))
			s.status()
		}
	})
	wg.Go(func() {
		buf := make([]byte, 333)
		for range 1000 {
			s.Read(buf)
		}
	})
	wg.Go(func() { s.close() })
	wg.Wait()
}
