package server

import "github.com/dyike/mirage-server/internal/video"

// Apple Screen Sharing's HEVC transport adds a DON to every NAL/FU and
// appends emulation-prevention-escaped presentation controls to the last VCL.
// The GPU encoder still produces ordinary Annex B; only compressed bytes pass
// through this packetizer. One full-canvas tile is explicitly negotiated.
type ardHEVCPayloader struct {
	width, height uint16
	don, frame    uint16
}

func (p *ardHEVCPayloader) Payload(mtu uint16, data []byte) [][]byte {
	if mtu < 64 {
		return nil
	}
	p.don++
	if p.don == 0 {
		p.don = 1
	}
	nals := video.SplitAnnexB(data)
	last := -1
	refresh := false
	for i, n := range nals {
		if len(n) < 2 {
			continue
		}
		typ := (n[0] >> 1) & 63
		if typ < 32 {
			last = i
		}
		if typ >= 16 && typ <= 23 {
			refresh = true
		}
	}
	if last < 0 {
		return nil
	}
	trailer := make([]byte, 8)
	networkOrder.PutUint16(trailer[4:], p.width)
	networkOrder.PutUint16(trailer[6:], p.height)
	flags := byte(0x13) // screen/window, end-of-frame, tile origin
	if refresh {
		flags |= 8
		trailer = networkOrder.AppendUint16(trailer, p.width)
		trailer = networkOrder.AppendUint16(trailer, p.height)
	}
	trailer = append(trailer, 0, 0, 0, 0, 0, 0, flags) // tile origin x/y, reserved, control byte
	var result [][]byte
	for i, n := range nals {
		if len(n) < 2 {
			continue
		}
		typ := (n[0] >> 1) & 63
		if typ == 39 || typ == 40 {
			continue
		} // presentation controls replace optional SEI
		if typ >= 32 {
			if len(n)+6 > int(mtu) {
				return nil
			}
			packet := []byte{n[0]&0x81 | 48<<1, n[1], byte(p.don >> 8), byte(p.don)}
			packet = networkOrder.AppendUint16(packet, uint16(len(n)))
			packet = append(packet, n...)
			result = append(result, packet)
			continue
		}
		body := n[2:]
		if i == last {
			body = ardAppendEscaped(append([]byte(nil), body...), trailer)
		}
		if len(body)+4 <= int(mtu) {
			packet := []byte{n[0], n[1], byte(p.don >> 8), byte(p.don)}
			result = append(result, append(packet, body...))
			continue
		}
		for offset := 0; offset < len(body); {
			end := min(len(body), offset+int(mtu)-5)
			flags := typ
			if offset == 0 {
				flags |= 0x80
			}
			if end == len(body) {
				flags |= 0x40
			}
			packet := []byte{n[0]&0x81 | 49<<1, n[1], flags, byte(p.don >> 8), byte(p.don)}
			result = append(result, append(packet, body[offset:end]...))
			offset = end
		}
	}
	return result
}

func ardAppendEscaped(dst, src []byte) []byte {
	zeros := 0
	for i := len(dst) - 1; i >= 0 && zeros < 2 && dst[i] == 0; i-- {
		zeros++
	}
	for _, v := range src {
		if zeros >= 2 && v <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		dst = append(dst, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return dst
}
