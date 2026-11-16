package server

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
)

func TestARDAnswerSelectsCodecAndSingleTile(t *testing.T) {
	for _, codec := range []string{"hevc", "h264"} {
		offer := ardOffer{codec: codec, tiles: 4, videoPayload: ardPBInt(ardPBData(nil, 3, []byte("features")), 4, 14)}
		answer := ardAnswerForOffer(42, 3840, 2160, 60, offer)
		blob, err := ardPlistData(answer[18:], "avcMediaStreamNegotiatorMediaBlob")
		if err != nil {
			t.Fatal(err)
		}
		zr, err := zlib.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		pb, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			t.Fatal(err)
		}
		fields, err := ardPBFields(pb)
		if err != nil {
			t.Fatal(err)
		}
		var stream []ardPBField
		for _, f := range fields {
			if f.number == 5 {
				stream, err = ardPBFields(f.data)
			}
		}
		if err != nil || len(stream) == 0 {
			t.Fatal("missing screen settings")
		}
		found := false
		for _, f := range stream {
			if f.number == 6 && f.value != 1 {
				t.Fatal("advertised unencoded tiles")
			}
			if f.number == 3 {
				payload, err := ardPBFields(f.data)
				if err != nil {
					t.Fatal(err)
				}
				want := uint64(100)
				if codec == "h264" {
					want = 123
				}
				for _, v := range payload {
					if v.number == 1 {
						found = v.value == want
					}
					if v.number == 4 && v.value != 14 {
						t.Fatal("lost parameter set")
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing negotiated %s payload", codec)
		}
	}
}

func TestARDAnswerIncludesServerEndpoint(t *testing.T) {
	answer := ardAnswer(42, 3840, 2160, 60)
	if got := int(networkOrder.Uint16(answer[10:])); got != len(answer)-18 {
		t.Fatalf("plist length = %d, want %d", got, len(answer)-18)
	}
	endpoint, err := ardPlistData(answer[18:], "avcMediaStreamOptionRemoteEndpointInfo")
	if err != nil {
		t.Fatal(err)
	}
	fields, err := ardPBFields(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || fields[0].value != 42 || string(fields[2].data) != "Mirage" || string(fields[4].data) != "Linux" {
		t.Fatalf("incorrect server endpoint: %+v", fields)
	}
	// Both dictionary entries must resolve independently with nontrivial data lengths.
	data := bytes.Repeat([]byte{0x71}, 1024)
	plist := ardMediaEndpointPlist(data, endpoint)
	got, err := ardPlistData(plist, "avcMediaStreamNegotiatorMediaBlob")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("media blob corrupted: %v", err)
	}
}
