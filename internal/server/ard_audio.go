package server

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"log"
	"net"
	"sync"
	"time"
)

// Audio owns a separate socket, SRTP contexts and capture worker; video pacing
// must never block 10ms audio packets. Only authenticated RTCP learns the peer.
type ardAudio struct {
	socket *net.UDPConn
	tx, rx *srtp.Context
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	remote *net.UDPAddr
	seen   time.Time
}

func newARDAudio(ctx context.Context, socket *net.UDPConn, o ardOffer, ip net.IP, source func(context.Context, func([]byte) error) error) (*ardAudio, error) {
	tx, err := srtp.CreateContext(o.audioSendKey[:32], o.audioSendKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80)
	if err != nil {
		return nil, err
	}
	rx, err := srtp.CreateContext(o.audioReceiveKey[:32], o.audioReceiveKey[32:], srtp.ProtectionProfileAes256CmHmacSha1_80, srtp.SRTCPReplayProtection(128))
	if err != nil {
		return nil, err
	}
	var seed [6]byte
	if _, err = rand.Read(seed[:]); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	a := &ardAudio{socket: socket, tx: tx, rx: rx, cancel: cancel, done: make(chan struct{})}
	peerSSRC := o.audioPeerSSRC
	clear(o.audioSendKey[:])
	clear(o.audioReceiveKey[:])
	clear(o.sendKey[:])
	clear(o.receiveKey[:])
	ready := make(chan struct{})
	var readyOnce sync.Once
	feedbackDone := make(chan struct{})
	go func() {
		defer close(feedbackDone)
		buf := make([]byte, 2048)
		for {
			socket.SetReadDeadline(time.Now().Add(time.Second))
			n, addr, err := socket.ReadFromUDP(buf)
			if err != nil {
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() && ctx.Err() == nil {
					a.mu.Lock()
					expired := a.remote != nil && time.Since(a.seen) > 3*time.Second
					a.mu.Unlock()
					if !expired {
						continue
					}
					cancel()
				}
				return
			}
			if !addr.IP.Equal(ip) {
				continue
			}
			plain, err := rx.DecryptRTCP(nil, buf[:n], nil)
			if err != nil {
				continue
			}
			packets, err := ardRTCPPackets(plain)
			if err != nil || len(packets) == 0 || networkOrder.Uint32(packets[0][4:]) != peerSSRC {
				continue
			}
			a.mu.Lock()
			if a.remote == nil || a.remote.String() == addr.String() {
				a.remote = addr
				a.seen = time.Now()
				readyOnce.Do(func() { close(ready) })
			}
			a.mu.Unlock()
		}
	}()
	go func() {
		defer close(a.done)
		defer func() { cancel(); socket.Close(); <-feedbackDone }()
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
		ssrc := networkOrder.Uint32(seed[:]) | 1
		seq := networkOrder.Uint16(seed[4:])
		var timestamp, packets, octets uint32
		var lastReport time.Time
		err := source(ctx, func(data []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(data) == 0 || len(data) > 1150 {
				return errors.New("invalid AAC-ELD RTP frame size")
			}
			ts := timestamp
			timestamp += 480
			a.mu.Lock()
			addr, seen := a.remote, a.seen
			a.mu.Unlock()
			if addr == nil || time.Since(seen) > 3*time.Second {
				return errors.New("audio receiver feedback timed out")
			}
			now := time.Now()
			socket.SetWriteDeadline(now.Add(50 * time.Millisecond))
			if now.Sub(lastReport) >= time.Second {
				raw, err := (&rtcp.SenderReport{SSRC: ssrc, NTPTime: ardNTP(now), RTPTime: ts, PacketCount: packets, OctetCount: octets}).Marshal()
				if err != nil {
					return err
				}
				encrypted, err := tx.EncryptRTCP(nil, raw, nil)
				if err != nil {
					return err
				}
				if _, err = socket.WriteToUDP(encrypted, addr); err != nil {
					return err
				}
				lastReport = now
			}
			raw, err := (&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 101, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Marker: true}, Payload: data}).Marshal()
			if err != nil {
				return err
			}
			seq++
			encrypted, err := tx.EncryptRTP(nil, raw, nil)
			if err != nil {
				return err
			}
			if _, err = socket.WriteToUDP(encrypted, addr); err != nil {
				return err
			}
			packets++
			octets += uint32(len(data))
			if packets == 1 {
				log.Printf("ARD audio sending AAC-ELD: 48000 Hz stereo, port=%d", socket.LocalAddr().(*net.UDPAddr).Port)
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			log.Printf("ARD audio stopped: %v", err)
		}
	}()
	return a, nil
}
func (a *ardAudio) close() { a.cancel(); a.socket.Close(); <-a.done }
