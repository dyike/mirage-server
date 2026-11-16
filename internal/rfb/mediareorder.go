package rfb

import (
	"time"

	"github.com/pion/rtp"
)

// Over the internet datagrams arrive out of order, and the assembler drops
// an access unit at any sequence gap. reorderBuffer holds packets that come
// early and hands them on in sequence order; a gap that outlasts
// reorderDepth later packets or reorderDelay counts as lost, and the
// assembler sees it.

const (
	reorderSlots = 128
	reorderDepth = 48 // packets past a gap before it counts as lost
	reorderDelay = 30 * time.Millisecond
)

type reorderPacket struct {
	h       rtp.Header
	payload []byte
	arrived time.Time
	held    bool
}

type reorderBuffer struct {
	slots   [reorderSlots]reorderPacket
	next    uint16 // the sequence number handed on next
	top     uint16 // the highest sequence number held
	held    int
	started bool
	gapAt   time.Time // when next first went missing
	late    int64     // packets that came after their slot was given up
	lost    int64     // sequence numbers given up on
}

// push takes one packet; payload must stay valid until it is handed on.
// emit receives packets in sequence order, with gaps where packets were
// lost.
func (b *reorderBuffer) push(h *rtp.Header, payload []byte, now time.Time, emit func(*rtp.Header, []byte)) {
	if !b.started {
		b.started, b.next, b.top = true, h.SequenceNumber, h.SequenceNumber
	}
	d := int16(h.SequenceNumber - b.next)
	switch {
	case d < 0:
		b.late++ // already given up on, or a duplicate
		return
	case d == 0 && b.held == 0:
		b.next++
		b.top = b.next - 1
		emit(h, payload)
		return
	case int(d) >= reorderSlots:
		b.skipTo(h.SequenceNumber-reorderSlots+1, emit)
	}
	s := &b.slots[h.SequenceNumber%reorderSlots]
	if s.held {
		return // duplicate
	}
	*s = reorderPacket{h: *h, payload: payload, arrived: now, held: true}
	b.held++
	if int16(h.SequenceNumber-b.top) > 0 {
		b.top = h.SequenceNumber
	}
	b.drain(emit)
	b.expire(now, emit)
}

// deadline lets the socket wake up even if this was the desktop's last frame.
// Waiting for another RTP packet to check the gap can otherwise stall forever.
func (b *reorderBuffer) deadline() time.Time {
	if b.held == 0 {
		return time.Time{}
	}
	return b.gapAt.Add(reorderDelay)
}

func (b *reorderBuffer) expire(now time.Time, emit func(*rtp.Header, []byte)) {
	for b.held > 0 {
		// Use arrival time rather than restarting the wait at every gap.
		// Several missing packets in a final frame share the same wait budget.
		b.gapAt = b.slots[b.oldest()%reorderSlots].arrived
		if int(int16(b.top-b.next)) < reorderDepth && now.Before(b.deadline()) {
			return
		}
		b.skipTo(b.oldest(), emit)
	}
}

// drain hands on the packets that continue the sequence.
func (b *reorderBuffer) drain(emit func(*rtp.Header, []byte)) {
	for b.held > 0 {
		s := &b.slots[b.next%reorderSlots]
		if !s.held || s.h.SequenceNumber != b.next {
			return
		}
		h, p := s.h, s.payload
		*s = reorderPacket{}
		b.held--
		b.next++
		b.gapAt = time.Time{}
		emit(&h, p)
	}
}

// oldest returns the lowest sequence number held.
func (b *reorderBuffer) oldest() uint16 {
	for seq := b.next; ; seq++ {
		if s := &b.slots[seq%reorderSlots]; s.held && s.h.SequenceNumber == seq {
			return seq
		}
	}
}

// skipTo gives up on the packets before seq, handing on what it holds in
// between, and continues from seq.
func (b *reorderBuffer) skipTo(seq uint16, emit func(*rtp.Header, []byte)) {
	skipped := int64(uint16(seq - b.next))
	for ; b.held > 0 && b.next != seq; b.next++ {
		s := &b.slots[b.next%reorderSlots]
		if s.held && s.h.SequenceNumber == b.next {
			h, p := s.h, s.payload
			*s = reorderPacket{}
			b.held--
			skipped--
			emit(&h, p)
		}
	}
	b.lost += skipped
	b.next, b.gapAt = seq, time.Time{}
	b.drain(emit)
}
