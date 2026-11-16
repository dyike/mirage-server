package server

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dyike/mirage-server/internal/video"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/srtp/v3"
)

const ardMediaEncoding uint32 = 1010
const ardClock = 24000

type ardMedia struct {
	audio                 *ardAudio
	ignoreQueueFeedback   bool // diagnostic override; loss feedback remains active
	frameRate, maximumFPS int
	lastFPSAdjust         time.Time

	socket            *net.UDPConn
	offer             ardOffer
	tx, rx            *srtp.Context
	remoteIP          net.IP
	mu                sync.Mutex // address and receiver feedback; SRTP tx has only the capture writer
	remote            *net.UDPAddr
	lastFeedback      time.Time
	queueBaseline     time.Duration
	queueSeen         bool
	lastQueueLog      time.Time
	lastAdjust        time.Time
	lastLoss          time.Time
	bitrate, maximum  int
	echo              uint32
	echoAt            time.Time
	keyframe          atomic.Bool
	closed            atomic.Bool
	done              chan struct{}
	codec             string
	ssrc              uint32
	sequence          uint16
	epoch             time.Time
	packets, octets   uint32
	lastReport        time.Time
	lastFrameReport   time.Time
	reportFrames      uint64
	reportMaxSend     time.Duration
	reportMaxBytes    int
	lastSlowFrame     time.Time
	lastKeyframe      time.Time
	lastKeyframeBytes int
	keyframePending   bool
	payloader         rtp.Payloader
	pacer             videoPacer
	peakPacer         videoPacer
	wireMaximum       int
}

