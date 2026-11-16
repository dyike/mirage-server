package input

import (
	"errors"
	"image"
)

// One virtual pointer spans the compositor desktop, so a drag can cross output
// boundaries without releasing/re-pressing its buttons on different devices.
type MultiController struct {
	names      []string
	options    Options
	controller *Controller
	outputs    map[uint32]image.Rectangle
	desktop    image.Rectangle
	transform  uint32
	single     bool
}

func OpenOutputs(outputs []string, options Options) (*MultiController, error) {
	if len(outputs) == 0 || len(outputs) > 16 {
		return nil, errors.New("invalid output list")
	}
	m := &MultiController{names: append([]string(nil), outputs...), options: options, outputs: map[uint32]image.Rectangle{}, transform: options.Transform, single: len(outputs) == 1}
	if m.single {
		options.Output = outputs[0]
	} else {
		options.Output = "*"
		options.Transform = 0
	}
	c, err := Open(options)
	if err != nil {
		return nil, err
	}
	m.controller = c
	if m.single {
		m.outputs[1] = image.Rect(0, 0, 1, 1)
		return m, nil
	}
	w := c.native.(*wayland)
	available := map[string]image.Rectangle{}
	for _, o := range w.outputs {
		if o.logicalWidth > 0 && o.logicalHeight > 0 {
			r := image.Rect(o.x, o.y, o.x+o.logicalWidth, o.y+o.logicalHeight)
			available[o.name] = r
			m.desktop = m.desktop.Union(r)
		}
	}
	for i, name := range outputs {
		r, ok := available[name]
		if !ok {
			c.Close()
			return nil, errors.New("shared output has no XDG logical geometry")
		}
		m.outputs[uint32(i+1)] = r
	}
	return m, nil
}
func (m *MultiController) Apply(e Event) error { return m.controller.Apply(e) }
func (m *MultiController) PointerOnScreen(id uint32, x, y float64, mask uint16) error {
	return m.eventOnScreen(id, Event{Type: "pointer", X: x, Y: y, Buttons: mask})
}
func (m *MultiController) ScrollOnScreen(id uint32, e Event) error {
	return m.eventOnScreen(id, e)
}
func (m *MultiController) eventOnScreen(id uint32, e Event) error {
	r, ok := m.outputs[id]
	if !ok {
		return errors.New("unknown output")
	}
	if !m.single {
		x, y := TransformPoint(e.X, e.Y, m.transform)
		e.X = (float64(r.Min.X-m.desktop.Min.X) + x*float64(r.Dx())) / float64(m.desktop.Dx())
		e.Y = (float64(r.Min.Y-m.desktop.Min.Y) + y*float64(r.Dy())) / float64(m.desktop.Dy())
	}
	return m.controller.Apply(e)
}
func (m *MultiController) LEDs() uint32 { return m.controller.LEDs() }
func (m *MultiController) Close() error { return m.controller.Close() }

func (m *MultiController) RefreshOutputs() error {
	next, err := OpenOutputs(m.names, m.options)
	if err != nil {
		return err
	}
	m.controller.Close()
	*m = *next
	return nil
}
