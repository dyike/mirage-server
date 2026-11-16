package rfb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"slices"
	"testing"

	"github.com/dyike/mirage-server/internal/frame"
	"github.com/dyike/mirage-server/internal/video"
)

type fakeH264 struct {
	cb            func(video.Frame)
	calls, closed int
}

func (d *fakeH264) Decode(_ [][]byte) error {
	d.calls++
	d.cb(video.Frame{W: 3, H: 2, Stride: 12, Pix: bytes.Repeat([]byte{10, 20, byte(d.calls), 255}, 6)})
	return nil
}
func (d *fakeH264) SetCPUFrames(bool) {}
func (d *fakeH264) Status() video.DecoderStatus {
	return video.DecoderStatus{HardwareKnown: true, Hardware: true, Codec: "H.264"}
}
func (d *fakeH264) Close() { d.closed++ }
func h264Packet(flags uint32, payload []byte) *bufio.Reader {
	var b bytes.Buffer
	var h [8]byte
	be.PutUint32(h[:], uint32(len(payload)))
	be.PutUint32(h[4:], flags)
	b.Write(h[:])
	b.Write(payload)
	return bufio.NewReader(&b)
}
func TestH264AccessUnits(t *testing.T) {
	units, err := h264AccessUnits(h264Probe)
	if err != nil || len(units) != 3 {
		t.Fatalf("units=%d err=%v", len(units), err)
	}
	for _, data := range [][]byte{{0, 0, 1, 0x65}, {0, 0, 1, 2, 128}} {
		if _, err := h264AccessUnits(data); err == nil {
			t.Fatal("malformed slice accepted")
		}
	}
}
func TestH264RectanglesAndReset(t *testing.T) {
	var decoders []*fakeH264
	c := &Client{fb: frame.Framebuffer{W: 4, H: 3, Pix: make([]byte, 4*3*4)}, h264Factory: func(cb func(video.Frame)) (h264Decoder, error) {
		d := &fakeH264{cb: cb}
		decoders = append(decoders, d)
		return d, nil
	}}
	defer c.closeH264Contexts()
	rect := image.Rect(1, 1, 3, 3)
	c.r = h264Packet(0, h264Probe)
	if changed, err := c.decodeH264(rect); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(decoders) != 1 || decoders[0].calls != 3 {
		t.Fatal("did not decode all pictures")
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			i := (y*4 + x) * 4
			want := []byte{0, 0, 0, 0}
			if image.Pt(x, y).In(rect) {
				want = []byte{3, 20, 10, 255}
			}
			if !bytes.Equal(c.fb.Pix[i:i+4], want) {
				t.Fatalf("pixel %d,%d: %v", x, y, c.fb.Pix[i:i+4])
			}
		}
	}
	c.r = h264Packet(1, nil)
	if changed, err := c.decodeH264(rect); err != nil || changed {
		t.Fatal(changed, err)
	}
	if decoders[0].closed != 1 || len(c.h264Contexts) != 0 {
		t.Fatal("reset did not close context")
	}
	c.h264Context(rect)
	c.h264Context(image.Rect(0, 0, 1, 1))
	c.r = h264Packet(2, nil)
	if _, err := c.decodeH264(rect); err != nil {
		t.Fatal(err)
	}
	if len(c.h264Contexts) != 0 {
		t.Fatal("reset all did not close contexts")
	}
}
func TestH264ContextLimit(t *testing.T) {
	c := &Client{h264Factory: func(cb func(video.Frame)) (h264Decoder, error) { return &fakeH264{cb: cb}, nil }}
	defer c.closeH264Contexts()
	for x := 0; x < 64; x++ {
		c.h264Context(image.Rect(x, 0, x+1, 1))
	}
	c.h264Context(image.Rect(0, 0, 1, 1))
	c.h264Context(image.Rect(64, 0, 65, 1))
	if len(c.h264Contexts) != 64 || c.h264Contexts[image.Rect(1, 0, 2, 1)] != nil || c.h264Contexts[image.Rect(0, 0, 1, 1)] == nil {
		t.Fatal("context LRU invalid")
	}
}
func TestH264HardwareProbe(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_LINUX_VIDEO") != "1" {
		t.Skip("opt-in GPU test")
	}
	c := &Client{cfg: Config{Encodings: []int32{EncH264, EncTight}}, authContext: context.Background()}
	c.prepareH264()
	if c.h264Disabled {
		t.Fatalf("hardware probe failed: %+v", c.h264Status.Load())
	}
	t.Logf("hardware: %+v", c.h264Status.Load())
}

func TestH264UnavailablePreservesFallbackOrder(t *testing.T) {
	encodings := []int32{EncH264, EncTight, EncCopyRect, EncRaw}
	c := &Client{cfg: Config{Encodings: encodings}, authContext: context.Background(), h264Factory: func(func(video.Frame)) (h264Decoder, error) { return nil, video.ErrUnsupported }}
	c.prepareH264()
	if !c.h264Disabled || len(c.cfg.Encodings) != 3 || c.cfg.Encodings[0] != EncTight || encodings[0] != EncH264 {
		t.Fatal("failed probe changed fallback or caller preferences")
	}
	if c.VideoStatus().Error == "" {
		t.Fatal("missing probe failure")
	}
}