func (p *rfbPeer) openARDPort() (*net.UDPConn, error) { return p.openARDMediaPort(false) }
func (p *rfbPeer) openARDMediaPort(audio bool) (*net.UDPConn, error) {
	local, ok := p.conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return nil, errors.New("ARD media requires TCP addresses")
	}
	count := max(1, p.server.MaxClients)
	if p.server.MaxClients == 0 {
		count = 4
	}
	count = min(count, 64)
	var err error
	for i := 0; i < count; i++ {
		port := p.server.ARDUDPPort
		if port != 0 {
			port += i
			if audio {
				port += count
			}
		}
		var sock *net.UDPConn
		sock, err = net.ListenUDP("udp", &net.UDPAddr{IP: local.IP, Port: port, Zone: local.Zone})
		if err == nil {
			sock.SetReadBuffer(256 << 10)
			sock.SetWriteBuffer(64 << 10)
			return sock, nil
		}
	}
	return nil, err
}
func newARDMedia(socket *net.UDPConn, offer ardOffer, remote net.IP, maximum int) (*ardMedia, error) {
	tx, err := srtp.CreateContext(offer.sendKey[:32], offer.sendKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80)
	if err != nil {
		return nil, err
	}
	rx, err := srtp.CreateContext(offer.receiveKey[:32], offer.receiveKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80, srtp.SRTCPReplayProtection(128))
	if err != nil {
		return nil, err
	}
	if maximum <= 0 {
		maximum = 8000
	}
	wireMaximum := maximum
	if offer.bitrate > 0 {
		maximum = min(maximum, offer.bitrate)
	}
	// Explicit local experiment only; normal negotiation always honors the peer cap.
	if requested, err := strconv.Atoi(os.Getenv("MIRAGE_ARD_TEST_BITRATE")); err == nil && requested > 0 {
		maximum = min(wireMaximum, requested)
		log.Printf("ARD diagnostic: overriding peer bitrate with %d kbit/s (server cap %d)", maximum, wireMaximum)
	}
	var random [6]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	// Start within both negotiated caps; a fixed 2 Mbit/s startup budget
	// can hold a 4K keyframe for hundreds of milliseconds. Receiver feedback
	// still reduces the encoder and wire rate when the path is congested.
	m := &ardMedia{socket: socket, offer: offer, tx: tx, rx: rx, remoteIP: remote, maximum: maximum, bitrate: maximum, codec: offer.codec, ssrc: networkOrder.Uint32(random[:]) | 1, sequence: networkOrder.Uint16(random[4:]), epoch: time.Now(), lastAdjust: time.Now(), lastLoss: time.Now(), done: make(chan struct{})}
	m.ignoreQueueFeedback = os.Getenv("MIRAGE_ARD_IGNORE_QUEUE_FEEDBACK") == "1"
	if m.ignoreQueueFeedback {
		log.Print("ARD diagnostic: queue-only bitrate cuts disabled; loss control active")
	}
	m.wireMaximum = wireMaximum
	m.pacer.burst = 500 * time.Millisecond
	m.peakPacer.burst = time.Millisecond
	clear(m.offer.sendKey[:])
	clear(m.offer.receiveKey[:])
	clear(m.offer.audioSendKey[:])
	clear(m.offer.audioReceiveKey[:])
	if offer.codec == "hevc" {
		m.payloader = &codecs.H265Payloader{}
		if offer.tiles > 1 {
			m.payloader = &ardHEVCPayloader{don: 1}
		}
	} else {
		m.payloader = &codecs.H264Payloader{}
	}
	go m.feedback()
	return m, nil
}
func (m *ardMedia) close() {
	if m != nil && !m.closed.Swap(true) {
		m.socket.Close()
		if m.audio != nil {
			m.audio.close()
		}
		<-m.done
	}
}
func (m *ardMedia) rate() int { m.mu.Lock(); defer m.mu.Unlock(); return m.bitrate }
func (m *ardMedia) feedback() {
	defer close(m.done)
	buf := make([]byte, 2048)
	// One diagnostic per rejection class, never log packet contents or keys.
	rejected := map[string]bool{}
	reject := func(reason string) {
		if !rejected[reason] {
			rejected[reason] = true
			log.Printf("ARD UDP feedback rejected: peer=%s reason=%s", m.remoteIP, reason)
		}
	}
	for {
		n, addr, err := m.socket.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !addr.IP.Equal(m.remoteIP) || n < 12 || n == len(buf) {
			continue
		}
		plain, err := m.rx.DecryptRTCP(nil, buf[:n], nil)
		if err != nil {
			reject("SRTCP authentication")
			continue
		}
		// Validate the whole compound before learning a NAT mapping. Apple's FIR
		// is PT 192, unsupported by rtcp.Unmarshal, so split the bounded compound.
		packets, err := ardRTCPPackets(plain)
		if err != nil {
			reject("RTCP framing")
			continue
		}
		if len(packets) == 0 || len(packets[0]) < 8 || networkOrder.Uint32(packets[0][4:]) != m.offer.peerSSRC {
			reject("sender SSRC")
			continue
		}
		now := time.Now()
		m.mu.Lock()
		if m.remote != nil && m.remote.String() != addr.String() {
			m.mu.Unlock()
			continue
		}
		if m.remote == nil {
			log.Printf("ARD authenticated UDP feedback: peer=%s codec=%s", addr, m.codec)
		}
		m.remote = addr
		m.lastFeedback = now
		for _, raw := range packets {
			switch raw[1] {
			case 192:
				m.keyframe.Store(true)
			case 206: // Standard PLI/FIR are harmless additions to legacy FIR.
				if raw[0]&31 == 1 || raw[0]&31 == 4 {
					m.keyframe.Store(true)
				}
			case 200, 201:
				pkts, err := rtcp.Unmarshal(raw)
				if err != nil {
					continue
				}
				for _, pkt := range pkts {
					var reports []rtcp.ReceptionReport
					switch v := pkt.(type) {
					case *rtcp.SenderReport:
						m.echo = uint32(v.NTPTime >> 16)
						m.echoAt = now
						reports = v.Reports
					case *rtcp.ReceiverReport:
						reports = v.Reports
					}
					for _, r := range reports {
						if r.SSRC == m.ssrc {
							m.observeReceptionReport(r, now)
						}
					}
				}
			case 204:
				m.observeRateControl(raw, now)
			}
		}
		m.mu.Unlock()
	}
}

