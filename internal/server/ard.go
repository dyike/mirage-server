package server

import (
	"crypto/aes"
	"errors"
	"image"
	"log"
	"math"
	"net"
	"sync/atomic"

	"github.com/dyike/mirage-server/internal/displaymeta"
	"github.com/dyike/mirage-server/internal/server/input"
)

type ardPeer struct {
	conn        *ardConn
	media       atomic.Pointer[ardMedia]
	audioSocket *net.UDPConn
	socket      *net.UDPConn // readRFB owns negotiation until it exits
	offered     bool
}

func rectHeaderEmpty(enc uint32) []byte { return rectHeader(image.Rectangle{}, enc) }
func ardName(name []byte) []byte {
	header := make([]byte, 22)
	// One independently configured encoded canvas per connection (including All Displays).
	networkOrder.PutUint32(header[2:], 1<<5)
	// Only implemented commands are advertised. Message 23 carries native
	// scroll events, so Screen Sharing need not synthesize wheel clicks.
	for _, v := range []byte{0, 2, 3, 4, 5, 6, 8, 9, 10, 13, 16, 18, 23, 28, 29, 33} {
		header[6+v/8] |= 0x80 >> (v % 8)
	}
	return append(header, name...)
}
func (a *ardPeer) close() {
	if a.audioSocket != nil {
		a.audioSocket.Close()
	}
	if m := a.media.Swap(nil); m != nil {
		m.close()
	}
	if a.socket != nil {
		a.socket.Close()
	}
}
func (p *rfbPeer) ardReply(body []byte) error {
	b := make([]byte, 2)
	networkOrder.PutUint16(b, uint16(len(body)))
	return p.send(updateMessage(append(append(rectHeaderEmpty(ardMediaEncoding), b...), body...)))
}
func (p *rfbPeer) ardRefuse() error {
	body := make([]byte, 16)
	networkOrder.PutUint16(body, 3)
	networkOrder.PutUint32(body[8:], 2)
	return p.ardReply(body)
}
func (p *rfbPeer) announceARD(offers map[uint32]bool) error {
	p.advertised[ardMediaEncoding] = offers[ardMediaEncoding]
	if offers[1104] && !p.advertised[1104] {
		p.advertised[1104] = true
		if p.server.OpenCursor != nil {
			var err error
			p.cursor, err = p.server.OpenCursor()
			if err != nil {
				return err
			}
			p.state.Lock()
			p.state.cursorActive = true
			p.state.Unlock()
			p.watchCursor()
		}
	}
	p.advertised[displaymeta.EncodingWire] = offers[displaymeta.EncodingWire]
	if offers[1105] {
		if err := p.ardLayout(); err != nil {
			return err
		}
	}
	if !offers[ardMediaEncoding] {
		if p.ard.audioSocket != nil {
			p.ard.audioSocket.Close()
			p.ard.audioSocket = nil
		}
		if m := p.ard.media.Swap(nil); m != nil {
			m.close()
		}
		if p.ard.socket != nil {
			p.ard.socket.Close()
			p.ard.socket = nil
		}
		p.ard.offered = false
		return nil
	}
	if p.ard.offered {
		return nil
	}
	p.ard.offered = true
	socket, err := p.openARDPort()
	if err != nil {
		log.Printf("ARD UDP unavailable: %v", err)
		return p.ardRefuse()
	}
	p.ard.socket = socket
	body := make([]byte, 36)
	if p.server.ARDAudio != nil {
		audioSocket, e := p.openARDMediaPort(true)
		if e != nil {
			log.Printf("ARD audio port unavailable: %v", e)
		} else {
			p.ard.audioSocket = audioSocket
			networkOrder.PutUint16(body[8:], uint16(audioSocket.LocalAddr().(*net.UDPAddr).Port))
			networkOrder.PutUint32(body[10:], 1)
		}
	}
	networkOrder.PutUint16(body, 1)
	networkOrder.PutUint16(body[2:], 1)
	networkOrder.PutUint16(body[14:], uint16(socket.LocalAddr().(*net.UDPAddr).Port))
	networkOrder.PutUint32(body[16:], 1)
	return p.ardReply(body)
}
func (p *rfbPeer) ardOffer() error {
	h, err := readBytes(p.conn, 3)
	if err != nil {
		return err
	}
	n := int(networkOrder.Uint16(h[1:]))
	if n < 124 {
		return errARDOffer
	}
	body, err := readBytes(p.conn, n)
	if err != nil {
		return err
	}
	message := append(append([]byte{28}, h...), body...)
	defer clear(message)
	if !p.ard.offered {
		return p.ardRefuse()
	}
	offer, err := parseARDOffer(message, p.server.PreferHEVC)
	if err != nil {
		log.Printf("ARD media offer rejected: peer=%s error=%v", p.conn.RemoteAddr(), err)
		return p.ardRefuse()
	}
	log.Printf("ARD media negotiation: peer=%s codec=%s requested-tiles=%d selected-tiles=1", p.conn.RemoteAddr(), offer.codec, offer.tiles)
	log.Printf("ARD bandwidth offer: mode=%d selected=%d kbit/s tiers:%s", offer.mode, offer.bitrate, offer.bandwidthSummary)
	defer clear(offer.sendKey[:])
	defer clear(offer.receiveKey[:])
	defer clear(offer.audioSendKey[:])
	defer clear(offer.audioReceiveKey[:])
	// The existing client renegotiates lower bitrate on the SAME advertised
	// port, with fresh SRTP keys. A new ports reply is not required.
	if old := p.ard.media.Swap(nil); old != nil {
		address := old.socket.LocalAddr().(*net.UDPAddr)
		var audioAddress *net.UDPAddr
		if old.audio != nil {
			audioAddress = old.audio.socket.LocalAddr().(*net.UDPAddr)
		}
		old.close()
		if audioAddress != nil {
			p.ard.audioSocket, err = net.ListenUDP("udp", audioAddress)
			if err != nil {
				return p.ardRefuse()
			}
		}
		p.ard.socket, err = net.ListenUDP("udp", address)
		if err != nil {
			log.Printf("ARD renegotiation could not reopen media port: %v", err)
			return p.ardRefuse()
		}
		p.ard.socket.SetReadBuffer(256 << 10)
		p.ard.socket.SetWriteBuffer(64 << 10)
		log.Printf("ARD accepting client media renegotiation: codec=%s cap=%d kbit/s", offer.codec, offer.bitrate)
	}
	if p.ard.socket == nil {
		return p.ardRefuse()
	}
	remote, ok := p.conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return p.ardRefuse()
	}
	media, err := newARDMedia(p.ard.socket, offer, remote.IP, p.server.VideoBitrate)
	if err != nil {
		return p.ardRefuse()
	}
	p.ard.socket = nil
	if p.ard.audioSocket != nil && offer.audioPeerSSRC != 0 && p.server.ARDAudio != nil {
		media.audio, err = newARDAudio(p.ctx, p.ard.audioSocket, offer, remote.IP, p.server.ARDAudio)
		if err != nil {
			media.close()
			return p.ardRefuse()
		}
		p.ard.audioSocket = nil
	}
	p.state.Lock()
	size := p.state.desktop.Size
	p.state.Unlock()
	if apple, ok := media.payloader.(*ardHEVCPayloader); ok {
		apple.width = uint16(size.X)
		apple.height = uint16(size.Y)
	}
	if err = p.ardReply(ardAnswerForOffer(media.ssrc, size.X, size.Y, p.server.VideoFPS, offer)); err != nil {
		media.close()
		return err
	}
	p.ard.media.Store(media)
	p.state.Lock()
	// MediaStream is a pseudo-encoding and may follow Zlib in SetEncodings.
	// A completed media offer, not that list order, activates GPU capture.
	p.state.encoding = ardMediaEncoding
	p.state.invalidateCaptureLocked()
	p.state.Unlock()
	p.wakeCapture()
	log.Printf("ARD SRTP negotiated codec=%s port=%d bitrate=%d kbit/s", media.codec, media.socket.LocalAddr().(*net.UDPAddr).Port, media.rate())
	return nil
}
func (p *rfbPeer) wakeCapture() {
	select {
	case p.requests <- struct{}{}:
	default:
	}
}
func (p *rfbPeer) ardLayout() error {
	p.write.Lock()
	defer p.write.Unlock()
	return p.ardLayoutLocked()
}

