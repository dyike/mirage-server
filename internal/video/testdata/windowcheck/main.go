//go:build linux && !android

// A real-window smoke check for DMA-BUF import, frame replacement, native
// maximize/restore and teardown. Run from the repository root; no connection
// profiles or credentials are read.
package main

import (
	"encoding/json"
	"fmt"
	"image"
	"os"
	"time"

	"github.com/dyike/keel/gio/f32"
	"github.com/dyike/keel/gio/op"
	"github.com/dyike/keel/gio/op/paint"
	"github.com/dyike/keel/ui/core"
	"github.com/dyike/keel/ui/window"
	"github.com/dyike/mirage-server/internal/video"
)

type nativeView struct {
	surface     paint.Surface
	frame       video.Frame
	retained    [3]uintptr
	closed      bool
	drawn, last uint64
}

func (v *nativeView) Layout(gtx core.C) core.D {
	if v.frame.Surface != 0 {
		scale := min(float32(gtx.Constraints.Max.X)/float32(v.frame.W), float32(gtx.Constraints.Max.Y)/float32(v.frame.H))
		transform := op.Affine(f32.Affine2D{}.Scale(f32.Point{}, f32.Pt(scale, scale))).Push(gtx.Ops)
		paint.NewSurfaceOp(&v.surface).Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		transform.Pop()
		if uint64(v.frame.Surface) != v.last {
			v.drawn++
			v.last = uint64(v.frame.Surface)
		}
	}
	return core.D{Size: gtx.Constraints.Max}
}

func main() {
	view := new(nativeView)
	d, err := video.NewDecoder(video.HEVC, func(f video.Frame) {
		video.RetainBuffer(f.Buffer)
		core.Update(func() {
			if view.closed {
				video.ReleaseBuffer(f.Buffer)
				return
			}
			video.ReleaseBuffer(view.retained[2])
			view.retained = [3]uintptr{f.Buffer, view.retained[0], view.retained[1]}
			view.frame = f
			view.surface.Set(f.Surface, image.Pt(f.W, f.H))
		})
	})
	if err != nil {
		panic(err)
	}
	w := window.Open(window.Options{Title: "Mirage DMA-BUF check", Width: 960, Height: 540, Content: view, OnClose: func() {
		d.Close()
		view.closed = true
		for _, h := range view.retained {
			video.ReleaseBuffer(h)
		}
	}})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for !paint.DMABufSupported() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !paint.DMABufSupported() {
			fmt.Fprintln(os.Stderr, "FAIL: no EGL DMA-BUF importer")
			w.Close()
			return
		}
		d.SetCPUFrames(false)
		stream, err := os.ReadFile("internal/video/testdata/red_4k.hevc")
		if err != nil {
			panic(err)
		}
		nals := video.SplitAnnexB(stream)
		ticker := time.NewTicker(time.Second / 60)
		defer ticker.Stop()
		for i := 0; i < 180; i++ {
			<-ticker.C
			if err := d.Decode(nals); err != nil {
				fmt.Fprintln(os.Stderr, "FAIL:", err)
				break
			}
			if i == 60 || i == 120 {
				core.Update(w.ToggleMaximize)
			}
		}
		d.Close()
		time.Sleep(300 * time.Millisecond)
		core.Update(func() {
			status := d.Status()
			result := struct {
				Status video.DecoderStatus
				Drawn  uint64
			}{status, view.drawn}
			_ = json.NewEncoder(os.Stdout).Encode(result)
			if status.Error != "" || !status.ZeroCopy || status.ReadbackFrames != 0 || view.drawn < 30 {
				fmt.Fprintln(os.Stderr, "FAIL: native presentation check")
			} else {
				fmt.Println("PASS: real-window 4K DMA-BUF replacement, maximize/restore and teardown")
			}
			w.Close()
		})
	}()
	window.Main()
}
