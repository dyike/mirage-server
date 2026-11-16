// Package vncfeedback defines Mirage's explicitly negotiated VNC feedback v1.
package vncfeedback

import (
	"encoding/binary"
	"errors"
)

const Encoding int32 = 0x4d464231 // MFB1; never sent without server confirmation.
const Message byte = 249
const Size = 24

type Report struct {
	Presentation                                     bool
	IntervalMS, Frames, Presented, DecodeUS, Pending uint32
}

func (r Report) Marshal() []byte {
	b := make([]byte, Size)
	b[0], b[1] = Message, 1
	if r.Presentation {
		b[2] = 1
	}
	for i, v := range []uint32{r.IntervalMS, r.Frames, r.Presented, r.DecodeUS, r.Pending} {
		binary.BigEndian.PutUint32(b[4+i*4:], v)
	}
	return b
}
func Parse(b []byte) (Report, error) {
	if len(b) != Size || b[0] != Message || b[1] != 1 || b[2] > 1 || b[3] != 0 {
		return Report{}, errors.New("invalid video feedback header")
	}
	r := Report{Presentation: b[2] == 1, IntervalMS: binary.BigEndian.Uint32(b[4:]), Frames: binary.BigEndian.Uint32(b[8:]), Presented: binary.BigEndian.Uint32(b[12:]), DecodeUS: binary.BigEndian.Uint32(b[16:]), Pending: binary.BigEndian.Uint32(b[20:])}
	if r.IntervalMS < 500 || r.IntervalMS > 10000 || r.Frames > 10000 || r.Presented > 10000 || r.DecodeUS > 10000000 || r.Pending > 10000 {
		return Report{}, errors.New("invalid video feedback bounds")
	}
	return r, nil
}
