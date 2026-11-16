package server

import (
	"bytes"
	"compress/zlib"
	"testing"
)

func TestARDSelectsBandwidthForNegotiatorMode(t *testing.T) {
	for _, tc := range []struct {
		mode byte
		want int
	}{{7, 60000}, {9, 40000}} {
		stream := ardPBInt(nil, 1, 42)
		stream = ardPBData(stream, 3, ardPBInt(nil, 1, 100))
		pb := ardPBData(nil, 5, stream)
		for _, tier := range [][2]uint64{{6000000, 131072}, {60000000, 262144}, {40000000, 12288}, {3000000, 98304}} {
			row := ardPBInt(nil, 1, 0)
			row = ardPBInt(row, 2, tier[0])
			row = ardPBInt(row, 3, tier[1])
			pb = ardPBData(pb, 9, row)
		}
		var compressed bytes.Buffer
		z := zlib.NewWriter(&compressed)
		z.Write(pb)
		z.Close()
		// Dictionary, two ASCII keys, data and integer mode, with 32-bit offsets.
		objects := [][]byte{{0xd2, 1, 2, 3, 4}}
		for _, key := range []string{"avcMediaStreamNegotiatorMediaBlob", "avcMediaStreamNegotiatorMode"} {
			objects = append(objects, append([]byte{0x5f, 0x10, byte(len(key))}, []byte(key)...))
		}
		data := append([]byte{0x4f, 0x12}, u32(uint32(compressed.Len()))...)
		objects = append(objects, append(data, compressed.Bytes()...), []byte{0x10, tc.mode})
		plist := []byte("bplist00")
		var offsets []int
		for _, object := range objects {
			offsets = append(offsets, len(plist))
			plist = append(plist, object...)
		}
		table := len(plist)
		for _, offset := range offsets {
			plist = append(plist, u32(uint32(offset))...)
		}
		trailer := make([]byte, 32)
		trailer[6], trailer[7] = 4, 1
		networkOrder.PutUint64(trailer[8:], uint64(len(objects)))
		networkOrder.PutUint64(trailer[24:], uint64(table))
		plist = append(plist, trailer...)
		wire := append(ardTestOffer()[:220:220], plist...)
		networkOrder.PutUint16(wire[12:], uint16(len(plist)))
		networkOrder.PutUint16(wire[2:], uint16(len(wire)-4))
		got, err := parseARDOffer(wire, true)
		if err != nil || got.bitrate != tc.want {
			t.Fatalf("mode %d: rate=%d err=%v", tc.mode, got.bitrate, err)
		}
	}
}

func TestARDAnswerDeclaresFrameRate(t *testing.T) {
	for _, fps := range []int{0, 30, 60} {
		answer := ardAnswer(42, 3840, 2160, fps)
		if networkOrder.Uint16(answer[2:]) != 2 {
			t.Fatal("missing answer version")
		}
		if got := networkOrder.Uint32(answer[4:]); (got == 1) != (fps > 30) {
			t.Fatalf("fps=%d flags=%d", fps, got)
		}
		if int(networkOrder.Uint16(answer[10:])) != len(answer)-18 {
			t.Fatal("bad payload length")
		}
	}
}
