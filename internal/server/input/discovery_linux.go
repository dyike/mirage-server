package input

import (
	"image"
	"sort"
)

// ListOutputs returns the named outputs currently advertised by the compositor.
// Discovery does not create virtual input devices or change display modes.
func ListOutputs() ([]string, error) {
	outputs, err := DiscoverOutputs()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(outputs))
	for i, o := range outputs {
		names[i] = o.Name
	}
	return names, nil
}

type OutputInfo struct {
	Name   string
	Bounds image.Rectangle
	Pixels image.Point
}

func DiscoverOutputs() ([]OutputInfo, error) {
	w, err := discoverWayland("", "", true)
	if err != nil {
		return nil, err
	}
	defer w.c.Close()
	var names []OutputInfo
	for _, o := range w.outputs {
		if o.name != "" {
			if o.logicalWidth <= 0 || o.logicalHeight <= 0 {
				continue // disabled/disconnecting outputs cannot be captured or routed
			}
			names = append(names, OutputInfo{Name: o.name, Bounds: image.Rect(o.x, o.y, o.x+o.logicalWidth, o.y+o.logicalHeight), Pixels: image.Pt(o.width, o.height)})
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
	return names, nil
}
