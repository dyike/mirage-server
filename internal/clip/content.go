// Package clip defines the text and image clipboard content shared by a session.
package clip

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"unicode/utf8"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	Text      = "text/plain"
	PNG       = "image/png"
	MaxBytes  = 32 << 20
	MaxText   = 1 << 20
	MaxPixels = 32 << 20
)

var ErrUnsupported = errors.New("clipboard has no supported text or image")
var ErrEmpty = errors.New("clipboard is empty")

// Content owns its bytes. An empty MIME means there is no supported content;
// a text representation with zero bytes is an explicitly empty clipboard.
type Content struct {
	MIME string
	Data []byte
}

func (c Content) Clone() Content { return Content{MIME: c.MIME, Data: bytes.Clone(c.Data)} }

func (c Content) Hash() [32]byte {
	h := sha256.New()
	h.Write([]byte(c.MIME))
	h.Write([]byte{0})
	h.Write(c.Data)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// Normalize validates before decoding and gives image representations a stable
// PNG encoding, so native conversions do not create a clipboard echo loop.
func (c Content) Normalize() (Content, error) {
	if len(c.Data) > MaxBytes {
		return Content{}, errors.New("clipboard exceeds 32 MiB")
	}
	if c.MIME == Text {
		if len(c.Data) > MaxText || !utf8.Valid(c.Data) {
			return Content{}, errors.New("clipboard text is too large or invalid UTF-8")
		}
		return c.Clone(), nil
	}
	switch c.MIME {
	case PNG, "image/tiff", "image/jpeg", "image/gif", "image/webp", "image/bmp":
	default:
		return Content{}, ErrUnsupported
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(c.Data))
	if err != nil {
		return Content{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || uint64(cfg.Width)*uint64(cfg.Height) > MaxPixels {
		return Content{}, errors.New("clipboard image exceeds 32 megapixels")
	}
	img, _, err := image.Decode(bytes.NewReader(c.Data))
	if err != nil {
		return Content{}, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return Content{}, err
	}
	if out.Len() > MaxBytes {
		return Content{}, errors.New("clipboard image exceeds 32 MiB")
	}
	return Content{MIME: PNG, Data: out.Bytes()}, nil
}