// RCTL reports the longest loss burst, not a loss fraction. One missing
// datagram needs a keyframe, but is not evidence that bitrate exceeds capacity.
// Mirage records RCTL gaps before reordering, so even a multi-packet gap can
// resolve without loss. Only RR supplies interval loss rate; use RCTL for queues.
func (m *ardMedia) observeRateControl(raw []byte, now time.Time) {
	if len(raw) != 32 || string(raw[8:12]) != "RCTL" {
		return
	}
	queue := time.Duration(networkOrder.Uint16(raw[26:])) * time.Second / 8192
	burst := networkOrder.Uint16(raw[28:]) >> 12
	// A persistent delay offset is not repeated evidence of growing queues.
	// Compare against the post-adjustment baseline so a stable receiver delay
	// cannot drive successive cuts down to the minimum bitrate.
	if !m.queueSeen || queue < m.queueBaseline {
		m.queueBaseline, m.queueSeen = queue, true
	}
	growing := queue > 80*time.Millisecond && queue-m.queueBaseline > 20*time.Millisecond
	previous := m.bitrate
	if m.maximumFPS > 0 && !m.ignoreQueueFeedback {
		if m.adjustFrameRate(now, queue, growing) {
			m.queueBaseline = queue
		}
		// Queue delay alone cannot distinguish bandwidth from receiver load.
		// Reduce capture load; RR still independently controls encoder bitrate.
		m.adjust(now, false, false)
	} else {
		m.adjust(now, false, growing && !m.ignoreQueueFeedback)
	}
	if previous != m.bitrate {
		m.queueBaseline = queue
	}
	if now.Sub(m.lastQueueLog) >= 10*time.Second {
		log.Printf("ARD receiver feedback: delay=%s baseline=%s burst-loss=%d growing=%t", queue.Round(time.Millisecond), m.queueBaseline.Round(time.Millisecond), burst, growing)
		m.lastQueueLog = now
	}
}

// Use the interval loss fraction, not a transient sequence gap, for loss cuts.
func (m *ardMedia) observeReceptionReport(r rtcp.ReceptionReport, now time.Time) {
	if r.SSRC != m.ssrc {
		return
	}
	previous := m.bitrate
	m.adjust(now, r.FractionLost > 2, false)
	if m.bitrate < previous {
		log.Printf("ARD receiver loss: fraction=%d/256 highest-sequence=%d", r.FractionLost, r.LastSequenceNumber)
	}
}