func BenchmarkOrderedVideoCPUOutput4K(b *testing.B) {
	pixels := make([]byte, 3840*2160*4)
	d := &orderedH264{ready: make(chan h264Result, 1)}
	f := video.Frame{W: 3840, H: 2160, Stride: 3840 * 4, Pix: pixels}
	b.ReportAllocs()
	b.SetBytes(int64(len(pixels)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.waiting = true
		d.output(f)
		<-d.ready
	}
}

func TestOrderedVideoCPUOutputOwnsAndReusesPixels(t *testing.T) {
	d := &orderedH264{ready: make(chan h264Result, 1)}
	send := func(w, h int, pixels []byte) video.Frame {
		t.Helper()
		d.waiting = true
		d.output(video.Frame{W: w, H: h, Stride: w * 4, Pix: pixels})
		r := <-d.ready
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.frame
	}
	source := bytes.Repeat([]byte{10, 20, 30, 255}, 6)
	first := send(3, 2, source)
	saved := append([]byte(nil), first.Pix...)
	source[0] = 99
	if first.Pix[0] != 10 {
		t.Fatal("output aliases platform buffer")
	}
	second := send(3, 2, source)
	if &first.Pix[0] != &second.Pix[0] || second.Pix[0] != 99 {
		t.Fatal("same-size output did not reuse storage")
	}
	if saved[0] != 10 {
		t.Fatal("consumer snapshot changed")
	}
	resized := send(4, 3, bytes.Repeat([]byte{1, 2, 3, 255}, 12))
	if len(resized.Pix) != 48 || resized.Pix[0] != 1 {
		t.Fatal("invalid resized picture")
	}
	shrunk := send(1, 1, []byte{4, 5, 6, 255})
	if len(shrunk.Pix) != 4 || shrunk.Pix[0] != 4 {
		t.Fatal("invalid smaller picture")
	}
}

func TestHEVCFailurePreservesH264AndSkipsQueuedHEVC(t *testing.T) {
	offers := []int32{EncMirageHEVC, EncH264, EncTight, EncRaw}
	conn := &keyWriteConn{}
	c := &Client{conn: conn, hevcActive: true, cfg: Config{Encodings: offers}, h264Factory: func(cb func(video.Frame)) (h264Decoder, error) { return &fakeH264{cb: cb}, nil }}
	c.fb.Resize(3, 2)
	defer c.closeH264Contexts()
	if err := c.h264Fallback(errors.New("HEVC output failed")); err != nil {
		t.Fatal(err)
	}
	if c.h264Disabled || !c.hevcDisabled || !slices.Equal(c.cfg.Encodings, []int32{EncH264, EncTight, EncRaw}) || offers[0] != EncMirageHEVC {
		t.Fatal("HEVC failure disabled H264 or mutated offers")
	}
	if c.VideoStatus().Codec != "HEVC" {
		t.Fatal(c.VideoStatus())
	}
	c.r = h264Packet(0, []byte{1, 2, 3})
	if changed, err := c.decodeH264(image.Rect(0, 0, 3, 2)); changed || err != nil {
		t.Fatal("queued HEVC was decoded", changed, err)
	}
	c.hevcActive = false
	c.r = h264Packet(2, h264Probe)
	if changed, err := c.decodeH264(image.Rect(0, 0, 3, 2)); !changed || err != nil {
		t.Fatal("H264 recovery failed", changed, err)
	}
	if c.VideoStatus().Error != "" || !c.VideoStatus().Hardware {
		t.Fatal(c.VideoStatus())
	}
	if err := c.h264Fallback(errors.New("H264 also failed")); err != nil {
		t.Fatal(err)
	}
	if !c.h264Disabled || !slices.Equal(c.cfg.Encodings, []int32{EncTight, EncRaw}) {
		t.Fatal("second failure did not select image encoding")
	}
}

func TestVideoRecoveryWaitsForIndependentPictureAndIsBounded(t *testing.T) {
	r := image.Rect(0, 0, 3, 2)
	calls := 0
	c := &Client{conn: &keyWriteConn{}, cfg: Config{Encodings: []int32{EncH264, EncTight}}, h264Factory: func(cb func(video.Frame)) (h264Decoder, error) {
		calls++
		return &fakeH264{cb: cb}, nil
	}}
	c.fb.Resize(3, 2)
	c.h264Frames.Store(1)
	defer c.closeH264Contexts()
	if err := c.recoverVideo(r, errors.New("temporary decoder failure")); err != nil {
		t.Fatal(err)
	}
	if c.h264Disabled {
		t.Fatal("transient error disabled hardware video")
	}
	c.r = h264Packet(0, []byte{0, 0, 1, 0x41, 0x80})
	if changed, err := c.decodeH264(r); changed || err != nil || calls != 0 {
		t.Fatal("dependent frame reached fresh decoder", changed, err, calls)
	}
	c.r = h264Packet(2, h264Probe)
	if changed, err := c.decodeH264(r); !changed || err != nil || calls != 1 {
		t.Fatal("keyframe did not recover", changed, err, calls)
	}
	if c.VideoStatus().Error != "" {
		t.Fatal("successful recovery retained error")
	}
	for range 2 {
		if err := c.recoverVideo(r, errors.New("persistent failure")); err != nil {
			t.Fatal(err)
		}
	}
	if !c.h264Disabled {
		t.Fatal("persistent failure retries were unbounded")
	}
}

func TestVideoRestartPictureRequiresParameters(t *testing.T) {
	if videoRestartPicture([][][]byte{{{0x65, 0x80}}}, false) {
		t.Fatal("H264 keyframe without sets accepted")
	}
	if videoRestartPicture([][][]byte{{{19 << 1, 1, 128}}}, true) {
		t.Fatal("HEVC keyframe without sets accepted")
	}
	if !videoRestartPicture([][][]byte{{{32 << 1, 1}, {33 << 1, 1}, {34 << 1, 1}, {19 << 1, 1, 128}}}, true) {
		t.Fatal("complete HEVC restart rejected")
	}
}
