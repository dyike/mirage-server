package rfb

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// macOS Screen Sharing's high-performance mode streams the screen as HEVC
// or H.264 over SRTP, negotiated through AVConference. The client turns on
// the MediaStream encoding; the server answers with a pseudo-rectangle
// naming its UDP ports; the client sends an offer (message 0x1c) holding
// SRTP master keys and, per stream, a binary property list around a
// zlib-compressed protobuf "media blob"; the server answers or refuses.
//
// Field layouts and the values a native offer carries follow the
// open-screen-sharing project's notes on ScreenSharing.app and
// AVConference.

const (
	msgAppleMediaStream = 0x1c
	mediaKeyLen         = 46 // SRTP master key (32) and salt (14)

	negotiatorRemoteMic     = 4
	negotiatorRemoteDesktop = 7
	negotiatorSystemAudio   = 8
	negotiatorNearby        = 9

	mediaFlag60FPS    = 1 << 0
	mediaFlagNoCursor = 1 << 2

	payloadHEVC  = 100
	payloadAudio = 101 // AAC-ELD
	payloadH264  = 123

	maxVideoBitrate = 60_000_000
	nearbyBitrate   = 40_000_000
)

// Offer holds one negotiation attempt's identities and keys.
type mediaOffer struct {
	mode                 int64 // negotiatorNearby or negotiatorRemoteDesktop
	codecs               []int // payload types, preferred first
	fps60                bool
	maxBitrate           uint64 // bit/s caps in the bandwidth settings; 0 none
	session              [16]byte
	audioCall, videoCall string
	audioSend, audioRecv [mediaKeyLen]byte
	videoSend, videoRecv [mediaKeyLen]byte // videoSend: server to client
	audioSSRC, videoSSRC uint32
}

func newMediaOffer(mode int64, codecs []int, fps60 bool) *mediaOffer {
	o := &mediaOffer{mode: mode, codecs: codecs, fps60: fps60}
	for _, b := range [][]byte{o.session[:], o.audioSend[:], o.audioRecv[:], o.videoSend[:], o.videoRecv[:]} {
		rand.Read(b)
	}
	uuid4(&o.session)
	o.audioCall, o.videoCall = newUUID(), newUUID()
	o.audioSSRC, o.videoSSRC = nonzeroRandom(), nonzeroRandom()
	return o
}

func uuid4(id *[16]byte) {
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
}

func newUUID() string {
	var id [16]byte
	rand.Read(id[:])
	uuid4(&id)
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]))
}

func nonzeroRandom() uint32 {
	for {
		var b [4]byte
		rand.Read(b[:])
		if v := be.Uint32(b[:]); v != 0 {
			return v
		}
	}
}

// ntp64 is t as a 64-bit NTP timestamp.
func ntp64(t time.Time) uint64 {
	const epoch1900 = 2208988800
	return uint64(t.Unix()+epoch1900)<<32 | uint64(t.Nanosecond())<<32/1e9
}

// message builds client message 0x1c: a 0x80-byte header (version 3, flags,
// the two offers' lengths, a session UUID, the audio keys), the audio
// offer, the video keys and the video offer.
func (o *mediaOffer) message() []byte {
	created := ntp64(time.Now())
	audioMode := int64(negotiatorSystemAudio)
	audio := o.streamOffer(audioMode, o.audioCall, o.audioBlob(created))
	video := o.streamOffer(o.mode, o.videoCall, o.videoBlob(created))
	m := make([]byte, 0x80, 0x80+len(audio)+2*mediaKeyLen+len(video))
	m[0] = msgAppleMediaStream
	be.PutUint16(m[4:], 3)
	flags := uint32(mediaFlagNoCursor) // the viewer draws the pointer
	if o.fps60 {
		flags |= mediaFlag60FPS
	}
	// Unlike the surrounding network-order fields, the daemon reads and
	// forwards this flags word in host order to ScreensharingAgent.
	binary.LittleEndian.PutUint32(m[6:], flags)
	be.PutUint16(m[0x0a:], uint16(len(audio)))
	be.PutUint16(m[0x0c:], uint16(len(video)))
	copy(m[0x14:], o.session[:])
	copy(m[0x24:], o.audioSend[:])
	copy(m[0x52:], o.audioRecv[:])
	m = append(m, audio...)
	m = append(m, o.videoSend[:]...)
	m = append(m, o.videoRecv[:]...)
	m = append(m, video...)
	be.PutUint16(m[2:], uint16(len(m)-4))
	return m
}

