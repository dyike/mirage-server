package server

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
)

const encZlib uint32 = 6
const encTight uint32 = 7
const encZRLE uint32 = 16

type pixelEncoder struct {
	zrle, tight, plain *zlib.Writer
	zbuf, tbuf, pbuf   bytes.Buffer
}

func (e *pixelEncoder) close() {
	if e.plain != nil {
		e.plain.Close()
	}
	if e.zrle != nil {
		e.zrle.Close()
	}
	if e.tight != nil {
		e.tight.Close()
	}
}
func rectHeader(r image.Rectangle, encoding uint32) []byte {
	b := make([]byte, 12)
	networkOrder.PutUint16(b, uint16(r.Min.X))
	networkOrder.PutUint16(b[2:], uint16(r.Min.Y))
	networkOrder.PutUint16(b[4:], uint16(r.Dx()))
	networkOrder.PutUint16(b[6:], uint16(r.Dy()))
	networkOrder.PutUint32(b[8:], encoding)
	return b
}
func updateMessage(rects ...[]byte) []byte {
	b := []byte{0, 0, 0, 0}
	networkOrder.PutUint16(b[2:], uint16(len(rects)))
	for _, r := range rects {
		b = append(b, r...)
	}
	return b
}
func compactLength(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	if n < 16384 {
		return []byte{byte(n) | 128, byte(n >> 7)}
	}
	return []byte{byte(n) | 128, byte(n>>7) | 128, byte(n >> 14)}
}
func compactPixels(p rfbFormat, packed []byte) []byte {
	if p[0] != 32 || p[1] > 24 {
		return packed
	}
	var used uint32
	for j := 0; j < 3; j++ {
		used |= uint32(binary.BigEndian.Uint16(p[4+2*j:])) << p[10+j]
	}
	skip := -1
	if used&0xff000000 == 0 {
		skip = 3
	} else if used&255 == 0 {
		skip = 0
	}
	if skip < 0 {
		return packed
	}
	if p[2] != 0 {
		skip = 3 - skip
	}
	out := make([]byte, 0, len(packed)/4*3)
	for i := 0; i < len(packed); i += 4 {
		for j := 0; j < 4; j++ {
			if j != skip {
				out = append(out, packed[i+j])
			}
		}
	}
	return out
}
func (e *pixelEncoder) encode(enc uint32, p rfbFormat, rgba []byte, w, h int) ([]byte, error) {
	if len(rgba) != w*h*4 {
		return nil, errors.New("invalid RGBA framebuffer")
	}
	if enc == 0 {
		data, err := p.pack(rgba, w, h)
		if err != nil {
			return nil, err
		}
		return updateMessage(append(rectHeader(image.Rect(0, 0, w, h), 0), data...)), nil
	}
	if enc == encZlib {
		return e.encodeZlib(p, rgba, w, h)
	}
	if enc == encZRLE {
		e.zbuf.Reset()
		if e.zrle == nil {
			var err error
			e.zrle, err = zlib.NewWriterLevel(&e.zbuf, zlib.BestSpeed)
			if err != nil {
				return nil, err
			}
		}
		for y := 0; y < h; y += 64 {
			for x := 0; x < w; x += 64 {
				tw, th := min(64, w-x), min(64, h-y)
				tile := make([]byte, 0, tw*th*4)
				for row := 0; row < th; row++ {
					start := ((y+row)*w + x) * 4
					tile = append(tile, rgba[start:start+tw*4]...)
				}
				packed, err := p.pack(tile, tw, th)
				if err != nil {
					return nil, err
				}
				packed = compactPixels(p, packed)
				size := len(packed) / (tw * th)
				solid := true
				for i := size; i < len(packed); i += size {
					if !bytes.Equal(packed[:size], packed[i:i+size]) {
						solid = false
						break
					}
				}
				if solid {
					e.zrle.Write([]byte{1})
					e.zrle.Write(packed[:size])
				} else {
					e.zrle.Write([]byte{0})
					e.zrle.Write(packed)
				}
			}
		}
		if err := e.zrle.Flush(); err != nil {
			return nil, err
		}
		r := append(rectHeader(image.Rect(0, 0, w, h), encZRLE), u32(uint32(e.zbuf.Len()))...)
		r = append(r, e.zbuf.Bytes()...)
		return updateMessage(r), nil
	}
	if enc != encTight {
		return nil, errors.New("unsupported pixel encoding")
	}
	var rects [][]byte
	for y := 0; y < h; y += 128 {
		for x := 0; x < w; x += 1024 {
			tw, th := min(1024, w-x), min(128, h-y)
			tile := make([]byte, 0, tw*th*4)
			for row := 0; row < th; row++ {
				start := ((y+row)*w + x) * 4
				tile = append(tile, rgba[start:start+tw*4]...)
			}
			var data []byte
			if p[0] == 32 && p[1] == 24 && networkOrder.Uint16(p[4:]) == 255 && networkOrder.Uint16(p[6:]) == 255 && networkOrder.Uint16(p[8:]) == 255 {
				data = make([]byte, 0, tw*th*3)
				for i := 0; i < len(tile); i += 4 {
					data = append(data, tile[i:i+3]...)
				}
			} else {
				var err error
				data, err = p.pack(tile, tw, th)
				if err != nil {
					return nil, err
				}
			}
			control := byte(0)
			e.tbuf.Reset()
			if len(data) >= 12 {
				if e.tight == nil {
					var err error
					e.tight, err = zlib.NewWriterLevel(&e.tbuf, zlib.BestSpeed)
					if err != nil {
						return nil, err
					}
					control = 1
				}
				if _, err := e.tight.Write(data); err != nil {
					return nil, err
				}
				if err := e.tight.Flush(); err != nil {
					return nil, err
				}
				data = append(compactLength(e.tbuf.Len()), e.tbuf.Bytes()...)
			}
			r := append(rectHeader(image.Rect(x, y, x+tw, y+th), encTight), control)
			r = append(r, data...)
			rects = append(rects, r)
		}
	}
	return updateMessage(rects...), nil
}

