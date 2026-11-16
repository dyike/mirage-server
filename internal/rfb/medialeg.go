package rfb

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// mediaLeg is one UDP media stream: SRTP from the server, SRTCP both ways.
// The socket binds the same port number the server named, as Apple's
// viewer does, and connects to it.
type mediaLeg struct {
	conn         net.Conn
	rx           *srtp.Context // server to viewer
	tx           *srtp.Context // viewer to server (RTCP); txMu
	txMu         sync.Mutex
	ssrc         uint32 // ours, for reports
	measureClock bool
	clockSent    [16]struct {
		stamp uint32
		at    time.Time
	}
	clockNext int
	// Shared with the client so accounting survives this leg being replaced.
	received *atomic.Int64

	packets, bytes, rtcpIn, bad atomic.Int64
	refused, sendErrs           atomic.Int64
	arrived                     atomic.Bool // a picture was decoded

	statsMu  sync.Mutex // the reader records, the feedback loop reports
	sources  map[uint32]*reception
	rate     rateFeedback
	err      atomic.Value // string: why run stopped
	onPacket func(h *rtp.Header, payload []byte)
	// Both callbacks run on the reader goroutine, like onPacket.
	readDeadline func() time.Time
	onTimeout    func(time.Time)
}

func newSRTP(key []byte) (*srtp.Context, error) {
	return srtp.CreateContext(key[:32], key[32:46], srtp.ProtectionProfileAes256CmHmacSha1_80)
}

func openMediaLeg(dialer Dialer, local, remote netip.Addr, port uint16, rxKey, txKey []byte, ssrc uint32) (*mediaLeg, error) {
	rx, err := newSRTP(rxKey)
	if err != nil {
		return nil, err
	}
	tx, err := newSRTP(txKey)
	if err != nil {
		return nil, err
	}
	if dialer == nil {
		dialer = systemDialer{}
	}
	conn, err := dialer.DialUDP(netip.AddrPortFrom(local, port), netip.AddrPortFrom(remote, port))
	if err != nil {
		return nil, err
	}
	if b, ok := conn.(interface{ SetReadBuffer(int) error }); ok {
		b.SetReadBuffer(4 << 20)
	}
	return &mediaLeg{conn: conn, rx: rx, tx: tx, ssrc: ssrc}, nil
}

// systemDialer uses the system's network.
type systemDialer struct{}

func (systemDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// DialUDP binds with the port reusable (see reuseMediaPort).
func (systemDialer) DialUDP(local, remote netip.AddrPort) (net.Conn, error) {
	d := net.Dialer{
		LocalAddr: net.UDPAddrFromAddrPort(local),
		Control: func(_, _ string, raw syscall.RawConn) error {
			var serr error
			err := raw.Control(func(fd uintptr) {
				serr = reuseMediaPort(fd)
			})
			if err != nil {
				return err
			}
			return serr
		},
	}
	return d.DialContext(context.Background(), "udp", remote.String())
}

// sendRTCP protects and sends one compound RTCP packet.
func (l *mediaLeg) sendRTCP(pkts ...rtcp.Packet) error {
	raw, err := rtcp.Marshal(pkts)
	if err != nil {
		return err
	}
	return l.sendRaw(raw)
}

// requestKeyframe asks for a keyframe: AVConference takes a legacy FIR
// (payload type 192 with only the sender's SSRC) after a receiver report,
// and rejects a datagram that starts with the FIR.
func (l *mediaLeg) requestKeyframe() error {
	raw, err := rtcp.Marshal([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: l.ssrc}})
	if err != nil {
		return err
	}
	return l.sendRaw(be.AppendUint32(append(raw, 0x80, 192, 0, 1), l.ssrc))
}

func (l *mediaLeg) sendRaw(raw []byte) error {
	l.txMu.Lock()
	defer l.txMu.Unlock()
	out, err := l.tx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		return err
	}
	_, err = l.conn.Write(out)
	return err
}