// streamOffer wraps a media blob the way AVConference does: a binary plist
// with the endpoint's identity, the negotiator mode and the compressed blob.
func (o *mediaOffer) streamOffer(mode int64, call string, blob []byte) []byte {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(blob)
	w.Close()
	var endpoint pbuf
	endpoint.uint(1, 0)
	endpoint.uint(2, 1)
	endpoint.bytes(3, []byte("Mac14,3"))  // device model
	endpoint.bytes(4, []byte("2215.5.1")) // AVConference version
	endpoint.bytes(5, []byte("25G72"))    // OS build
	d := []plistEntry{
		{"avcMediaStreamOptionRemoteEndpointInfo", []byte(endpoint)},
		{"avcMediaStreamNegotiatorMode", mode},
		{"avcMediaStreamNegotiatorMediaBlob", z.Bytes()},
		{"avcMediaStreamOptionCallID", call},
	}
	if mode == negotiatorNearby {
		d = append(d, plistEntry{"avcMediaStreamNegotiatorDirection", int64(2)})
	}
	return binaryPlist(d)
}

func (o *mediaOffer) blobTail(b *pbuf, created uint64, video bool) {
	b.bytes(6, []byte("Viceroy 1.7.0"))
	b.uint(8, 0)
	// Bandwidth settings as a native offer carries them.
	for _, e := range [][3]uint64{
		{4074, 0, 16384}, {0, 20000000, 98304}, {0, 40000000, 12288},
		{0, 100000000, 1048576}, {0, 6000000, 131072}, {1, 299, 0},
		{16, 4100, 0}, {0, maxVideoBitrate, 262144}, {4, 6500, 0},
		{0, 75000000, 524288},
	} {
		if video && o.mode == negotiatorNearby && e[2] == 12288 {
			e[1], e[2] = nearbyBitrate, 4096
		}
		// Kind 0 entries carry bit rates. The server keeps under the cap
		// but does not adapt below it to a slower link by itself.
		if o.maxBitrate > 0 && e[0] == 0 {
			e[1] = min(e[1], o.maxBitrate)
		}
		var item pbuf
		item.uint(1, e[0])
		item.uint(2, e[1])
		if e[2] != 0 {
			item.uint(3, e[2])
		}
		b.bytes(9, item)
	}
	b.uint(13, created)
	b.uint(14, 2) // blob version
	b.uint(16, 0)
	b.uint(18, 1) // access network: viewer
}

func (o *mediaOffer) audioBlob(created uint64) []byte {
	var s pbuf
	s.uint(1, uint64(o.audioSSRC))
	s.uint(2, 0)
	s.uint(3, 0)
	s.uint(4, 0x5e7f)
	s.uint(5, 0)
	s.uint(6, 0)
	var b pbuf
	b.uint(1, 1)
	b.uint(2, 1)
	b.bytes(3, s)
	o.blobTail(&b, created, false)
	return b
}

