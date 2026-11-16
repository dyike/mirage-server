package rfb

import (
	"testing"

	"github.com/dyike/mirage-server/internal/video"
)

// BenchmarkCopyPath4K is the per-frame CPU work the surface path removes:
// swizzling a 4K BGRA picture into the framebuffer. (The viewer then copied
// the same 33 MB into tile images and uploaded it.)
func BenchmarkCopyPath4K(b *testing.B) {
	const w, h = 3840, 2160
	f := video.Frame{W: w, H: h, Stride: w * 4, Pix: make([]byte, w*h*4)}
	dst := make([]uint32, w*h)
	b.SetBytes(w * h * 4)
	for b.Loop() {
		bgraToRGBA(dst, f)
	}
}