func (p *rfbPeer) ardLayoutLocked() error {
	p.state.Lock()
	d := p.state.desktop.clone()
	base, scale := p.state.ardScaleBase.clone(), p.state.ardScale
	// Each new layout needs the viewer to re-arm its framebuffer receiver.
	// Do not push new pixels while native Screen Sharing is rebuilding its canvas.
	if p.state.ardReady == nil || p.state.ardLayoutRevision != p.state.revision {
		p.state.ardReady = make(chan struct{})
		p.state.ardAwaitingRefresh = true
		p.state.ardLayoutRevision = p.state.revision
	}
	p.state.Unlock()
	b := ardLayoutBody(d, p.server.AvailableScreens)
	if !p.advertised[ardMediaEncoding] {
		b = ardPixelLayoutBody(d, p.server.AvailableScreens)
		if scale > 0 && base.Size.X > 0 {
			b = ardScaledLayoutBody(base, d, p.server.AvailableScreens, scale)
		}
	}
	log.Printf("ARD layout: peer=%s current=%d points=%dx%d framebuffer=%dx%d media=%t", p.conn.RemoteAddr(), networkOrder.Uint32(b[10:]), networkOrder.Uint16(b[2:]), networkOrder.Uint16(b[4:]), d.Size.X, d.Size.Y, p.advertised[ardMediaEncoding])
	if p.advertised[displaymeta.EncodingWire] {
		var err error
		b, err = displaymeta.Append(b, displaymeta.Metadata{Names: p.server.DisplayNames, SingleDisplayOnly: false})
		if err != nil {
			return err
		}
	}
	prefix := make([]byte, 2)
	networkOrder.PutUint16(prefix, uint16(len(b)))
	layout := updateMessage(append(append(rectHeader(image.Rectangle{Max: d.Size}, 1105), prefix...), b...))
	_, err := p.sendPacedLocked(0, layout)
	return err
}

