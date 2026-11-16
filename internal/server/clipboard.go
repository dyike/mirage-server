package server

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const extClipboard uint32 = 0xc0a1e5ce
const clipCaps uint32 = 1 << 24
const clipRequest uint32 = 1 << 25
const clipPeek uint32 = 1 << 26
const clipNotify uint32 = 1 << 27
const clipProvide uint32 = 1 << 28

func (p *rfbPeer) sendClipboard(flags uint32, data []byte) error {
	payload := append(u32(flags), data...)
	return p.send(append([]byte{3, 0, 0, 0}, u32(uint32(-int32(len(payload))))...), payload)
}
func (p *rfbPeer) offerClipboard(text string) error {
	p.clipMu.Lock()
	p.clipText = text
	extended, caps, limit := p.clipExtended, p.clipCaps, p.clipLimit
	p.clipMu.Unlock()
	if !extended {
		data := latin1Bytes(text)
		return p.send(append([]byte{3, 0, 0, 0}, u32(uint32(len(data)))...), data)
	}
	if caps&clipProvide != 0 && len(text)+1 <= int(limit) {
		return p.provideClipboard(text)
	}
	if caps&clipNotify != 0 {
		return p.sendClipboard(clipNotify|1, nil)
	}
	return nil
}
func (p *rfbPeer) provideClipboard(text string) error {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	data := append([]byte(text), 0)
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write(u32(uint32(len(data))))
	w.Write(data)
	if err := w.Close(); err != nil {
		return err
	}
	return p.sendClipboard(clipProvide|1, b.Bytes())
}
func (p *rfbPeer) extendedClipboard(data []byte) error {
	p.clipMu.Lock()
	enabled := p.clipExtended
	p.clipMu.Unlock()
	if !enabled || len(data) < 4 {
		return errors.New("unnegotiated extended clipboard")
	}
	flags := networkOrder.Uint32(data)
	body := data[4:]
	if flags&clipCaps != 0 {
		if flags&1 != 0 && len(body) < 4 {
			return io.ErrUnexpectedEOF
		}
		p.clipMu.Lock()
		p.clipCaps = flags
		if flags&1 != 0 {
			p.clipLimit = networkOrder.Uint32(body)
		}
		p.clipMu.Unlock()
		return nil
	}
	switch flags & 0xff000000 {
	case clipRequest:
		p.clipMu.Lock()
		text := p.clipText
		p.clipMu.Unlock()
		if flags&1 != 0 {
			return p.provideClipboard(text)
		}
	case clipPeek:
		return p.sendClipboard(clipNotify|1, nil)
	case clipNotify:
		if flags&1 != 0 {
			return p.sendClipboard(clipRequest|1, nil)
		}
	case clipProvide:
		if flags&0xffff != 1 {
			return errors.New("unsupported clipboard format")
		}
		r, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer r.Close()
		decoded, err := io.ReadAll(io.LimitReader(r, (1<<20)+5))
		if err != nil {
			return err
		}
		if len(decoded) < 5 || len(decoded) > (1<<20)+4 {
			return errors.New("clipboard expansion too large")
		}
		n := networkOrder.Uint32(decoded)
		if int(n) != len(decoded)-4 || decoded[len(decoded)-1] != 0 {
			return errors.New("invalid clipboard text length")
		}
		text := decoded[4 : len(decoded)-1]
		if !utf8.Valid(text) {
			return errors.New("invalid UTF-8 clipboard")
		}
		if p.clipboard != nil {
			return p.clipboard.Set(strings.ReplaceAll(string(text), "\r\n", "\n"))
		}
	default:
		return errors.New("invalid clipboard action")
	}
	return nil
}
