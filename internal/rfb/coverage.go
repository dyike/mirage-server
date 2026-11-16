package rfb

import (
	"image"
	"sync"
	"time"
)

// coverageCell is the grid, in pixels, on which the client tracks which
// parts of the framebuffer have been painted since the last resize.
const coverageCell = 16

// coverage records the cells that updates have fully painted. macOS in its
// adaptive encodings answers a full-screen request without the menu bar and
// Dock bands, and sends those only when they change; this finds such holes.
//
// With displays of different sizes the framebuffer holds areas no display
// shows, which the server never paints. Once the display rectangles are
// known (setDisplays), a cell needs only its parts on displays: each such
// part painted by one rectangle, a cell outside every display nothing.
type coverage struct {
	cols, rows int
	w, h       int
	painted    []bool
	left       int // unpainted cells

	displays  []image.Rectangle // nil: the whole framebuffer is shown
	need, got []uint32          // per cell, a bit per display it overlaps
}

func (cv *coverage) reset(w, h int) {
	cv.w, cv.h = w, h
	cv.cols, cv.rows = (w+coverageCell-1)/coverageCell, (h+coverageCell-1)/coverageCell
	n := cv.cols * cv.rows
	if cap(cv.painted) < n {
		cv.painted = make([]bool, n)
	}
	cv.painted = cv.painted[:n]
	clear(cv.painted)
	cv.left = n
	cv.displays, cv.need, cv.got = nil, nil, nil
}

func (cv *coverage) cell(x, y int) image.Rectangle {
	return image.Rect(x*coverageCell, y*coverageCell, min((x+1)*coverageCell, cv.w), min((y+1)*coverageCell, cv.h))
}

// setDisplays limits the cells' needs to the given display rectangles, in
// framebuffer pixels. Cells already painted stay painted.
func (cv *coverage) setDisplays(rects []image.Rectangle) {
	bounds := image.Rect(0, 0, cv.w, cv.h)
	var shown []image.Rectangle
	for _, r := range rects {
		if r = r.Intersect(bounds); !r.Empty() && len(shown) < 32 {
			shown = append(shown, r)
		}
	}
	if len(shown) == 0 || len(shown) == 1 && shown[0] == bounds {
		cv.displays, cv.need, cv.got = nil, nil, nil
		return
	}
	n := cv.cols * cv.rows
	cv.displays, cv.need, cv.got = shown, make([]uint32, n), make([]uint32, n)
	for y := range cv.rows {
		for x := range cv.cols {
			i, c := y*cv.cols+x, cv.cell(x, y)
			for k, d := range shown {
				if c.Overlaps(d) {
					cv.need[i] |= 1 << k
				}
			}
			if cv.need[i] == 0 && !cv.painted[i] {
				cv.painted[i] = true
				cv.left--
			}
		}
	}
}

// mark records r as painted: every cell r covers completely, where cells
// at the right and bottom edges end at the framebuffer edge, or with
// displays known, every part of a cell on a display that r covers.
func (cv *coverage) mark(r image.Rectangle) {
	if cv.left == 0 {
		return
	}
	r = r.Intersect(image.Rect(0, 0, cv.w, cv.h))
	if cv.displays != nil {
		cv.markParts(r)
		return
	}
	x0, y0 := (r.Min.X+coverageCell-1)/coverageCell, (r.Min.Y+coverageCell-1)/coverageCell
	x1, y1 := r.Max.X/coverageCell, r.Max.Y/coverageCell
	if r.Max.X == cv.w {
		x1 = cv.cols
	}
	if r.Max.Y == cv.h {
		y1 = cv.rows
	}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if i := y*cv.cols + x; !cv.painted[i] {
				cv.painted[i] = true
				cv.left--
			}
		}
	}
}