func (o *mediaOffer) videoBlob(created uint64) []byte {
	var s pbuf
	s.uint(1, uint64(o.videoSSRC))
	s.uint(2, 0)
	for _, pt := range o.codecs {
		var p pbuf
		p.uint(1, uint64(pt))
		for _, op := range []uint64{1, 2} { // encode, decode
			var rule pbuf
			rule.uint(1, 1)      // transport
			rule.uint(2, op)     // operation
			rule.uint(3, 0xc3c3) // screen formats
			rule.uint(4, 0)
			p.bytes(2, rule)
		}
		ar := "AR:16/9,5/8;XR:16/9,5/8;"
		if pt == payloadHEVC {
			p.bytes(3, []byte("FLS;LF:-1;POS:5;EOD:1;HTS:2;RR:3;POSE:4;SW:0;"+ar))
			p.uint(4, 14) // parameter sets
		} else {
			p.bytes(3, []byte("FLS;MS:-1;LF:-1;LTR;CABAC;POS:0;EOD:1;HTS:2;RR:3;SW:4;"+ar))
			p.uint(4, 1)
		}
		s.bytes(3, p)
	}
	s.uint(6, 1) // tiles per frame
	s.uint(7, 1)
	s.uint(8, 3) // pixel formats: 420f, 420v
	s.uint(9, 1) // HDR modes: standard
	s.uint(12, 1)
	var b pbuf
	b.uint(1, 1)
	b.uint(2, 1)
	b.bytes(5, s)
	o.blobTail(&b, created, true)
	return b
}

// pbuf builds a protobuf message.
type pbuf []byte

func (p *pbuf) varint(v uint64) {
	for v >= 0x80 {
		*p = append(*p, byte(v)|0x80)
		v >>= 7
	}
	*p = append(*p, byte(v))
}

func (p *pbuf) uint(field int, v uint64) {
	p.varint(uint64(field) << 3)
	p.varint(v)
}

func (p *pbuf) bytes(field int, b []byte) {
	p.varint(uint64(field)<<3 | 2)
	p.varint(uint64(len(b)))
	*p = append(*p, b...)
}

// --- binary property lists (bplist00) ---

type plistEntry struct {
	key   string
	value any // []byte, int64 or ASCII string
}

// binaryPlist encodes a dictionary of ASCII keys to data, integers and
// ASCII strings.
func binaryPlist(entries []plistEntry) []byte {
	n := 1 + 2*len(entries) // dictionary, keys, values
	out := []byte("bplist00")
	offsets := make([]int, 0, n)
	marker := func(kind byte, length int) {
		if length < 15 {
			out = append(out, kind|byte(length))
			return
		}
		out = append(out, kind|0x0f)
		out = appendPlistInt(out, int64(length))
	}
	offsets = append(offsets, len(out))
	marker(0xd0, len(entries))
	for i := range entries {
		out = append(out, byte(1+i))
	}
	for i := range entries {
		out = append(out, byte(1+len(entries)+i))
	}
	for _, e := range entries {
		offsets = append(offsets, len(out))
		marker(0x50, len(e.key))
		out = append(out, e.key...)
	}
	for _, e := range entries {
		offsets = append(offsets, len(out))
		switch v := e.value.(type) {
		case []byte:
			marker(0x40, len(v))
			out = append(out, v...)
		case int64:
			out = appendPlistInt(out, v)
		case string:
			marker(0x50, len(v))
			out = append(out, v...)
		}
	}
	table := len(out)
	size := 1
	for table >= 1<<(8*size) {
		size *= 2
	}
	for _, off := range offsets {
		for i := size - 1; i >= 0; i-- {
			out = append(out, byte(off>>(8*i)))
		}
	}
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = byte(size), 1 // offset size, object reference size
	be.PutUint64(trailer[8:], uint64(n))
	be.PutUint64(trailer[16:], 0) // top object
	be.PutUint64(trailer[24:], uint64(table))
	return append(out, trailer...)
}

func appendPlistInt(b []byte, v int64) []byte {
	switch {
	case v >= 0 && v <= math.MaxUint8:
		return append(b, 0x10, byte(v))
	case v >= 0 && v <= math.MaxUint16:
		return be.AppendUint16(append(b, 0x11), uint16(v))
	case v >= 0 && v <= math.MaxUint32:
		return be.AppendUint32(append(b, 0x12), uint32(v))
	default:
		return be.AppendUint64(append(b, 0x13), uint64(v))
	}
}
