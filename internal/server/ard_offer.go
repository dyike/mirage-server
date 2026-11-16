package server

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

var errARDPlistMissing = errors.New("missing ARD plist key")

var errARDOffer = errors.New("invalid ARD media offer")

type ardOffer struct {
	videoPayload                  []byte // selected codec descriptor; never contains key material
	tiles                         uint64
	audioSendKey, audioReceiveKey [46]byte
	audioPeerSSRC                 uint32
	sendKey, receiveKey           [46]byte
	peerSSRC                      uint32
	codec                         string
	bitrate                       int
	mode                          uint64
	bandwidthSummary              string
}

func parseARDOffer(b []byte, preferHEVC bool) (o ardOffer, err error) {
	if len(b) < 0x80 || b[0] != 0x1c || int(networkOrder.Uint16(b[2:])) != len(b)-4 || networkOrder.Uint16(b[4:]) != 3 {
		return o, errARDOffer
	}
	audio, video := int(networkOrder.Uint16(b[10:])), int(networkOrder.Uint16(b[12:]))
	at := 0x80 + audio
	if len(b) != at+92+video {
		return o, errARDOffer
	}
	if audio > 0 {
		copy(o.audioReceiveKey[:], b[0x24:0x52])
		copy(o.audioSendKey[:], b[0x52:0x80])
		blob, e := ardPlistData(b[0x80:at], "avcMediaStreamNegotiatorMediaBlob")
		if e != nil {
			return o, e
		}
		zr, e := zlib.NewReader(bytes.NewReader(blob))
		if e != nil {
			return o, e
		}
		raw, e := io.ReadAll(io.LimitReader(zr, (1<<20)+1))
		zr.Close()
		if e != nil || len(raw) > 1<<20 {
			return o, errARDOffer
		}
		fields, e := ardPBFields(raw)
		if e != nil {
			return o, e
		}
		for _, f := range fields {
			if f.number == 3 {
				stream, e := ardPBFields(f.data)
				if e != nil {
					return o, e
				}
				for _, v := range stream {
					if v.number == 1 && v.value <= 0xffffffff {
						o.audioPeerSSRC = uint32(v.value)
					}
				}
			}
		}
		if o.audioPeerSSRC == 0 {
			return o, errARDOffer
		}
	}
	// Offer directions are relative to the viewer: send first, receive second.
	copy(o.receiveKey[:], b[at:at+46])
	copy(o.sendKey[:], b[at+46:at+92])
	o.mode = 7
	bandwidthBit := uint64(262144) // remote desktop mode 7
	mode, modeErr := ardPlistValue(b[at+92:], "avcMediaStreamNegotiatorMode", 1)
	if modeErr != nil && !errors.Is(modeErr, errARDPlistMissing) {
		return o, modeErr
	}
	if modeErr == nil {
		var value uint64
		for _, n := range mode {
			value = value<<8 | uint64(n)
		}
		o.mode = value
		switch value {
		case 7:
		case 9:
			bandwidthBit = 4096
		default:
			return o, errARDOffer
		}
	}
	blob, err := ardPlistData(b[at+92:], "avcMediaStreamNegotiatorMediaBlob")
	if err != nil {
		return o, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(blob))
	if err != nil {
		return o, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return o, errARDOffer
	}
	fields, err := ardPBFields(raw)
	if err != nil {
		return o, err
	}
	supported := map[uint64]bool{}
	payloads := map[uint64][]byte{}
	for _, f := range fields {
		if f.number == 5 {
			stream, e := ardPBFields(f.data)
			if e != nil {
				return o, e
			}
			for _, v := range stream {
				if v.number == 1 && v.value <= 0xffffffff {
					o.peerSSRC = uint32(v.value)
				}
				if v.number == 6 {
					o.tiles = v.value
				}
				if v.number == 3 {
					codec, e := ardPBFields(v.data)
					if e != nil {
						return o, e
					}
					for _, c := range codec {
						if c.number == 1 {
							supported[c.value] = true
							payloads[c.value] = append([]byte(nil), v.data...)
						}
					}
				}
			}
		}
		if f.number == 9 {
			settings, e := ardPBFields(f.data)
			if e != nil {
				return o, e
			}
			var kind, rate, modes uint64
			for _, v := range settings {
				if v.number == 1 {
					kind = v.value
				}
				if v.number == 2 {
					rate = v.value
				}
				if v.number == 3 {
					modes = v.value
				}
			}
			if kind == 0 && len(o.bandwidthSummary) < 1024 {
				o.bandwidthSummary += fmt.Sprintf(" mask=%#x:%dkbps", modes, rate/1000)
			}
			// Field 3 selects the connection/arbiter mode; unrelated tiers are
			// alternatives, not additional caps on this video stream.
			if kind == 0 && rate > 0 && modes&bandwidthBit != 0 {
				limit := int(min(rate/1000, 1000000))
				if limit > 0 && (o.bitrate == 0 || limit < o.bitrate) {
					o.bitrate = limit
				}
			}
		}
	}
	if preferHEVC && supported[100] {
		o.codec = "hevc"
	} else if supported[123] {
		o.codec = "h264"
	} else if supported[100] {
		o.codec = "hevc"
	}
	if o.peerSSRC == 0 || o.codec == "" {
		return o, errARDOffer
	}
	payloadType := uint64(100)
	if o.codec == "h264" {
		payloadType = 123
	}
	o.videoPayload = payloads[payloadType]
	return o, nil
}

