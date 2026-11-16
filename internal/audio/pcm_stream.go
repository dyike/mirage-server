package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

const (
	pcmOutputRate  = 48000
	pcmFrameBytes  = 4                                  // stereo signed 16-bit
	pcmBlockBytes  = pcmOutputRate * pcmFrameBytes / 50 // 20 ms
	pcmQueueBlocks = 10                                 // bound network jitter to 200 ms
)

// pcmResampler keeps its phase across RDP packets. The shared output device
// stays at 48 kHz stereo even when a server changes its negotiated format.
type pcmResampler struct {
	rate, channels int
	previous       [2]int32
	phase          int
	havePrevious   bool
}

func (r *pcmResampler) convert(pcm []byte) ([]byte, error) {
	frameBytes := r.channels * 2
	if len(pcm)%frameBytes != 0 {
		return nil, fmt.Errorf("audio: incomplete %d-channel PCM frame", r.channels)
	}
	out := make([]byte, 0, (len(pcm)/frameBytes*pcmOutputRate/r.rate+1)*pcmFrameBytes)
	appendFrame := func(v [2]int32) {
		for _, sample := range v {
			out = binary.LittleEndian.AppendUint16(out, uint16(int16(sample)))
		}
	}
	for len(pcm) > 0 {
		sample := [2]int32{int32(int16(binary.LittleEndian.Uint16(pcm))), 0}
		sample[1] = sample[0]
		if r.channels == 2 {
			sample[1] = int32(int16(binary.LittleEndian.Uint16(pcm[2:])))
		}
		pcm = pcm[frameBytes:]
		if !r.havePrevious {
			r.previous, r.havePrevious, r.phase = sample, true, r.rate
			appendFrame(sample)
			continue
		}
		for r.phase <= pcmOutputRate {
			var interpolated [2]int32
			for ch := range interpolated {
				interpolated[ch] = int32((int64(r.previous[ch])*int64(pcmOutputRate-r.phase) + int64(sample[ch])*int64(r.phase)) / pcmOutputRate)
			}
			appendFrame(interpolated)
			r.phase += r.rate
		}
		r.phase -= pcmOutputRate
		r.previous = sample
	}
	return out, nil
}

// pcmStream never waits for the network in the audio callback. Underruns
// produce silence without EOF, so later packets resume on the same player.
// A slow device drops the oldest blocks instead of accumulating stale sound.
type pcmStream struct {
	mu                           sync.Mutex
	blocks                       [pcmQueueBlocks][]byte
	head, count, offset          int
	closed                       bool
	enqueued, completed, dropped int64
}

func (s *pcmStream) push(pcm []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for len(pcm) > 0 {
		n := min(len(pcm), pcmBlockBytes)
		if s.count == len(s.blocks) {
			s.blocks[s.head] = nil
			s.head = (s.head + 1) % len(s.blocks)
			s.count--
			s.offset = 0
			s.dropped++
		}
		tail := (s.head + s.count) % len(s.blocks)
		s.blocks[tail] = append([]byte(nil), pcm[:n]...)
		s.count++
		s.enqueued++
		pcm = pcm[n:]
	}
}

func (s *pcmStream) Read(dst []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.EOF
	}
	read := 0
	for read < len(dst) && s.count > 0 {
		block := s.blocks[s.head]
		n := copy(dst[read:], block[s.offset:])
		read += n
		s.offset += n
		if s.offset == len(block) {
			s.blocks[s.head] = nil
			s.head = (s.head + 1) % len(s.blocks)
			s.offset = 0
			s.count--
			s.completed++
		}
	}
	clear(dst[read:])
	return len(dst), nil
}

func (s *pcmStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	clear(s.blocks[:])
	s.count, s.offset = 0, 0
}

func (s *pcmStream) status() PlaybackStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PlaybackStatus{Enqueued: s.enqueued, Completed: s.completed, Dropped: s.dropped}
}
