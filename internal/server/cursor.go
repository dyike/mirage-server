package server

import (
	"errors"
	"image"
	"math"

	"github.com/dyike/mirage-server/internal/server/input"
)

type Cursor interface {
	Updates() <-chan input.CursorImage
	Close() error
}

func (p *rfbPeer) watchCursor() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.ctx.Done():
				return
			case v, ok := <-p.cursor.Updates():
				if !ok {
					p.cancel()
					return
				}
				p.state.Lock()
				p.state.cursorImage = v
				p.state.Unlock()
				if err := p.refreshCursor(); err != nil {
					p.cancel()
					return
				}
			}
		}
	}()
}
func scaleCursor(v input.CursorImage, size image.Point) input.CursorImage {
	if v.SourceWidth <= 0 || v.SourceHeight <= 0 || v.Width == 0 || v.Height == 0 {
		return v
	}
	sx, sy := float64(size.X)/float64(v.SourceWidth), float64(size.Y)/float64(v.SourceHeight)
	w, h := max(1, int(math.Round(float64(v.Width)*sx))), max(1, int(math.Round(float64(v.Height)*sy)))
	if w > 512 || h > 512 {
		return input.CursorImage{}
	}
	pixels := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src := (min(v.Height-1, y*v.Height/h)*v.Width + min(v.Width-1, x*v.Width/w)) * 4
			copy(pixels[(y*w+x)*4:], v.Pixels[src:src+4])
		}
	}
	v.Pixels = pixels
	v.Width, v.Height = w, h
	v.Hotspot = image.Pt(min(w-1, int(math.Round(float64(v.Hotspot.X)*sx))), min(h-1, int(math.Round(float64(v.Hotspot.Y)*sy))))
	return v
}
func cursorMessage(pf rfbFormat, v input.CursorImage) ([]byte, error) {
	if v.Width < 0 || v.Height < 0 || v.Width > 512 || v.Height > 512 || len(v.Pixels) != v.Width*v.Height*4 {
		return nil, errors.New("invalid cursor image")
	}
	// RFB's one-bit mask cannot carry fractional alpha. Convert premultiplied
	// RGBA to straight RGB before packing the visible pixels.
	rgba := append([]byte(nil), v.Pixels...)
	mask := make([]byte, ((v.Width+7)/8)*v.Height)
	for y := 0; y < v.Height; y++ {
		for x := 0; x < v.Width; x++ {
			i := (y*v.Width + x) * 4
			a := int(rgba[i+3])
			if a >= 128 {
				mask[y*((v.Width+7)/8)+x/8] |= 128 >> uint(x%8)
				for c := 0; c < 3; c++ {
					rgba[i+c] = byte(min(255, int(rgba[i+c])*255/a))
				}
			}
		}
	}
	pixels, err := pf.pack(rgba, v.Width, v.Height)
	if err != nil {
		return nil, err
	}
	hot := v.Hotspot
	if v.Width > 0 {
		hot.X = max(0, min(v.Width-1, hot.X))
	}
	if v.Height > 0 {
		hot.Y = max(0, min(v.Height-1, hot.Y))
	}
	r := rectHeader(image.Rectangle{Min: hot, Max: hot.Add(image.Pt(v.Width, v.Height))}, uint32(0xffffff11))
	r = append(r, pixels...)
	r = append(r, mask...)
	return updateMessage(r), nil
}

func (p *rfbPeer) refreshCursor() error {
	p.state.Lock()
	if !p.state.cursorActive {
		p.state.Unlock()
		return nil
	}
	v, pf, d := p.state.cursorImage, p.state.format, p.state.presented.clone()
	p.state.Unlock()
	for _, s := range d.Screens {
		if s.ID == v.ScreenID || v.ScreenID == 0 {
			v = scaleCursor(v, s.Bounds.Size())
			break
		}
	}
	var message []byte
	var err error
	if p.ard != nil {
		message, err = ardCursorMessage(v)
	} else {
		message, err = cursorMessage(pf, v)
	}
	if err != nil {
		return err
	}
	return p.send(message)
}