func ardRTCPPackets(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 4 || b[0]>>6 != 2 {
			return nil, errors.New("invalid RTCP")
		}
		n := (int(networkOrder.Uint16(b[2:])) + 1) * 4
		if n < 8 || n > len(b) || len(out) >= 32 {
			return nil, errors.New("invalid RTCP length")
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out, nil
}

// Authenticated receiver evidence, never a successful UDP Write, controls rate.
func (m *ardMedia) adjust(now time.Time, loss, queued bool) {
	if loss || queued {
		m.lastLoss = now
		if now.Sub(m.lastAdjust) >= time.Second {
			previous := m.bitrate
			m.bitrate = max(min(500, m.maximum), m.bitrate*2/3)
			if previous != m.bitrate {
				log.Printf("ARD rate reduced: %d -> %d kbit/s loss=%t queued=%t", previous, m.bitrate, loss, queued)
			}
			m.lastAdjust = now
		}
		return
	}
	if now.Sub(m.lastLoss) >= 10*time.Second && now.Sub(m.lastAdjust) >= 10*time.Second {
		m.bitrate = min(m.maximum, m.bitrate+max(100, m.bitrate/10))
		m.lastAdjust = now
	}
}
func ardNTP(t time.Time) uint64 {
	return uint64(t.Unix()+2208988800)<<32 | uint64(t.Nanosecond())<<32/1000000000
}
func (m *ardMedia) send(ctx context.Context, data []byte, first bool, forceKeyframe func() bool) error {
	if len(data) == 0 || len(data) > 32<<20 {
		return errors.New("invalid ARD video frame size")
	}
	m.mu.Lock()
	addr, last, rate, echo, echoAt := m.remote, m.lastFeedback, m.bitrate, m.echo, m.echoAt
	m.mu.Unlock()
	if m.closed.Load() {
		return renegotiate
	}
	if addr == nil || time.Since(last) > 3*time.Second {
		// Keep capture bounded while waiting for the existing client's first RR.
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		m.keyframe.Store(true)
		return nil
	}
	isKeyframe := ardKeyframe(m.codec, data)
	if isKeyframe {
		m.keyframe.Store(false)
	}
	if !first && !isKeyframe && !m.keyframePending && m.keyframe.Load() && time.Since(m.lastKeyframe) >= ardKeyframeInterval(m.lastKeyframeBytes, rate) {
		m.keyframe.Store(false)
		if forceKeyframe == nil || !forceKeyframe() {
			return renegotiate
		}
		m.lastKeyframe = time.Now()
		m.keyframePending = true
		log.Printf("ARD requested an in-place keyframe; capture remains running")
	}
	// Retry if an encoder accepts a request but never produces a keyframe.
	if m.keyframePending && time.Since(m.lastKeyframe) > 2*time.Second {
		m.keyframePending = false
	}
	if first {
		m.lastKeyframe = time.Now()
		m.keyframe.Store(false)
	}
	payloads := m.payloader.Payload(1150, data)
	if len(payloads) == 0 {
		return errors.New("empty ARD video access unit")
	}
	now := time.Now()
	timestamp := uint32(now.Sub(m.epoch) * ardClock / time.Second)
	if now.Sub(m.lastReport) >= time.Second {
		sr := &rtcp.SenderReport{SSRC: m.ssrc, NTPTime: ardNTP(now), RTPTime: timestamp, PacketCount: m.packets, OctetCount: m.octets}
		if echo != 0 {
			sr.Reports = []rtcp.ReceptionReport{{SSRC: m.offer.peerSSRC, LastSenderReport: echo, Delay: uint32(now.Sub(echoAt) * 65536 / time.Second)}}
		}
		raw, err := sr.Marshal()
		if err != nil {
			return err
		}
		packet, err := m.tx.EncryptRTCP(nil, raw, nil)
		if err != nil {
			return err
		}
		m.socket.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err = m.socket.WriteToUDP(packet, addr); err != nil {
			return err
		}
		m.lastReport = now
	}
	started := time.Now()
	var paced time.Duration
	defer func() {
		elapsed := time.Since(started)
		if m.lastFrameReport.IsZero() {
			m.lastFrameReport = started
		}
		m.reportFrames++
		m.reportMaxSend = max(m.reportMaxSend, elapsed)
		m.reportMaxBytes = max(m.reportMaxBytes, len(data))
		if time.Since(m.lastFrameReport) >= 10*time.Second {
			log.Printf("ARD video window: frames=%d max-frame-bytes=%d max-send=%s rate=%d kbit/s", m.reportFrames, m.reportMaxBytes, m.reportMaxSend.Round(time.Millisecond), rate)
			m.lastFrameReport = time.Now()
			m.reportFrames, m.reportMaxBytes, m.reportMaxSend = 0, 0, 0
		}
		if elapsed >= 100*time.Millisecond && time.Since(m.lastSlowFrame) >= 2*time.Second {
			m.lastSlowFrame = time.Now()
			log.Printf("ARD slow frame: bytes=%d rate=%d kbit/s send=%s pacing=%s", len(data), rate, elapsed.Round(time.Millisecond), paced.Round(time.Millisecond))
		}
	}()
	// Complete the paced access unit at the negotiated bitrate. A slow frame
	// must not resize the desktop or force TCP fallback; context cancellation
	// and socket errors still interrupt delivery.
	for i, payload := range payloads {
		waited, err := m.pacer.wait(ctx, len(payload)+22, rate)
		paced += waited
		if err != nil {
			return err
		}
		// A bounded burst finishes large frames promptly without changing
		// the negotiated sustained budget. The second bucket caps wire peaks.
		if m.wireMaximum > 0 {
			waited, err = m.peakPacer.wait(ctx, len(payload)+22, min(m.wireMaximum, rate*4))
			paced += waited
			if err != nil {
				return err
			}
		}
		if i == 0 {
			// RCTL compares the first packet arrival of each frame with its RTP
			// time. Sampling before pacing makes our own rate-limit wait look
			// like network queue growth and causes a self-reinforcing rate cut.
			timestamp = uint32(time.Since(m.epoch) * ardClock / time.Second)
		}
		packet := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 123, SequenceNumber: m.sequence, Timestamp: timestamp, SSRC: m.ssrc, Marker: i == len(payloads)-1}, Payload: payload}
		if m.codec == "hevc" {
			packet.PayloadType = 100
			if apple, ok := m.payloader.(*ardHEVCPayloader); ok {
				packet.Extension = true
				packet.ExtensionProfile = 0x9311
				extension := make([]byte, 4)
				networkOrder.PutUint16(extension, uint16(len(payloads)))
				networkOrder.PutUint16(extension[2:], apple.frame)
				if err := packet.SetExtension(0, extension); err != nil {
					return err
				}
			}
		}
		m.sequence++
		raw, err := packet.Marshal()
		if err != nil {
			return err
		}
		encrypted, err := m.tx.EncryptRTP(nil, raw, nil)
		if err != nil {
			return err
		}
		m.socket.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err = m.socket.WriteToUDP(encrypted, addr); err != nil {
			return err
		}
		m.packets++
		m.octets += uint32(len(payload))
	}
	if apple, ok := m.payloader.(*ardHEVCPayloader); ok {
		if apple.frame == 0 {
			log.Printf("ARD Apple HEVC first frame sent: peer=%s dimensions=%dx%d packets=%d DON=%d", addr, apple.width, apple.height, len(payloads), apple.don)
		}
		apple.frame++
	}
	if isKeyframe {
		m.lastKeyframe = time.Now()
		m.lastKeyframeBytes = len(data)
		m.keyframePending = false
		// Do not erase feedback received during this frame: if it was lost,
		// the pending request must still trigger recovery after the cooldown.
	}
	return nil
}