// RFB Zlib uses packed full pixels (not ZRLE CPIXEL) and one continuous
// deflate stream per connection. Bound rectangles so viewers can decode and
// paint incrementally instead of buffering a multi-megapixel rectangle.
func (e *pixelEncoder) encodeZlib(p rfbFormat, rgba []byte, w, h int) ([]byte, error) {
	if e.plain == nil {
		var err error
		e.plain, err = zlib.NewWriterLevel(&e.pbuf, zlib.BestSpeed)
		if err != nil {
			return nil, err
		}
	}
	var rects [][]byte
	for y := 0; y < h; y += 128 {
		for x := 0; x < w; x += 256 {
			tw, th := min(256, w-x), min(128, h-y)
			tile := make([]byte, 0, tw*th*4)
			for row := 0; row < th; row++ {
				start := ((y+row)*w + x) * 4
				tile = append(tile, rgba[start:start+tw*4]...)
			}
			packed, err := p.pack(tile, tw, th)
			if err != nil {
				return nil, err
			}
			e.pbuf.Reset()
			if _, err = e.plain.Write(packed); err != nil {
				return nil, err
			}
			if err = e.plain.Flush(); err != nil {
				return nil, err
			}
			r := append(rectHeader(image.Rect(x, y, x+tw, y+th), encZlib), u32(uint32(e.pbuf.Len()))...)
			r = append(r, e.pbuf.Bytes()...)
			rects = append(rects, r)
		}
	}
	return updateMessage(rects...), nil
}

// Serialize the revision check, compression and transmission with layout
// announcements. A persistent Zlib stream must never encode a discarded frame.
func (p *rfbPeer) sendPixelFrame(e *pixelEncoder, enc uint32, pf rfbFormat, data []byte, size image.Point, revision uint64) (int, error) {
	p.write.Lock()
	defer p.write.Unlock()
	p.state.Lock()
	stale := p.state.revision != revision || p.state.encoding != enc || p.state.ardAwaitingRefresh
	p.state.Unlock()
	if stale {
		return 0, renegotiate
	}
	message, err := e.encode(enc, pf, data, size.X, size.Y)
	if err != nil {
		return 0, err
	}
	_, err = p.sendPacedLocked(0, message)
	return len(message), err
}
