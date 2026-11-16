package rfb

import (
	"bufio"
	"bytes"
	"image"
	"testing"

	"github.com/klauspost/compress/zlib"
)

func TestPictureEncodingFollowsSuccessfulDecoding(t *testing.T) {
	c := &Client{}
	c.fb.Resize(1, 1)
	c.cov.reset(1, 1)
	if c.Encoding() != "" {
		t.Fatal("new connection claimed an encoding before receiving pixels")
	}
	r := image.Rect(0, 0, 1, 1)
	decode := func(enc int32, rect image.Rectangle, body []byte) error {
		data := append([]byte{0, 0, 1}, rectHeader(rect, enc)...)
		c.r = bufio.NewReader(bytes.NewReader(append(data, body...)))
		return c.framebufferUpdate()
	}
	if err := decode(EncRaw, r, []byte{1, 2, 3, 0}); err != nil || c.Encoding() != "Raw" {
		t.Fatal("raw frame was not reported", err, c.Encoding())
	}
	if err := decode(EncCopyRect, r, make([]byte, 4)); err != nil || c.Encoding() != "Raw" {
		t.Fatal("copy operation replaced the picture encoding", err, c.Encoding())
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	writer.Write([]byte{1, 1, 2, 3}) // solid-colour ZRLE tile
	writer.Flush()
	body := append(u32(uint32(compressed.Len())), compressed.Bytes()...)
	if err := decode(EncZRLE, r, body); err != nil || c.Encoding() != "ZRLE" {
		t.Fatal("ZRLE frame was not reported", err, c.Encoding())
	}
	if err := decode(EncPseudoLastRect, image.Rectangle{}, nil); err != nil || c.Encoding() != "ZRLE" {
		t.Fatal("pseudo-encoding replaced the picture encoding", err, c.Encoding())
	}
	if err := decode(EncRaw, r, []byte{1}); err == nil || c.Encoding() != "ZRLE" {
		t.Fatal("truncated frame published a new encoding", err, c.Encoding())
	}
}