// Returns handled=false only for ordinary RFB messages.
func (p *rfbPeer) readARD(t byte) (bool, error) {
	switch t {
	case 8: // Existing clients can request a scale. Report the actual geometry.
		b, err := readBytes(p.conn, 9)
		if err != nil {
			return true, err
		}
		scale := math.Float64frombits(networkOrder.Uint64(b[1:]))
		if !(scale > 0 && scale <= 1) {
			return true, errors.New("invalid ARD scale")
		}
		log.Printf("ARD scale request: peer=%s scale=%g", p.conn.RemoteAddr(), scale)
		p.write.Lock()
		defer p.write.Unlock()
		if !p.advertised[ardMediaEncoding] {
			p.setARDScale(scale)
		}
		return true, p.ardLayoutLocked()
	case 9:
		b, err := readBytes(p.conn, 15)
		if err != nil {
			return true, err
		}
		p.state.Lock()
		awaiting := p.state.ardAwaitingRefresh
		p.state.continuous = networkOrder.Uint32(b[3:]) != 0xffffffff
		if p.state.continuous {
			p.state.armARDFrameLocked()
		}
		enabled := p.state.continuous
		p.state.Unlock()
		if awaiting {
			log.Printf("ARD auto update: peer=%s enabled=%t value=%d region=%dx%d", p.conn.RemoteAddr(), enabled, networkOrder.Uint32(b[3:]), networkOrder.Uint16(b[11:]), networkOrder.Uint16(b[13:]))
		}
		p.wakeCapture()
		return true, nil
	case 10:
		_, err := readBytes(p.conn, 3)
		return true, err
	case 13:
		b, err := readBytes(p.conn, 7)
		if err != nil {
			return true, err
		}
		id := ardSelectedDisplay(b)
		log.Printf("ARD display selection: peer=%s combine=%t wire-id=%d selected=%d", p.conn.RemoteAddr(), b[0] != 0, networkOrder.Uint32(b[3:]), id)
		p.write.Lock()
		defer p.write.Unlock()
		if len(p.server.AvailableScreens) > 0 {
			p.state.Lock()
			revision := p.state.revision
			p.state.Unlock()
			if err := p.selectNativeDisplay(id); err != nil {
				return true, err
			}
			p.state.Lock()
			unchanged := p.state.revision == revision
			p.state.Unlock()
			if unchanged {
				return true, nil
			} // avoid layout/select feedback loops
		} else {
			p.state.Lock()
			valid := id == 0 || id == 0xffffffff
			for _, s := range p.state.desktop.Screens {
				valid = valid || s.ID == id
			}
			p.state.Unlock()
			if !valid {
				return true, errors.New("unknown ARD display")
			}
		}
		return true, p.ardLayoutLocked()
	case 16:
		b, err := readBytes(p.conn, 17)
		if err != nil {
			return true, err
		}
		block, _ := aes.NewCipher(p.ard.conn.key[:])
		block.Decrypt(b[1:], b[1:])
		v := b[1:]
		if v[0] == 255 {
			if v[1] > 1 {
				return true, errors.New("invalid ARD key state")
			}
			symbol := networkOrder.Uint32(v[2:])
			if symbol == 0xffe7 {
				symbol = 0xffe9
			}
			if symbol == 0xffe8 {
				symbol = 0xffea
			}
			return true, p.apply(input.Event{Type: "key", Symbol: symbol, Down: v[1] == 1})
		}
		if v[10] != 255 {
			return true, errors.New("invalid ARD pointer event")
		}
		return true, p.pointer(uint16(v[11]), int(networkOrder.Uint16(v[12:])), int(networkOrder.Uint16(v[14:])))
	case 23:
		return true, p.ardEvent()
	case 28:
		return true, p.ardOffer()
	case 29:
		return true, p.ardConfigureDisplay()
	}
	return false, nil
}
