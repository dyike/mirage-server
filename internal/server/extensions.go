package server

import (
	"context"
	"errors"
	"github.com/dyike/mirage-server/internal/vncfeedback"
	"image"
	"log"
	"net"
	"sync"
	"time"

	"github.com/dyike/mirage-server/internal/server/input"
)

// Clipboard is a per-authenticated-session desktop clipboard subscription.
type Clipboard interface {
	Set(string) error
	Updates() <-chan string
	Close() error
}
type AudioFormat struct {
	Format, Channels byte
	Rate             uint32
}
type rfbPeer struct {
	inputTiming struct {
		sync.Mutex
		since          time.Time
		count          uint64
		total, maximum time.Duration
	}
	ard                 *ardPeer
	videoPacer          videoPacer
	server              *RFBServer
	ctx                 context.Context
	cancel              context.CancelFunc
	conn                net.Conn
	state               *rfbState
	controls            InputSession
	clipboard           Clipboard
	write               sync.Mutex
	wg                  sync.WaitGroup
	audioCancel         context.CancelFunc
	audioDone           chan struct{}
	audioFormat         AudioFormat
	audioEnabled        bool
	advertised          map[uint32]bool
	led                 uint32
	clipMu              sync.Mutex
	clipText            string
	clipExtended        bool
	clipCaps, clipLimit uint32
	requests            chan struct{}
	cursor              Cursor
}

func (p *rfbPeer) send(chunks ...[]byte) error {
	_, err := p.sendPaced(0, chunks...)
	return err
}
func (p *rfbPeer) sendPaced(bitrate int, chunks ...[]byte) (time.Duration, error) {
	p.write.Lock()
	defer p.write.Unlock()
	return p.sendPacedLocked(bitrate, chunks...)
}

