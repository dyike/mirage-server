package inputtrace

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var cursorTrace struct {
	sync.Mutex
	once sync.Once
	dir  string
	n    int
}

// Cursor captures only cursor shapes, never desktop pixels. Enable explicitly
// with MIRAGE_CURSOR_TRACE=1; each process writes at most 128 samples.
// raw is the Cursor encoding payload (RGBA pixels followed by the bit mask).
func Cursor(stage string, img *image.RGBA, hot image.Point, raw []byte) {
	cursorTrace.once.Do(func() {
		if os.Getenv("MIRAGE_CURSOR_TRACE") != "1" {
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir := filepath.Join(home, "Documents", "mirage-cursor-trace", time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Printf("cursor trace: %v", err)
			return
		}
		cursorTrace.dir = dir
		log.Printf("cursor trace: %s", dir)
	})
	if cursorTrace.dir == "" {
		return
	}
	cursorTrace.Lock()
	defer cursorTrace.Unlock()
	if cursorTrace.n >= 128 {
		return
	}
	cursorTrace.n++
	base := filepath.Join(cursorTrace.dir, fmt.Sprintf("%03d-%s", cursorTrace.n, stage))
	var bounds image.Rectangle
	opaque, black := 0, 0
	if img != nil {
		bounds = img.Bounds()
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				p := img.RGBAAt(x, y)
				if p.A != 0 {
					opaque++
					if p.R == 0 && p.G == 0 && p.B == 0 {
						black++
					}
				}
			}
		}
		if !bounds.Empty() {
			f, err := os.OpenFile(base+".png", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err == nil {
				err = png.Encode(f, img)
				f.Close()
			}
			if err != nil {
				log.Printf("cursor trace png: %v", err)
			}
		}
	}
	meta, _ := json.Marshal(struct {
		Time          time.Time
		Stage         string
		Bounds        image.Rectangle
		Hotspot       image.Point
		Opaque, Black int
	}{time.Now(), stage, bounds, hot, opaque, black})
	if err := os.WriteFile(base+".json", meta, 0600); err != nil {
		log.Printf("cursor trace metadata: %v", err)
	}
	if raw != nil {
		if err := os.WriteFile(base+".bin", raw, 0600); err != nil {
			log.Printf("cursor trace payload: %v", err)
		}
	}
	log.Printf("cursor trace %03d %s bounds=%v hot=%v opaque=%d black=%d", cursorTrace.n, stage, bounds, hot, opaque, black)
}
