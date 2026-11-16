package video

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/frame"
)

// encode makes a short test clip with ffmpeg and returns its Annex B
// stream (with access unit delimiters) and ffmpeg's own BGRA decoding.
func encode(t *testing.T, c Codec, w, h, frames int) (stream, ref []byte) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	es := filepath.Join(dir, "clip.es")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc2=size=" + itoa(w) + "x" + itoa(h) + ":rate=30", "-frames:v", itoa(frames), "-pix_fmt", "yuv420p"}
	if c == HEVC {
		args = append(args, "-c:v", "libx265", "-x265-params", "aud=1:bframes=0:log-level=error", "-f", "hevc")
	} else {
		args = append(args, "-c:v", "libx264", "-bf", "0", "-x264-params", "aud=1", "-f", "h264")
	}
	run(t, append(args, es)...)
	raw := filepath.Join(dir, "clip.bgra")
	run(t, "-hide_banner", "-loglevel", "error", "-i", es, "-f", "rawvideo", "-pix_fmt", "bgra", raw)
	stream, _ = os.ReadFile(es)
	ref, _ = os.ReadFile(raw)
	return stream, ref
}

func run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// accessUnits groups NAL units at access unit delimiters.
func accessUnits(c Codec, nals [][]byte) [][][]byte {
	aud := 9
	if c == HEVC {
		aud = 35
	}
	var aus [][][]byte
	for _, n := range nals {
		if NALType(c, n) == aud || len(aus) == 0 {
			aus = append(aus, nil)
			if NALType(c, n) == aud {
				continue
			}
		}
		aus[len(aus)-1] = append(aus[len(aus)-1], n)
	}
	return aus
}

