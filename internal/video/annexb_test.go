package video

import (
	"bytes"
	"errors"
	"testing"
)

func TestAppendAnnexBReusesDescriptors(t *testing.T) {
	data := []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 1, 0x68, 2, 0, 0, 0, 1, 0x65, 3}
	var storage [8][]byte
	nals := AppendAnnexB(storage[:0], data)
	if len(nals) != 3 || !bytes.Equal(nals[0], []byte{0x67, 1}) || !bytes.Equal(nals[1], []byte{0x68, 2}) || !bytes.Equal(nals[2], []byte{0x65, 3}) {
		t.Fatal(nals)
	}
	data[4] = 0x27
	if nals[0][0] != 0x27 {
		t.Fatal("NAL payload was copied")
	}
	if n := testing.AllocsPerRun(100, func() { nals = AppendAnnexB(storage[:0], data) }); n != 0 {
		t.Fatal(n)
	}
	clear(storage[:])
}

func TestAnnexBDescriptorLimit(t *testing.T) {
	data := bytes.Repeat([]byte{0, 0, 1, 0x65, 1}, 4097)
	storage := make([][]byte, 0, 4096)
	nals, err := AppendAnnexBBounded(storage, data, 4096)
	if !errors.Is(err, ErrNALCount) || len(nals) != 4096 || cap(nals) != cap(storage) {
		t.Fatal(len(nals), cap(nals), err)
	}
	if n := testing.AllocsPerRun(100, func() {
		nals, err = AppendAnnexBBounded(storage, data, 4096)
	}); n != 0 {
		t.Fatal(n)
	}
	if _, err := AppendAnnexBBounded(nil, data, 0); !errors.Is(err, ErrNALCount) {
		t.Fatal(err)
	}
	nals, err = AppendAnnexBBounded(storage, data[:4096*5], 4096)
	if err != nil || len(nals) != 4096 {
		t.Fatal(len(nals), err)
	}
	clear(nals)
}