// Read top-level data and integer settings. Validate every offset and length
// against the object table; malicious plist integers must never overflow slices.
func ardPlistData(b []byte, key string) ([]byte, error) { return ardPlistValue(b, key, 4) }

func ardPlistValue(b []byte, key string, want byte) ([]byte, error) {
	if len(b) < 40 || string(b[:8]) != "bplist00" {
		return nil, errARDOffer
	}
	trailer := b[len(b)-32:]
	os, rs := int(trailer[6]), int(trailer[7])
	count, root, table := networkOrder.Uint64(trailer[8:]), networkOrder.Uint64(trailer[16:]), networkOrder.Uint64(trailer[24:])
	if os < 1 || os > 8 || rs < 1 || rs > 8 || count == 0 || count > 65536 || root >= count || table < 8 || table > uint64(len(b)-32) || count*uint64(os) > uint64(len(b)-32)-table {
		return nil, errARDOffer
	}
	number := func(b []byte) uint64 {
		var v uint64
		for _, n := range b {
			v = v<<8 | uint64(n)
		}
		return v
	}
	object := func(index uint64) (byte, []byte, error) {
		if index >= count {
			return 0, nil, errARDOffer
		}
		at := number(b[table+index*uint64(os) : table+(index+1)*uint64(os)])
		if at < 8 || at >= table {
			return 0, nil, errARDOffer
		}
		marker := b[at]
		at++
		n := uint64(marker & 15)
		if marker>>4 == 1 {
			if n > 3 {
				return 0, nil, errARDOffer
			}
			n = 1 << n
		} else if n == 15 {
			if at >= table || b[at]>>4 != 1 || b[at]&15 > 3 {
				return 0, nil, errARDOffer
			}
			size := uint64(1) << (b[at] & 15)
			at++
			if size > table-at {
				return 0, nil, errARDOffer
			}
			n = number(b[at : at+size])
			at += size
		}
		if marker>>4 == 13 {
			if n > count {
				return 0, nil, errARDOffer
			}
			n *= 2 * uint64(rs)
		}
		if n > table-at {
			return 0, nil, errARDOffer
		}
		return marker >> 4, b[at : at+n], nil
	}
	kind, dict, err := object(root)
	if err != nil || kind != 13 {
		return nil, errARDOffer
	}
	n := len(dict) / (rs * 2)
	for i := 0; i < n; i++ {
		kind, k, e := object(number(dict[i*rs : (i+1)*rs]))
		if e != nil {
			return nil, e
		}
		if kind == 5 && string(k) == key {
			kind, data, e := object(number(dict[(n+i)*rs : (n+i+1)*rs]))
			if e != nil || kind != want {
				return nil, errARDOffer
			}
			return data, nil
		}
	}
	return nil, errARDPlistMissing
}

type ardPBField struct {
	number uint64
	value  uint64
	data   []byte
}

func ardPBFields(b []byte) ([]ardPBField, error) {
	var out []ardPBField
	varint := func() (uint64, bool) {
		var v uint64
		for i := 0; i < 10 && len(b) > 0; i++ {
			c := b[0]
			b = b[1:]
			if i == 9 && c > 1 {
				return 0, false
			}
			v |= uint64(c&127) << uint(7*i)
			if c < 128 {
				return v, true
			}
		}
		return 0, false
	}
	for len(b) > 0 {
		key, ok := varint()
		if !ok || key>>3 == 0 || len(out) >= 65536 {
			return nil, errARDOffer
		}
		f := ardPBField{number: key >> 3}
		switch key & 7 {
		case 0:
			if f.value, ok = varint(); !ok {
				return nil, errARDOffer
			}
		case 2:
			n, ok := varint()
			if !ok || n > uint64(len(b)) {
				return nil, errARDOffer
			}
			f.data = b[:n]
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, errARDOffer
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return nil, errARDOffer
			}
			b = b[4:]
		default:
			return nil, errARDOffer
		}
		out = append(out, f)
	}
	return out, nil
}
func ardVarint(b []byte, v uint64) []byte {
	for v >= 128 {
		b = append(b, byte(v)|128)
		v >>= 7
	}
	return append(b, byte(v))
}
func ardPBInt(b []byte, field, value uint64) []byte { return ardVarint(ardVarint(b, field<<3), value) }
func ardPBData(b []byte, field uint64, data []byte) []byte {
	return append(ardVarint(ardVarint(b, field<<3|2), uint64(len(data))), data...)
}
func ardAnswer(ssrc uint32, w, h, fps int) []byte {
	return ardAnswerForOffer(ssrc, w, h, fps, ardOffer{codec: "hevc"})
}

