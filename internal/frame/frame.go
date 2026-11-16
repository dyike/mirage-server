// Package frame holds the pixel buffer shared by remote desktop protocols
// and renderers.
package frame

import (
	"image"
	"time"
)

// Timing follows one picture through decoding and drawing. A synchronized
// source is mapped onto the client's monotonic clock, with a clock-error bound.
type Timing struct {
	Source      time.Time
	ClockError  time.Duration
	ClockSynced bool
}

// Framebuffer is the remote screen as tightly packed RGBA (stride W*4).
// Protocol readers and renderers share the same external pixel lock.
type Framebuffer struct {
	W, H   int
	Pix    []byte
	Timing Timing
}

// Resize sets the dimensions and clears the pixels, reusing storage when
// possible. The caller must hold the framebuffer's external lock.
func (f *Framebuffer) Resize(w, h int) {
	f.W, f.H = w, h
	f.Timing = Timing{}
	if n := w * h * 4; cap(f.Pix) >= n {
		f.Pix = f.Pix[:n]
		clear(f.Pix)
	} else {
		f.Pix = make([]byte, n)
	}
}

// Bounds returns the framebuffer rectangle.
func (f *Framebuffer) Bounds() image.Rectangle { return image.Rect(0, 0, f.W, f.H) }
