package video

import (
	"sync"

	"github.com/dyike/mirage-server/internal/frame"
)

// Keep timestamps by decoder token, rather than FIFO: codecs can reorder or
// discard pictures. The fixed window also bounds storage when output stalls.
type frameTimes struct {
	mu    sync.Mutex
	slots [256]struct {
		id uint64
		at frame.Timing
	}
}

func (f *frameTimes) put(id uint64, at frame.Timing) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slots[id%uint64(len(f.slots))] = struct {
		id uint64
		at frame.Timing
	}{id, at}
}

func (f *frameTimes) take(id uint64) frame.Timing {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &f.slots[id%uint64(len(f.slots))]
	if s.id != id {
		return frame.Timing{}
	}
	at := s.at
	s.at = frame.Timing{}
	return at
}