func testDecode(t *testing.T, c Codec) {
	const w, h, frames = 320, 240, 5
	stream, ref := encode(t, c, w, h, frames)
	var got [][]byte
	pictures := make(chan []byte, 8)
	timestamps := make(chan frame.Timing, 8)
	d, err := NewDecoder(c, func(f Frame) {
		if f.W != w || f.H != h {
			t.Errorf("frame %dx%d", f.W, f.H)
			return
		}
		pix := make([]byte, 0, w*h*4)
		for y := range h {
			pix = append(pix, f.Pix[y*f.Stride:y*f.Stride+w*4]...)
		}
		select {
		case pictures <- pix:
			timestamps <- f.Timing
		default:
			t.Error("unexpected extra decoded picture")
		}
	})
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skipf("platform decoder unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer d.Close()
	for _, au := range accessUnits(c, SplitAnnexB(stream)) {
		sourceTime := frame.Timing{Source: time.Now().Add(-100 * time.Millisecond), ClockSynced: true, ClockError: 2 * time.Millisecond}
		timed, supportsTime := any(d).(interface {
			DecodeTimed([][]byte, frame.Timing) error
		})
		var err error
		if supportsTime {
			err = timed.DecodeTimed(au, sourceTime)
		} else {
			err = d.Decode(au)
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case pix := <-pictures:
			got = append(got, pix)
			if at := <-timestamps; supportsTime && at != sourceTime {
				t.Fatalf("decoded picture lost its exact source time: got %v, want %v", at, sourceTime)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no decoded picture: %+v", d.Status())
		}
	}
	if len(got) != frames {
		t.Fatalf("%d frames decoded, want %d", len(got), frames)
	}
	status := d.Status()
	if status.Codec != c.String() || status.Error != "" {
		t.Fatalf("hardware query did not produce an actual decoder state: %+v", status)
	}
	// YUV to RGB conversion differs slightly between decoders.
	for i, pix := range got {
		want := ref[i*w*h*4 : (i+1)*w*h*4]
		var sum int
		for j := range pix {
			if j%4 == 3 {
				continue
			}
			d := int(pix[j]) - int(want[j])
			sum += max(d, -d)
		}
		if avg := float64(sum) / float64(w*h*3); avg > 6 {
			t.Errorf("frame %d differs from ffmpeg by %.1f on average", i, avg)
		}
	}
}

func TestDecodeH264(t *testing.T) { testDecode(t, H264) }
func TestDecodeHEVC(t *testing.T) { testDecode(t, HEVC) }

func TestSplitAnnexB(t *testing.T) {
	got := SplitAnnexB([]byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65})
	want := [][]byte{{0x67, 1, 2}, {0x68, 3}, {0x65}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("NAL %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// BenchmarkDecode4K decodes a 4K HEVC clip; each op is one frame, with the
// BGRA rows read the way a viewer would copy them out.
func BenchmarkDecode4K(b *testing.B) {
	stream, _ := encodeOnly(b, HEVC, 3840, 2160, 30)
	aus := accessUnits(HEVC, SplitAnnexB(stream))
	var sink byte
	d, err := NewDecoder(HEVC, func(f Frame) { sink ^= f.Pix[len(f.Pix)-1] })
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%len(aus) == 0 && i > 0 {
			d.Close()
			d, _ = NewDecoder(HEVC, func(f Frame) { sink ^= f.Pix[len(f.Pix)-1] })
		}
		if err := d.Decode(aus[i%len(aus)]); err != nil {
			b.Fatal(err)
		}
	}
}

func encodeOnly(b *testing.B, c Codec, w, h, frames int) ([]byte, error) {
	dir := b.TempDir()
	es := filepath.Join(dir, "clip.es")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "testsrc2=size=" + itoa(w) + "x" + itoa(h) + ":rate=30", "-frames:v", itoa(frames), "-pix_fmt", "yuv420p",
		"-preset", "ultrafast"}
	if c == HEVC {
		args = append(args, "-c:v", "libx265", "-x265-params", "aud=1:bframes=0:log-level=error", "-f", "hevc")
	} else {
		args = append(args, "-c:v", "libx264", "-bf", "0", "-x264-params", "aud=1", "-f", "h264")
	}
	out, err := exec.Command("ffmpeg", append(args, es)...).CombinedOutput()
	if err != nil {
		b.Skipf("ffmpeg: %v %s", err, out)
	}
	return os.ReadFile(es)
}

// BenchmarkDecodeSteadyState repeats one independently decodable picture
// after warming the session. It includes purego and VideoToolbox calls,
// excluding stream splitting, decoder creation and format changes.
func BenchmarkDecodeSteadyState(b *testing.B) {
	for _, codec := range []Codec{H264, HEVC} {
		b.Run(codec.String(), func(b *testing.B) {
			stream, err := encodeOnly(b, codec, 320, 240, 1)
			if err != nil {
				b.Fatal(err)
			}
			aus := accessUnits(codec, SplitAnnexB(stream))
			if len(aus) != 1 {
				b.Fatalf("encoded %d access units, want 1", len(aus))
			}
			decoder, err := NewDecoder(codec, func(Frame) {})
			if err != nil {
				b.Fatal(err)
			}
			defer decoder.Close()
			decoder.SetCPUFrames(false)
			if err := decoder.Decode(aus[0]); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := decoder.Decode(aus[0]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestSurfaceFrames: frames carry an IOSurface a renderer can draw, and
// without CPU frames no pixels are mapped.
func TestSurfaceFrames(t *testing.T) {
	if (runtime.GOOS != "darwin" && runtime.GOOS != "ios") || runtime.GOARCH != "arm64" {
		t.Skip("native surface ownership requires Apple arm64")
	}
	stream, reference := encode(t, HEVC, 320, 240, 2)
	var held []uintptr
	d, err := NewDecoder(HEVC, func(f Frame) {
		if f.Surface == 0 || f.Buffer == 0 || f.Pix != nil {
			t.Errorf("frame: surface %#x buffer %#x pix %d bytes", f.Surface, f.Buffer, len(f.Pix))
		}
		RetainBuffer(f.Buffer)
		held = append(held, f.Buffer)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetCPUFrames(false)
	for _, au := range accessUnits(HEVC, SplitAnnexB(stream)) {
		if err := d.Decode(au); err != nil {
			t.Fatal(err)
		}
	}
	if len(held) != 2 {
		t.Fatalf("%d frames", len(held))
	}
	for frameIndex, buffer := range held {
		img := Snapshot(buffer)
		ReleaseBuffer(buffer)
		if img == nil || img.Bounds().Dx() != 320 || img.Bounds().Dy() != 240 {
			t.Fatalf("invalid snapshot for frame %d", frameIndex)
		}
		want := reference[frameIndex*320*240*4:][:320*240*4]
		var totalDifference int
		for pixel := 0; pixel < len(want); pixel += 4 {
			for channel := range 3 {
				difference := int(img.Pix[pixel+channel]) - int(want[pixel+2-channel])
				totalDifference += max(difference, -difference)
			}
			if img.Pix[pixel+3] != 255 {
				t.Fatal("snapshot is not opaque")
			}
		}
		if average := float64(totalDifference) / (320 * 240 * 3); average > 3 {
			t.Errorf("snapshot color difference %.2f", average)
		}
	}
}
