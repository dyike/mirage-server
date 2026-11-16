package server

import (
	"bytes"
	"compress/zlib"
	"errors"
	"image"

	"github.com/dyike/mirage-server/internal/server/input"
)

func ardCursorMessage(v input.CursorImage) ([]byte, error) {
	if v.Width < 0 || v.Height < 0 || v.Width > 512 || v.Height > 512 || len(v.Pixels) != v.Width*v.Height*4 {
		return nil, errors.New("invalid ARD cursor")
	}
	if v.Width == 0 || v.Height == 0 {
		return updateMessage(append(rectHeaderEmpty(1104), u32(0)...)), nil
	}
	pixels := v.Width * v.Height
	raw := make([]byte, pixels*5)
	for i := 0; i < pixels; i++ {
		a := uint32(v.Pixels[i*4+3])
		raw[pixels*4+i] = byte(a)
		for j := 0; j < 3; j++ {
			if a > 0 {
				raw[i*4+j] = byte(min(255, uint32(v.Pixels[i*4+j])*255/a))
			}
		}
		raw[i*4+3] = 255
	}
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	z.Write(raw)
	z.Close()
	hot := image.Pt(max(0, min(v.Width-1, v.Hotspot.X)), max(0, min(v.Height-1, v.Hotspot.Y)))
	body := rectHeader(image.Rectangle{Min: hot, Max: hot.Add(image.Pt(v.Width, v.Height))}, 1104)
	body = append(body, u32(1000)...)
	body = append(body, u32(uint32(compressed.Len()))...)
	return updateMessage(append(body, compressed.Bytes()...)), nil
}
