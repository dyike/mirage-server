package video

import (
	"bytes"
	"runtime"
	"testing"
)

func TestCopyBGRACropsRetainedBufferAfterDecoderClose(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("VideoToolbox requires Apple arm64")
	}
	stream, _ := encode(t, H264, 64, 48, 1)
	var retained uintptr
	var original []byte
	d, err := NewDecoder(H264, func(f Frame) {
		RetainBuffer(f.Buffer)
		retained = f.Buffer
		original = make([]byte, f.W*f.H*4)
		for y := range f.H {
			copy(original[y*f.W*4:][:f.W*4], f.Pix[y*f.Stride:][:f.W*4])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	defer func() { ReleaseBuffer(retained) }()
	for _, au := range accessUnits(H264, SplitAnnexB(stream)) {
		if err := d.Decode(au); err != nil {
			t.Fatal(err)
		}
	}
	if retained == 0 {
		t.Fatal("no retained picture")
	}
	d.Close()
	const w, h, stride = 17, 13, 17*4 + 12
	dst := bytes.Repeat([]byte{0xee}, stride*h)
	if err := CopyBGRA(retained, dst, stride, w, h); err != nil {
		t.Fatal(err)
	}
	for y := range h {
		if !bytes.Equal(dst[y*stride:][:w*4], original[y*64*4:][:w*4]) {
			t.Fatal("native crop pixels", y)
		}
		if !bytes.Equal(dst[y*stride+w*4:(y+1)*stride], bytes.Repeat([]byte{0xee}, 12)) {
			t.Fatal("native copy changed padding")
		}
	}
	if CopyBGRA(retained, dst, stride, 65, h) == nil || CopyBGRA(retained, dst[:3], stride, w, h) == nil || CopyBGRA(0, dst, stride, w, h) == nil {
		t.Fatal("invalid native copy accepted")
	}
}
