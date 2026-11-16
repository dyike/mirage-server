package input

import (
	"errors"
	"image"
	"math"
)

// Coordinates are normalized to the captured image, independent of encoder
// resolution and local DPI. Code is a Linux evdev code; Symbol is an X keysym.
type Event struct {
	Type       string  `json:"type"`
	X          float64 `json:"x,omitempty"`
	Y          float64 `json:"y,omitempty"`
	Buttons    uint16  `json:"buttons,omitempty"`
	Symbol     uint32  `json:"symbol,omitempty"`
	Code       uint32  `json:"code,omitempty"`
	Down       bool    `json:"down,omitempty"`
	DX         float64 `json:"dx,omitempty"`
	DY         float64 `json:"dy,omitempty"`
	ScrollStop bool    `json:"scroll_stop,omitempty"` // end the continuous axis sequence
}

func (e Event) Validate() error {
	for _, v := range []float64{e.X, e.Y, e.DX, e.DY} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("non-finite input coordinate")
		}
	}
	switch e.Type {
	case "pointer":
		if e.Buttons > 511 {
			return errors.New("invalid pointer mask")
		}
	case "scroll":
		if math.Abs(e.DX) > 4096 || math.Abs(e.DY) > 4096 {
			return errors.New("scroll delta too large")
		}
	case "key":
		if e.Symbol == 0 {
			return errors.New("empty keysym")
		}
	case "keycode":
		if e.Code > 767 {
			return errors.New("invalid evdev keycode")
		}
	case "reset":
	default:
		return errors.New("unsupported input event")
	}
	return nil
}

type CursorImage struct {
	Pixels                    []byte
	Width, Height             int
	Hotspot                   image.Point
	SourceWidth, SourceHeight int
	ScreenID                  uint32
}