func (cv *coverage) markParts(r image.Rectangle) {
	if r.Empty() {
		return
	}
	x0, y0 := r.Min.X/coverageCell, r.Min.Y/coverageCell
	x1, y1 := (r.Max.X+coverageCell-1)/coverageCell, (r.Max.Y+coverageCell-1)/coverageCell
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			i := y*cv.cols + x
			if cv.painted[i] {
				continue
			}
			c := cv.cell(x, y)
			for k, d := range cv.displays {
				if bit := uint32(1) << k; cv.need[i]&bit != 0 && c.Intersect(d).In(r) {
					cv.got[i] |= bit
				}
			}
			if cv.got[i] == cv.need[i] {
				cv.painted[i] = true
				cv.left--
			}
		}
	}
}

// missing returns the unpainted area as horizontal bands: runs of cell rows
// that hold unpainted cells, each narrowed to the columns concerned.
func (cv *coverage) missing() []image.Rectangle {
	if cv.left == 0 {
		return nil
	}
	var bands []image.Rectangle
	var band image.Rectangle
	for y := range cv.rows {
		var row image.Rectangle
		for x := range cv.cols {
			if !cv.painted[y*cv.cols+x] {
				row = row.Union(image.Rect(x, y, x+1, y+1))
			}
		}
		if row.Empty() {
			if !band.Empty() {
				bands = append(bands, band)
				band = image.Rectangle{}
			}
			continue
		}
		band = band.Union(row)
	}
	if !band.Empty() {
		bands = append(bands, band)
	}
	for i, b := range bands {
		bands[i] = image.Rect(b.Min.X*coverageCell, b.Min.Y*coverageCell,
			b.Max.X*coverageCell, b.Max.Y*coverageCell).Intersect(image.Rect(0, 0, cv.w, cv.h))
	}
	return bands
}

// holes hands the reader's view of unpainted bands to the retry timer.
type holes struct {
	mu      sync.Mutex
	bands   []image.Rectangle
	gen     int // bumped on resize: restarts the retry budget
	armed   bool
	tries   int
	triesAt int // gen the tries count belongs to
	// asked is when the last requests went out; zero once an update
	// painted more. Over a slow link a full-screen answer takes seconds,
	// and asking again meanwhile only queues more of them.
	asked time.Time
	left  int // unpainted cells at the last update
}

const (
	holeRetryDelay = 400 * time.Millisecond
	holeRetries    = 10
	holeAnswerWait = 5 * time.Second
)

// noteCoverage runs on the reader after each update: it publishes the
// unpainted bands and arms the retry timer when there are any.
func (c *Client) noteCoverage(resized bool) {
	bands := c.cov.missing()
	h := &c.holes
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bands = append(h.bands[:0], bands...)
	if resized {
		h.gen++
	}
	if resized || c.cov.left < h.left || len(bands) == 0 {
		h.asked = time.Time{} // answered, or nothing to ask for
	}
	h.left = c.cov.left
	if len(bands) > 0 && !h.armed {
		h.armed = true
		time.AfterFunc(holeRetryDelay, c.retryHoles)
	}
}

// retryHoles asks for each unpainted band in full, a bounded number of
// times per framebuffer size.
func (c *Client) retryHoles() {
	h := &c.holes
	h.mu.Lock()
	if h.triesAt != h.gen {
		h.triesAt, h.tries = h.gen, 0
	}
	if len(h.bands) == 0 || h.tries >= holeRetries {
		h.armed = false
		h.mu.Unlock()
		return
	}
	if !h.asked.IsZero() && time.Since(h.asked) < holeAnswerWait {
		h.mu.Unlock()
		time.AfterFunc(holeRetryDelay, c.retryHoles)
		return
	}
	h.tries++
	h.asked = time.Now()
	bands := append([]image.Rectangle(nil), h.bands...)
	h.mu.Unlock()
	for _, b := range bands {
		c.trace("unpainted band %v, asking for it", b)
		if c.requestRect(false, b) != nil {
			return // the reader reports the broken connection
		}
	}
	time.AfterFunc(holeRetryDelay, c.retryHoles)
}
