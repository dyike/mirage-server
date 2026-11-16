package audio

import (
	"fmt"
	"io"
	"sync"
)

type pcmDevice interface {
	Play()
	PauseAndStopReading()
	IsPlaying() bool
	Err() error
}

// pcmPlayer is also exercised with an in-memory device on Apple build hosts.
// The actual Windows/Linux/Android device is supplied by Oto.
type pcmPlayer struct {
	mu              sync.Mutex
	stream          pcmStream
	resampler       pcmResampler
	device          pcmDevice
	started, closed bool
	err             error
	decoderName     string
	decoder         interface {
		Decode([]byte) ([]byte, error)
		Close()
	}
}

func newPCMPlayer(rate, channels int, open func(io.Reader) (pcmDevice, error)) (*pcmPlayer, error) {
	if rate < 8000 || rate > 192000 || channels < 1 || channels > 2 {
		return nil, fmt.Errorf("audio: unsupported PCM format %d Hz × %d", rate, channels)
	}
	p := &pcmPlayer{resampler: pcmResampler{rate: rate, channels: channels}}
	device, err := open(&p.stream)
	if err != nil {
		return nil, err
	}
	p.device = device
	return p, nil
}

func (p *pcmPlayer) Push(pcm []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}
	if err := p.device.Err(); err != nil {
		p.err = err
		return err
	}
	if p.decoder != nil {
		var err error
		pcm, err = p.decoder.Decode(pcm)
		if err != nil {
			p.err = err
			return err
		}
	}
	data, err := p.resampler.convert(pcm)
	if err != nil {
		p.err = err
		return err
	}
	p.stream.push(data)
	p.err = nil
	if !p.started && len(data) > 0 {
		p.device.Play()
		p.started = true
	}
	return nil
}

func (p *pcmPlayer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	// End the reader first, then wait until the backend has stopped using it.
	p.stream.close()
	p.device.PauseAndStopReading()
	if p.decoder != nil {
		p.decoder.Close()
	}
}

func (p *pcmPlayer) Status() PlaybackStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.stream.status()
	s.Decoder = p.decoderName
	s.StartRequested = p.started
	s.RunningKnown = true
	s.Running = !p.closed && p.started && p.device.IsPlaying()
	err := p.device.Err()
	if err == nil {
		err = p.err
	}
	if err != nil {
		s.Error = err.Error()
		s.Running = false
	}
	return s
}

func (p *pcmPlayer) Dropped() int64 { return p.stream.status().Dropped }
