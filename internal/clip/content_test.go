package clip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"golang.org/x/image/tiff"
)

func TestImageNormalizesTIFFAndPNGWithoutLosingPixels(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	source.SetNRGBA(1, 1, color.NRGBA{R: 253, G: 129, B: 61, A: 127})
	var data bytes.Buffer
	if err := tiff.Encode(&data, source, nil); err != nil {
		t.Fatal(err)
	}
	content, err := (Content{MIME: "image/tiff", Data: data.Bytes()}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(content.Data))
	if err != nil || color.NRGBAModel.Convert(decoded.At(1, 1)) != source.NRGBAAt(1, 1) {
		t.Fatal("TIFF conversion lost pixels", err)
	}
	again, err := content.Normalize()
	if err != nil || again.Hash() != content.Hash() {
		t.Fatal("PNG normalization would create an echo loop", err)
	}
}

func TestClipboardRejectsInvalidTextAndOversizedImagesBeforeDecoding(t *testing.T) {
	if _, err := (Content{MIME: Text, Data: []byte{0xff}}).Normalize(); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := (Content{MIME: Text, Data: bytes.Repeat([]byte{'x'}, MaxText+1)}).Normalize(); err == nil {
		t.Fatal("oversized text accepted")
	}
	var data bytes.Buffer
	png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	payload := bytes.Clone(data.Bytes())
	binary.BigEndian.PutUint32(payload[16:], 65536)
	binary.BigEndian.PutUint32(payload[20:], 65536)
	binary.BigEndian.PutUint32(payload[29:], crc32.ChecksumIEEE(payload[12:29]))
	if _, err := (Content{MIME: PNG, Data: payload}).Normalize(); err == nil || !strings.Contains(err.Error(), "megapixels") {
		t.Fatal("oversized image was not rejected before decoding", err)
	}
}