// Caller holds write for a complete protocol transition.
func (p *rfbPeer) sendPacedLocked(bitrate int, chunks ...[]byte) (time.Duration, error) {
	var waited time.Duration
	for _, b := range chunks {
		for len(b) > 0 {
			if err := p.ctx.Err(); err != nil {
				return waited, err
			}
			// Bound every write, including unpaced ZRLE frames. The deadline
			// measures lack of progress rather than the duration of a whole frame.
			n := min(len(b), 16<<10)
			if bitrate > 0 {
				delay, err := p.videoPacer.wait(p.ctx, n, bitrate)
				waited += delay
				if err != nil {
					return waited, err
				}
			}
			p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if err := writeBytes(p.conn, b[:n]); err != nil {
				p.cancel()
				return waited, err
			}
			b = b[n:]
		}
	}
	return waited, nil
}
func (p *rfbPeer) announce(offers map[uint32]bool) error {
	if p.ard != nil {
		if err := p.announceARD(offers); err != nil {
			return err
		}
	}
	if offers[extClipboard] && p.clipboard != nil {
		p.clipMu.Lock()
		first := !p.clipExtended
		p.clipExtended = true
		p.clipMu.Unlock()
		if first {
			if err := p.sendClipboard(clipCaps|clipRequest|clipPeek|clipNotify|clipProvide|1, u32(1<<20)); err != nil {
				return err
			}
		}
	}

	for _, code := range []int32{-258, -259, -261, -316, -308, -239, vncfeedback.Encoding} {
		v := uint32(code)
		// ARD already uses authenticated RTCP feedback. Older Mac clients
		// send the ordinary VNC feedback without the Apple record write lock.
		// Do not negotiate this unrelated extension on an ARD connection.
		if p.ard != nil && code == vncfeedback.Encoding {
			continue
		}
		if !offers[v] || p.advertised[v] {
			continue
		}
		if code == -259 && p.server.Audio == nil {
			continue
		}
		if (code == -258 || code == -261 || code == -316) && p.controls == nil {
			continue
		}
		if code == -239 && !p.server.EmbeddedCursor && p.server.OpenCursor == nil {
			continue
		}
		var data []byte
		switch code {
		case -261:
			data = []byte{byte(p.controls.LEDs())}
			p.led = p.controls.LEDs()
		case -308:
			p.state.Lock()
			d := p.state.desktop.clone()
			p.state.Unlock()
			if err := p.send(updateMessage(desktopRectangle(d, 0, 0))); err != nil {
				return err
			}
			p.advertised[v] = true
			continue

		}
		if code == -239 && p.server.OpenCursor != nil {
			var err error
			p.cursor, err = p.server.OpenCursor()
			if err != nil {
				return err
			}
			p.advertised[v] = true
			p.state.Lock()
			p.state.cursorActive = true
			p.state.Unlock()
			p.watchCursor()
			continue
		}
		// Empty Cursor hides the local cursor when PipeWire embeds it in video.
		if err := p.send(updateMessage(append(rectHeader(image.Rectangle{}, v), data...))); err != nil {
			return err
		}
		p.advertised[v] = true
	}
	continuous := uint32(0xfffffec7) // -313
	if offers[continuous] && !p.advertised[continuous] {
		p.advertised[continuous] = true
		if err := p.send([]byte{150}); err != nil {
			return err
		}
	}
	return nil
}
func (p *rfbPeer) apply(e input.Event) error {
	started := time.Now()
	defer func() {
		elapsed := time.Since(started)
		s := &p.inputTiming
		s.Lock()
		defer s.Unlock()
		if s.since.IsZero() {
			s.since = started
		}
		s.count++
		s.total += elapsed
		s.maximum = max(s.maximum, elapsed)
		if time.Since(s.since) >= 10*time.Second {
			log.Printf("RFB input dispatch: events=%d avg=%s max=%s", s.count, (s.total / time.Duration(s.count)).Round(time.Microsecond), s.maximum.Round(time.Microsecond))
			s.since = time.Now()
			s.count = 0
			s.total = 0
			s.maximum = 0
		}
	}()
	if p.controls == nil {
		return nil
	}
	if err := p.controls.Apply(e); err != nil {
		return err
	}
	led := p.controls.LEDs()
	if p.advertised[uint32(0xfffffefb)] && led != p.led {
		p.led = led
		return p.send(updateMessage(append(rectHeader(image.Rectangle{}, 0xfffffefb), byte(led))))
	}
	return nil
}
func (p *rfbPeer) stopAudio() {
	if p.audioCancel != nil {
		p.audioCancel()
		<-p.audioDone
		p.audioCancel = nil
	}
}
func (p *rfbPeer) startAudio() error {
	p.stopAudio()
	if p.server.Audio == nil {
		return errors.New("audio unavailable")
	}
	ctx, cancel := context.WithCancel(p.ctx)
	p.audioCancel = cancel
	p.audioDone = make(chan struct{})
	done := p.audioDone
	format := p.audioFormat
	if err := p.send([]byte{255, 1, 0, 1}); err != nil {
		cancel()
		close(done)
		return err
	}
	go func() {
		defer close(done)
		err := p.server.Audio(ctx, format, func(data []byte) error {
			frameSize := []int{1, 1, 2, 2, 4, 4}[format.Format] * int(format.Channels)
			if len(data) > 256<<10 || len(data)%frameSize != 0 {
				return errors.New("invalid audio packet")
			}
			return p.send(append([]byte{255, 1, 0, 2}, u32(uint32(len(data)))...), data)
		})
		if err != nil && ctx.Err() == nil && p.server.OnError != nil {
			p.server.OnError(err)
		}
		if p.ctx.Err() == nil {
			p.send([]byte{255, 1, 0, 0})
		}
	}()
	return nil
}
func (p *rfbPeer) qemu() error {
	b, err := readBytes(p.conn, 1)
	if err != nil {
		return err
	}
	if b[0] == 0 {
		if !p.advertised[0xfffffefe] {
			return errors.New("unnegotiated physical key")
		}
		b, err = readBytes(p.conn, 10)
		if err != nil {
			return err
		}
		down := networkOrder.Uint16(b)
		if down > 1 {
			return errors.New("invalid key state")
		}
		symbol, code := networkOrder.Uint32(b[2:]), networkOrder.Uint32(b[6:])
		if mapped, ok := qnumToEvdev(code); ok {
			return p.apply(input.Event{Type: "keycode", Code: mapped, Down: down == 1})
		}
		if symbol == 0 {
			return nil
		}
		return p.apply(input.Event{Type: "key", Symbol: symbol, Down: down == 1})
	}
	if b[0] != 1 || !p.advertised[0xfffffefd] {
		return errors.New("unnegotiated QEMU extension")
	}
	b, err = readBytes(p.conn, 2)
	if err != nil {
		return err
	}
	switch networkOrder.Uint16(b) {
	case 0:
		p.audioEnabled = true
		return p.startAudio()
	case 1:
		p.audioEnabled = false
		p.stopAudio()
		return nil
	case 2:
		b, err = readBytes(p.conn, 6)
		if err != nil {
			return err
		}
		f := AudioFormat{b[0], b[1], networkOrder.Uint32(b[2:])}
		if f.Format > 5 || f.Channels < 1 || f.Channels > 2 || f.Rate < 8000 || f.Rate > 192000 {
			return errors.New("unsupported audio format")
		}
		p.stopAudio()
		p.audioFormat = f
		if p.audioEnabled {
			return p.startAudio()
		}
		return nil
	default:
		return errors.New("unknown audio operation")
	}
}
func qnumToEvdev(code uint32) (uint32, bool) {
	if code > 0 && code <= 0x58 {
		return code, true
	}
	if code >= 0x64 && code <= 0x6f {
		return 183 + code - 0x64, true
	}
	codes := map[uint32]uint32{0x70: 93, 0x73: 89, 0x79: 92, 0x7b: 94, 0x7d: 124, 0x7e: 121, 0x90: 165, 0x99: 163, 0xa0: 113, 0xa1: 140, 0xa2: 164, 0xa4: 166, 0xae: 114, 0xb0: 115, 0xb2: 172, 0xde: 116, 0xdf: 142, 0xe3: 143, 0xe5: 217, 0xe6: 156, 0xe7: 173, 0xe8: 128, 0xe9: 159, 0xea: 158, 0xeb: 157, 0xec: 155, 0xed: 226, 0x56: 86, 0x57: 87, 0x58: 88, 0x9c: 96, 0x9d: 97, 0xb5: 98, 0xb7: 99, 0xb8: 100, 0xc7: 102, 0xc8: 103, 0xc9: 104, 0xcb: 105, 0xcd: 106, 0xcf: 107, 0xd0: 108, 0xd1: 109, 0xd2: 110, 0xd3: 111, 0xc6: 119, 0xdb: 125, 0xdc: 126, 0xdd: 127}
	v, ok := codes[code]
	return v, ok
}
func latin1Text(b []byte) string {
	r := make([]rune, len(b))
	for i, v := range b {
		r[i] = rune(v)
	}
	return string(r)
}
func latin1Bytes(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 255 {
			r = '?'
		}
		b = append(b, byte(r))
	}
	return b
}
func (p *rfbPeer) watchClipboard() {
	if p.clipboard == nil {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.ctx.Done():
				return
			case text, ok := <-p.clipboard.Updates():
				if !ok {
					return
				}
				data := latin1Bytes(text)
				if len(data) > 1<<20 {
					continue
				}
				if p.offerClipboard(text) != nil {
					return
				}
			}
		}
	}()
}
func (p *rfbPeer) cutText() error {
	b, err := readBytes(p.conn, 7)
	if err != nil {
		return err
	}
	n := networkOrder.Uint32(b[3:])
	if int32(n) < 0 {
		size := -int64(int32(n))
		if size < 4 || size > 1<<20 {
			return errors.New("extended clipboard too large")
		}
		data, err := readBytes(p.conn, int(size))
		if err != nil {
			return err
		}
		return p.extendedClipboard(data)
	}
	if n > 1<<20 {
		return errors.New("clipboard too large")
	}
	b, err = readBytes(p.conn, int(n))
	if err != nil {
		return err
	}
	if p.clipboard != nil {
		return p.clipboard.Set(latin1Text(b))
	}
	return nil
}
func (p *rfbPeer) fence() error {
	b, err := readBytes(p.conn, 8)
	if err != nil {
		return err
	}
	n := int(b[7])
	if n > 64 {
		return errors.New("oversized fence")
	}
	payload, err := readBytes(p.conn, n)
	if err != nil {
		return err
	}
	flags := networkOrder.Uint32(b[3:])
	if flags&0x80000000 == 0 {
		return nil
	}
	reply := []byte{248, 0, 0, 0, 0, 0, 0, 0, byte(n)}
	networkOrder.PutUint32(reply[4:], flags&3)
	return p.send(reply, payload)
}