func ardAnswerForOffer(ssrc uint32, w, h, fps int, offer ardOffer) []byte {
	stream := ardPBInt(nil, 1, uint64(ssrc))
	stream = ardPBInt(stream, 2, 0)
	payloadType := uint64(100)
	if offer.codec == "h264" {
		payloadType = 123
	}
	payload := ardPBInt(nil, 1, payloadType)
	// Select SDR 4:2:0, supported by the VA-API zero-CPU-copy pipeline.
	// Preserve codec-specific feature strings/parameter set IDs from the offer.
	for _, operation := range []uint64{1, 2} {
		rule := ardPBInt(nil, 1, 1)
		rule = ardPBInt(rule, 2, operation)
		rule = ardPBInt(rule, 3, 128)
		rule = ardPBInt(rule, 4, 0)
		payload = ardPBData(payload, 2, rule)
	}
	fields, _ := ardPBFields(offer.videoPayload)
	for _, f := range fields {
		if f.number == 3 {
			payload = ardPBData(payload, 3, f.data)
		}
		if f.number == 4 {
			payload = ardPBInt(payload, 4, f.value)
		}
	}
	stream = ardPBData(stream, 3, payload)
	stream = ardPBInt(stream, 4, uint64(w))
	stream = ardPBInt(stream, 5, uint64(h))
	stream = ardPBInt(stream, 6, 1)
	stream = ardPBInt(stream, 7, 0)
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	blob := ardPBInt(nil, 1, 1)
	blob = ardPBInt(blob, 2, 1)
	blob = ardPBData(blob, 5, stream)
	blob = ardPBData(blob, 6, []byte("Mirage"))
	blob = ardPBInt(blob, 14, 2)
	blob = ardPBInt(blob, 16, 0)
	z.Write(blob)
	z.Close()
	// AVConference expects endpoint information alongside the media blob.
	// Identify this implementation honestly rather than impersonating macOS.
	endpoint := ardPBInt(nil, 1, uint64(ssrc))
	endpoint = ardPBInt(endpoint, 2, 1)
	endpoint = ardPBData(endpoint, 3, []byte("Mirage"))
	endpoint = ardPBData(endpoint, 4, []byte("1"))
	endpoint = ardPBData(endpoint, 5, []byte("Linux"))
	plist := ardMediaEndpointPlist(compressed.Bytes(), endpoint)
	body := make([]byte, 18)
	networkOrder.PutUint16(body, 2)
	networkOrder.PutUint16(body[2:], 2)
	if fps > 30 {
		networkOrder.PutUint32(body[4:], 1)
	}
	networkOrder.PutUint16(body[10:], uint16(len(plist)))
	return append(body, plist...)
}

// Keep the single-key wrapper for existing callers and offer fixtures.
func ardMediaPlist(data []byte) []byte { return ardMediaEndpointPlist(data, nil) }

func ardMediaEndpointPlist(data, endpoint []byte) []byte {
	keys := []string{"avcMediaStreamNegotiatorMediaBlob"}
	values := [][]byte{data}
	if endpoint != nil {
		keys = append(keys, "avcMediaStreamOptionRemoteEndpointInfo")
		values = append(values, endpoint)
	}
	b := []byte("bplist00")
	offsets := []int{len(b)}
	b = append(b, 0xd0|byte(len(keys)))
	for i := range keys {
		b = append(b, byte(1+i))
	}
	for i := range values {
		b = append(b, byte(1+len(keys)+i))
	}
	for _, key := range keys {
		offsets = append(offsets, len(b))
		b = append(b, 0x5f, 0x10, byte(len(key)))
		b = append(b, key...)
	}
	for _, value := range values {
		offsets = append(offsets, len(b))
		b = append(b, 0x4f, 0x12)
		b = append(b, u32(uint32(len(value)))...)
		b = append(b, value...)
	}
	table := len(b)
	for _, off := range offsets {
		b = append(b, u32(uint32(off))...)
	}
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = 4, 1
	networkOrder.PutUint64(trailer[8:], uint64(len(offsets)))
	networkOrder.PutUint64(trailer[24:], uint64(table))
	return append(b, trailer...)
}