// Budget repair frames to at most half the sustained bitrate, with bounded
// recovery delay. A fixed 250ms retry can exceed the entire VPN video budget.
func ardKeyframeInterval(bytes, rate int) time.Duration {
	if rate <= 0 {
		return 250 * time.Millisecond
	}
	return min(2*time.Second, max(250*time.Millisecond, time.Duration(int64(bytes)*16)*time.Millisecond/time.Duration(rate)))
}
func ardKeyframe(codec string, data []byte) bool {
	c := video.H264
	if codec == "hevc" {
		c = video.HEVC
	}
	for _, nal := range video.SplitAnnexB(data) {
		typ := video.NALType(c, nal)
		if (c == video.H264 && typ == 5) || (c == video.HEVC && typ >= 16 && typ <= 21) {
			return true
		}
	}
	return false
}

func (m *ardMedia) configureFrameRate(fps int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maximumFPS == 0 {
		m.maximumFPS, m.frameRate = fps, fps
		m.lastFPSAdjust = time.Now()
	}
}
func (m *ardMedia) fps() int { m.mu.Lock(); defer m.mu.Unlock(); return m.frameRate }

// Called under mu. Loss control and capture load control are independent.
func (m *ardMedia) adjustFrameRate(now time.Time, queue time.Duration, growing bool) bool {
	next := m.frameRate
	if growing && now.Sub(m.lastFPSAdjust) >= 2*time.Second {
		next = max(min(15, m.maximumFPS), m.frameRate*3/4)
	} else if queue < 40*time.Millisecond && now.Sub(m.lastFPSAdjust) >= 10*time.Second {
		next = min(m.maximumFPS, m.frameRate+5)
	}
	if next == m.frameRate {
		return false
	}
	log.Printf("ARD capture rate: %d -> %d fps receiver-queue=%s", m.frameRate, next, queue.Round(time.Millisecond))
	m.frameRate = next
	m.lastFPSAdjust = now
	return true
}
