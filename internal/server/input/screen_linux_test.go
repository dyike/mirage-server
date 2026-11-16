package input

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestScreenConstraints(t *testing.T) {
	s := &ScreenCapture{format: ^uint32(0)}
	if err := s.constraint(0, words(3840, 2160)); err != nil {
		t.Fatal(err)
	}
	if err := s.constraint(1, words(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.constraint(4, nil); err != nil {
		t.Fatal(err)
	}
	if s.Width != 3840 || s.Height != 2160 {
		t.Fatal("incorrect dimensions")
	}
	if err := s.constraint(0, words(9000, 2160)); err == nil {
		t.Fatal("oversized buffer accepted")
	}
	if err := s.constraint(5, nil); err == nil {
		t.Fatal("removed output accepted")
	}
	if err := s.constraint(0, []byte{1}); err == nil {
		t.Fatal("truncated event accepted")
	}
}
func TestNativeMissingOutput(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT") == "" {
		t.Skip("requires compositor")
	}
	s, err := OpenScreen(context.Background(), "mirage-output-that-does-not-exist", false)
	if err == nil {
		s.Close()
		t.Fatal("missing output must fail, never capture another output")
	}
}

func TestNativeDMACapture(t *testing.T) {
	name := os.Getenv("MIRAGE_TEST_NATIVE_OUTPUT")
	if name == "" {
		t.Skip("requires compositor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := OpenScreen(ctx, name, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.NewDMAPool(3)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, b := range p.Buffers {
		transform, err := p.Capture(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("DMA fd=%d %dx%d stride=%d format=%x modifier=%x transform=%d", b.FD, b.Width, b.Height, b.Stride, b.Format, b.Modifier, transform)
	}
	if s.pixels != nil || s.buffer != 0 {
		t.Fatal("DMA capture allocated CPU shared memory")
	}
}
