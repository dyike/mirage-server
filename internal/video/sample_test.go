package video

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestAccessUnitSamples(t *testing.T) {
	for _, tc := range []struct {
		codec Codec
		sets  [][]byte
		frame []byte
	}{
		{H264, [][]byte{{0x67, 1}, {0x68, 2}}, []byte{0x65, 3, 4}},
		{HEVC, [][]byte{{0x40, 1}, {0x42, 2}, {0x44, 3}}, []byte{0x26, 1, 4}},
	} {
		t.Run(tc.codec.String(), func(t *testing.T) {
			nals := append([][]byte{tc.frame}, tc.sets...)
			nals = append(nals, tc.frame)
			var unit accessUnit
			unit.prepare(tc.codec, nals)
			if len(unit.sets) != len(tc.sets) {
				t.Fatalf("parameter sets = %v", unit.sets)
			}
			for i, set := range tc.sets {
				if !bytes.Equal(unit.sets[i], set) {
					t.Fatalf("parameter set %d = %x, want %x", i, unit.sets[i], set)
				}
			}
			want := binary.BigEndian.AppendUint32(nil, uint32(len(tc.frame)))
			want = append(want, tc.frame...)
			want = append(want, want...)
			if !bytes.Equal(unit.data, want) {
				t.Fatalf("sample = %x, want %x", unit.data, want)
			}
			// Reusing the sample must not leave a previous frame's tail or
			// keep references to its parameter sets.
			sets := unit.sets
			unit.prepare(tc.codec, [][]byte{tc.frame})
			if len(unit.sets) != 0 || !bytes.Equal(unit.data, want[:len(want)/2]) {
				t.Fatalf("stale sample data: sets %v, data %x", unit.sets, unit.data)
			}
			for _, set := range sets {
				if set != nil {
					t.Fatal("scratch list retained a previous input packet")
				}
			}
			unit.prepare(tc.codec, tc.sets)
			if len(unit.data) != 0 {
				t.Fatal("parameter-only access unit produced a picture sample")
			}
			unit.prepare(tc.codec, nil)
			if len(unit.data) != 0 || len(unit.sets) != 0 {
				t.Fatal("empty access unit retained a sample")
			}
		})
	}
}

func TestAccessUnitSteadyStateAllocations(t *testing.T) {
	for _, codec := range []Codec{H264, HEVC} {
		t.Run(codec.String(), func(t *testing.T) {
			nals := sampleNALs(codec)
			var unit accessUnit
			unit.prepare(codec, nals)
			if allocs := testing.AllocsPerRun(100, func() {
				unit.prepare(codec, nals)
				unit.clearSets()
			}); allocs != 0 {
				t.Fatalf("sample preparation allocated %.0f times per frame", allocs)
			}
		})
	}
}

func sampleNALs(codec Codec) [][]byte {
	picture := make([]byte, 256<<10)
	if codec == HEVC {
		picture[0] = 0x26
		return [][]byte{{0x40, 1}, {0x42, 1}, {0x44, 1}, picture}
	}
	picture[0] = 0x65
	return [][]byte{{0x67}, {0x68}, picture}
}

func BenchmarkAccessUnit(b *testing.B) {
	for _, codec := range []Codec{H264, HEVC} {
		b.Run(codec.String(), func(b *testing.B) {
			nals := sampleNALs(codec)
			var unit accessUnit
			unit.prepare(codec, nals)
			b.SetBytes(int64(len(unit.data)))
			b.ReportAllocs()
			for b.Loop() {
				unit.prepare(codec, nals)
				unit.clearSets()
			}
		})
	}
}
