package rfb

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"net"
)

// tightLength is the protocol's 7+7+8 bit compact length, not a varint.
func (c *Client) tightLength() (int, error) {
	a, err := c.r.ReadByte()
	if err != nil {
		return 0, err
	}
	n := int(a & 127)
	if a&128 == 0 {
		return n, nil
	}
	b, err := c.r.ReadByte()
	if err != nil {
		return 0, err
	}
	n |= int(b&127) << 7
	if b&128 == 0 {
		return n, nil
	}
	d, err := c.r.ReadByte()
	if err != nil {
		return 0, err
	}
	return n | int(d)<<14, nil
}

// decodeTight finishes decoding outside the shared pixel lock, then commits
// the rectangle atomically. Tight's four zlib streams persist across updates.
func (c *Client) decodeTight(r image.Rectangle) error {
	if r.Empty() || r.Dx() > 2048 || r.Dx()*r.Dy() > 16<<20 {
		return protoErr("invalid Tight rectangle %v", r)
	}
	control, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	for i := range c.tight {
		if control&(1<<i) != 0 {
			c.tight[i].stop()
		}
	}
	mode := control >> 4
	switch mode {
	case 8:
		var pixel [3]byte
		if _, err := io.ReadFull(c.r, pixel[:]); err != nil {
			return err
		}
		color := uint32(pixel[0]) | uint32(pixel[1])<<8 | uint32(pixel[2])<<16 | opaque
		c.lockFB()
		defer c.unlockFB()
		pix := c.pix32()
		for y := r.Min.Y; y < r.Max.Y; y++ {
			fillRow(pix[y*c.fb.W+r.Min.X:y*c.fb.W+r.Max.X], color)
		}
		c.wrote(r)
		return nil
	case 9:
		n, err := c.tightLength()
		if err != nil {
			return err
		}
		buf := c.scratch(n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return err
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(buf))
		if err != nil {
			return err
		}
		if cfg.Width != r.Dx() || cfg.Height != r.Dy() {
			return protoErr("Tight JPEG dimensions differ from rectangle")
		}
		img, err := jpeg.Decode(bytes.NewReader(buf))
		if err != nil {
			return err
		}
		c.lockFB()
		defer c.unlockFB()
		dst := &image.RGBA{Pix: c.fb.Pix, Stride: c.fb.W * 4, Rect: c.fb.Bounds()}
		draw.Draw(dst, r, img, img.Bounds().Min, draw.Src)
		c.wrote(r)
		return nil
	}
	if mode > 7 {
		return protoErr("unsupported Tight compression %d", mode)
	}
	filter := byte(0)
	if mode&4 != 0 {
		filter, err = c.r.ReadByte()
		if err != nil {
			return err
		}
	}
	w, h := r.Dx(), r.Dy()
	size := w * h * 3
	var palette []byte
	colors := 0
	switch filter {
	case 0, 2:
	case 1:
		count, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		colors = int(count) + 1
		if colors < 2 {
			return protoErr("invalid Tight palette size %d", colors)
		}
		palette = make([]byte, colors*3)
		if _, err := io.ReadFull(c.r, palette); err != nil {
			return err
		}
		size = w * h
		if colors == 2 {
			size = ((w + 7) / 8) * h
		}
	default:
		return protoErr("unsupported Tight filter %d", filter)
	}
	data := make([]byte, size)
	if size < 12 {
		if _, err := io.ReadFull(c.r, data); err != nil {
			return err
		}
	} else {
		n, err := c.tightLength()
		if err != nil {
			return err
		}
		stream := &c.tight[mode&3]
		if stream.src == nil {
			stream.start()
		}
		// A truncated compressed chunk may leave the inflater waiting for
		// more input. Closing the client must release both queues as well
		// as the socket. Capture the queues, not mutable stream fields.
		if c.decodeContext != nil {
			src, out := stream.src, stream.out
			stop := context.AfterFunc(c.decodeContext, func() {
				src.closeWithError(net.ErrClosed)
				out.closeWithError(net.ErrClosed)
			})
			defer stop()
		}
		if _, err := io.CopyN(stream.src, c.r, int64(n)); err != nil {
			return err
		}
		if _, err := io.ReadFull(stream.br, data); err != nil {
			return err
		}
	}
	var rgb []byte
	switch filter {
	case 0:
		rgb = data
	case 1:
		rgb = make([]byte, w*h*3)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				index := 0
				if colors == 2 {
					index = int(data[y*((w+7)/8)+x/8] >> uint(7-x%8) & 1)
				} else {
					index = int(data[y*w+x])
				}
				if index >= colors {
					return protoErr("Tight palette index outside palette")
				}
				copy(rgb[(y*w+x)*3:], palette[index*3:index*3+3])
			}
		}
	case 2:
		rgb = data
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				for channel := 0; channel < 3; channel++ {
					i := (y*w+x)*3 + channel
					left, up, corner := 0, 0, 0
					if x > 0 {
						left = int(rgb[i-3])
					}
					if y > 0 {
						up = int(rgb[i-w*3])
					}
					if x > 0 && y > 0 {
						corner = int(rgb[i-w*3-3])
					}
					predicted := max(0, min(255, left+up-corner))
					rgb[i] += byte(predicted)
				}
			}
		}
	}
	c.lockFB()
	defer c.unlockFB()
	for y := 0; y < h; y++ {
		dst := c.fb.Pix[((r.Min.Y+y)*c.fb.W+r.Min.X)*4:]
		src := rgb[y*w*3:]
		for x := 0; x < w; x++ {
			copy(dst[x*4:x*4+3], src[x*3:x*3+3])
			dst[x*4+3] = 255
		}
	}
	c.wrote(r)
	return nil
}
