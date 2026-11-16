package rfb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// macOS can leave its authentication state unchanged when it receives only
// the SRP selector. net.Pipe deterministically exposes separate writes, unlike
// loopback TCP, which can coalesce them and hide the intermittent failure.
func TestSRPSelectionArrivesWithInitialIdentity(t *testing.T) {
	for _, version := range []string{"003.889", "003.008", "003.003"} {
		t.Run(version, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			server.SetDeadline(time.Now().Add(3 * time.Second))
			result := make(chan error, 1)
			go func() {
				defer server.Close()
				check := func() error {
					if _, err := io.WriteString(server, "RFB "+version+"\n"); err != nil {
						return err
					}
					if _, err := io.ReadFull(server, make([]byte, 12)); err != nil {
						return err
					}
					offer := []byte{1, SecAppleSRP}
					if version == "003.003" {
						offer = u32(uint32(SecAppleSRP))
					}
					if _, err := server.Write(offer); err != nil {
						return err
					}
					first := make([]byte, 1024)
					n, err := server.Read(first)
					if err != nil {
						return err
					}
					first = first[:n]
					if version != "003.003" {
						if n <= 1 || first[0] != SecAppleSRP {
							return fmt.Errorf("SRP selector arrived without its identity: %d bytes", n)
						}
						first = first[1:]
					}
					// Two nested lengths, identity kind, username length/name,
					// and three trailing option bytes, with no second selector.
					if len(first) != 19 || be.Uint32(first) != 15 || be.Uint32(first[4:]) != 11 ||
						be.Uint16(first[8:]) != 0 || be.Uint16(first[10:]) != 4 || !bytes.Equal(first[12:16], []byte("user")) {
						return fmt.Errorf("invalid initial SRP identity framing: %x", first)
					}
					return nil
				}
				result <- check()
			}()
			_, err := NewClient(ctx, client, Config{Apple: true, Username: "user", Security: []uint8{SecAppleSRP}})
			if serverErr := <-result; serverErr != nil {
				t.Fatal(serverErr)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("expected peer close while waiting for challenge, got %v", err)
			}
		})
	}
}