// run reads until the socket closes; a second goroutine sends feedback.
func (l *mediaLeg) run(done <-chan struct{}) {
	l.sources = map[uint32]*reception{}
	l.rate.epoch = time.Now()
	go l.feedback(done)
	buf := make([]byte, 65536)
	var h rtp.Header
	var deadline time.Time
	for {
		if l.readDeadline != nil {
			if next := l.readDeadline(); !next.Equal(deadline) {
				if err := l.conn.SetReadDeadline(next); err != nil {
					l.err.Store(err.Error())
					return
				}
				deadline = next
			}
		}
		n, err := l.conn.Read(buf)
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			// The server's port may refuse our first reports before it
			// opens; a connected UDP socket then reports ECONNREFUSED.
			if errors.Is(err, syscall.ECONNREFUSED) {
				l.refused.Add(1)
				continue
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if l.onTimeout != nil {
					l.onTimeout(time.Now())
				}
				continue
			}
			l.err.Store(err.Error())
			return
		}
		if l.received != nil {
			l.received.Add(int64(n))
		}
		pkt := buf[:n]
		now := time.Now()
		if n >= 2 && pkt[1] >= 192 && pkt[1] <= 223 {
			l.rtcpIn.Add(1)
			l.senderReports(pkt, now)
			continue
		}
		plain, err := l.rx.DecryptRTP(nil, pkt, &h)
		if err != nil {
			l.bad.Add(1)
			continue
		}
		l.packets.Add(1)
		l.bytes.Add(int64(n))
		l.statsMu.Lock()
		src := l.sources[h.SSRC]
		if src == nil {
			if len(l.sources) >= 16 {
				clear(l.sources)
			}
			src = &reception{}
			l.sources[h.SSRC] = src
		}
		src.record(h.SequenceNumber, h.Timestamp, now)
		l.rate.record(h.SequenceNumber, h.Timestamp, now)
		l.statsMu.Unlock()
		if l.onPacket != nil {
			payload := plain[h.MarshalSize():]
			if h.Padding && len(payload) > 0 {
				payload = payload[:max(0, len(payload)-int(payload[len(payload)-1]))]
			}
			l.onPacket(&h, payload)
		}
	}
}

// senderReports notes each sender report's time, which the next receiver
// report echoes so the server can measure the round trip.
func (l *mediaLeg) senderReports(pkt []byte, now time.Time) {
	plain, err := l.rx.DecryptRTCP(nil, pkt, nil)
	if err != nil {
		return
	}
	pkts, err := rtcp.Unmarshal(plain)
	if err != nil {
		return
	}
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	if l.sources == nil {
		l.sources = make(map[uint32]*reception)
	}
	for _, p := range pkts {
		if sr, ok := p.(*rtcp.SenderReport); ok {
			src := l.sources[sr.SSRC]
			if src == nil && len(l.sources) < 16 {
				src = &reception{}
				l.sources[sr.SSRC] = src
			}
			if src != nil {
				src.lastSR, src.lastSRAt = uint32(sr.NTPTime>>16), now
				src.clock.report(sr.NTPTime, sr.RTPTime, now)
				for _, report := range sr.Reports {
					if report.SSRC != l.ssrc || report.LastSenderReport == 0 {
						continue
					}
					for _, sent := range l.clockSent {
						if sent.stamp == report.LastSenderReport && !sent.at.IsZero() {
							src.clock.calibrate(sr.NTPTime, sent.at, now, report.Delay)
							break
						}
					}
				}
			}
		}
	}
}

// feedback sends the rate feedback every 50 ms and a receiver report every
// second.
func (l *mediaLeg) feedback(done <-chan struct{}) {
	t := time.NewTicker(feedbackInterval)
	defer t.Stop()
	var lastReport time.Time
	for {
		now := time.Now()
		l.statsMu.Lock()
		l.rate.videoArrived = l.arrived.Load()
		rctl := l.rate.packet(l.ssrc, now)
		var rr *rtcp.ReceiverReport
		if now.Sub(lastReport) >= time.Second {
			lastReport = now
			rr = &rtcp.ReceiverReport{SSRC: l.ssrc}
			for ssrc, src := range l.sources {
				if now.Sub(src.lastPacketAt) <= 5*time.Second && len(rr.Reports) < 8 {
					rr.Reports = append(rr.Reports, src.report(ssrc, now))
				}
			}
		}
		l.statsMu.Unlock()
		if rctl != nil && l.sendRaw(rctl) != nil {
			l.sendErrs.Add(1)
		}
		if rr != nil {
			if l.sendReceptionReport(rr) != nil {
				l.sendErrs.Add(1)
			}
		}
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}

func (l *mediaLeg) sendReceptionReport(rr *rtcp.ReceiverReport) error {
	if !l.measureClock {
		return l.sendRTCP(rr)
	}
	now := time.Now()
	stamp := ntp64(now)
	l.statsMu.Lock()
	l.clockSent[l.clockNext] = struct {
		stamp uint32
		at    time.Time
	}{uint32(stamp >> 16), now}
	l.clockNext = (l.clockNext + 1) % len(l.clockSent)
	l.statsMu.Unlock()
	// There is no outgoing video: packet/octet counts remain zero. macOS
	// still echoes this advertised receiver SSRC's SR in its next report.
	return l.sendRTCP(&rtcp.SenderReport{SSRC: l.ssrc, NTPTime: stamp, RTPTime: uint32(now.Sub(l.rate.epoch) * videoClock / time.Second), Reports: rr.Reports},
		&rtcp.SourceDescription{Chunks: []rtcp.SourceDescriptionChunk{{Source: l.ssrc, Items: []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: "mirage"}}}}})
}

func (l *mediaLeg) close() { l.conn.Close() }
