package capture

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestAudioCapture(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_AUDIO") != "1" {
		t.Skip("opt-in audio backend")
	}
	for _, synthetic := range []bool{true, false} {
		name := "monitor"
		if synthetic {
			name = "synthetic"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := errors.New("complete")
			count := 0
			err := Audio(ctx, 3, 2, 48000, synthetic, func(data []byte) error {
				if len(data) == 0 || len(data)%4 != 0 {
					t.Error("invalid PCM size")
				}
				count++
				if count == 3 {
					return done
				}
				return nil
			})
			if !errors.Is(err, done) {
				t.Fatal(err)
			}
		})
	}
}
