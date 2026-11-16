package rfb

import (
	"bytes"
	"compress/zlib"
	"io"
)

// The server's answer (media reply kind 2) carries, per stream, a binary
// property list whose avcMediaStreamNegotiatorMediaBlob is a zlib-compressed
// protobuf; the video blob's field 5 describes the stream it will send.

type mediaAnswer struct {
	ssrc          uint32
	width, height int
	tiles         int
	ltrp          bool // long-term reference pictures, acknowledged per band
}

func parseMediaAnswer(body []byte) (*mediaAnswer, error) {
	if len(body) < 18 {
		return nil, protoErr("media answer of %d bytes", len(body))
	}
	audio, vid := int(be.Uint16(body[8:])), int(be.Uint16(body[10:]))
	if len(body) < 18+audio+vid {
		return nil, protoErr("media answer of %d bytes, want %d", len(body), 18+audio+vid)
	}
	blob := body[18+audio : 18+audio+vid]
	if v, ok := plistData(blob, "avcMediaStreamNegotiatorMediaBlob"); ok {
		blob = v
	}
	zr, err := zlib.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, protoErr("media answer blob: %v", err)
	}
	pb, err := io.ReadAll(io.LimitReader(zr, 1<<20))
	if err != nil {
		return nil, protoErr("media answer blob: %v", err)
	}
	a := &mediaAnswer{}
	for _, f := range pbFields(pb) {
		if f.num == 5 && f.bytes != nil {
			for _, g := range pbFields(f.bytes) {
				switch g.num {
				case 1:
					a.ssrc = uint32(g.v)
				case 4:
					a.width = int(g.v)
				case 5:
					a.height = int(g.v)
				case 6:
					a.tiles = int(g.v)
				case 7:
					a.ltrp = g.v != 0
				}
			}
			break
		}
	}
	return a, nil
}

type pbField struct {
	num   int
	v     uint64
	bytes []byte // length-delimited fields only
}

// pbFields splits a protobuf message into its varint and length-delimited
// fields, stopping at anything else or malformed input.
func pbFields(b []byte) []pbField {
	var out []pbField
	varint := func() (uint64, bool) {
		var v uint64
		for i := 0; i < 10 && len(b) > 0; i++ {
			c := b[0]
			b = b[1:]
			v |= uint64(c&0x7f) << (7 * i)
			if c < 0x80 {
				return v, true
			}
		}
		return 0, false
	}
	for len(b) > 0 {
		key, ok := varint()
		if !ok {
			break
		}
		f := pbField{num: int(key >> 3)}
		switch key & 7 {
		case 0:
			if f.v, ok = varint(); !ok {
				return out
			}
		case 2:
			n, ok := varint()
			if !ok || n > uint64(len(b)) {
				return out
			}
			f.bytes, b = b[:n], b[n:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

// plistData finds a data value by key in a binary property list's top
// dictionary.
func plistData(b []byte, key string) ([]byte, bool) {
	if len(b) < 40 || string(b[:8]) != "bplist00" {
		return nil, false
	}
	t := b[len(b)-32:]
	offSize, refSize := int(t[6]), int(t[7])
	n, top, table := be.Uint64(t[8:]), be.Uint64(t[16:]), be.Uint64(t[24:])
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || n > 1<<16 || table+n*uint64(offSize) > uint64(len(b)) {
		return nil, false
	}
	uintAt := func(p []byte, size int) uint64 {
		var v uint64
		for _, c := range p[:size] {
			v = v<<8 | uint64(c)
		}
		return v
	}
	offset := func(i uint64) (int, bool) {
		if i >= n {
			return 0, false
		}
		o := uintAt(b[table+i*uint64(offSize):], offSize)
		return int(o), o < table
	}
	// object returns an object's marker, payload start and length.
	object := func(i uint64) (byte, int, int, bool) {
		o, ok := offset(i)
		if !ok {
			return 0, 0, 0, false
		}
		m := b[o]
		length, start := int(m&0x0f), o+1
		if length == 0x0f && m>>4 != 0x1 {
			if start >= len(b) || b[start]>>4 != 0x1 {
				return 0, 0, 0, false
			}
			size := 1 << (b[start] & 0x0f)
			if start+1+size > len(b) {
				return 0, 0, 0, false
			}
			length = int(uintAt(b[start+1:], size))
			start += 1 + size
		}
		return m >> 4, start, length, true
	}
	kind, start, count, ok := object(top)
	if !ok || kind != 0xd || start+2*count*refSize > len(b) {
		return nil, false
	}
	for i := range count {
		kk, ks, kl, ok := object(uintAt(b[start+i*refSize:], refSize))
		if !ok || kk != 0x5 || ks+kl > len(b) || string(b[ks:ks+kl]) != key {
			continue
		}
		vk, vs, vl, ok := object(uintAt(b[start+(count+i)*refSize:], refSize))
		if !ok || vk != 0x4 || vs+vl > len(b) {
			return nil, false
		}
		return b[vs : vs+vl], true
	}
	return nil, false
}
