package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/dyike/mirage-server/internal/frame"
	"image"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/rfb"
)

type srpTamperConn struct {
	net.Conn
	serverProof, clientA bool
}

func (c *srpTamperConn) Write(b []byte) (int, error) {
	b = bytes.Clone(b)
	if c.serverProof && len(b) == 96 && networkOrder.Uint32(b) == 92 && b[8] == 64 {
		b[9] ^= 1
	}
	if c.clientA && len(b) > 520 && networkOrder.Uint32(b) == uint32(len(b)-4) && networkOrder.Uint16(b[8:]) == 512 {
		clear(b[10:522])
	}
	return c.Conn.Write(b)
}

type srpTamperListener struct {
	net.Listener
	proof bool
}

func (l srpTamperListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &srpTamperConn{Conn: c, serverProof: l.proof}, nil
}

func TestAppleSRPMutualAuthentication(t *testing.T) {
	cred, err := NewSRPCredential("user", []byte("correct-password"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, password   string
		badProof, badA, wantOK bool
	}{
		{name: "success", user: "user", password: "correct-password", wantOK: true},
		{name: "wrong-password", user: "user", password: "wrong"},
		{name: "unknown-user", user: "other", password: "correct-password"},
		{name: "forged-server-proof", user: "user", password: "correct-password", badProof: true},
		{name: "zero-client-public-value", user: "user", password: "correct-password", badA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg, _ := testTLS(t)
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := RFBServer{ARD: true, TLS: cfg, Width: 64, Height: 64, SRPCredential: func() (*SRPCredential, error) { return cred, nil }, Source: func(ctx context.Context, _ string, send func([]byte) error) error {
				for {
					if err := send(bytes.Repeat([]byte{1, 2, 3, 255}, 64*64)); err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(10 * time.Millisecond):
					}
				}
			}, OnError: func(e error) { t.Log(e) }}
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx, srpTamperListener{l, tc.badProof}) }()
			defer func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}()
			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			updates := make(chan struct{}, 1)
			c, err := rfb.NewClient(ctx, &srpTamperConn{Conn: conn, clientA: tc.badA}, rfb.Config{Apple: true, Username: tc.user, Password: tc.password, Security: []uint8{rfb.SecAppleSRP}, Encodings: []int32{rfb.EncRaw}, NoAudio: true, OnUpdate: func(*frame.Framebuffer, []image.Rectangle) {
				select {
				case updates <- struct{}{}:
				default:
				}
			}})
			if tc.wantOK {
				if err != nil {
					t.Fatal(err)
				}
				runDone := make(chan error, 1)
				go func() { runDone <- c.Run() }()
				select {
				case <-updates:
				case <-ctx.Done():
					t.Error("no frame over SRP-derived encrypted record layer")
				}
				c.Close()
				<-runDone
			} else {
				if err == nil {
					c.Close()
					t.Fatal("invalid authentication accepted")
				}
				if tc.badProof && !strings.Contains(err.Error(), "proof does not match") {
					t.Fatalf("server proof not rejected: %v", err)
				}
			}
		})
	}
}

func TestSRPRejectsLegacyAndMalformedIdentity(t *testing.T) {
	cases := map[string][]byte{"legacy": {30}, "oversized": {36, 0xff, 0xff, 0xff, 0xff}, "short": {36, 0, 0, 0, 3}, "inner-mismatch": {36, 0, 0, 0, 4, 0, 0, 0, 1}}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			a, c := net.Pipe()
			defer a.Close()
			defer c.Close()
			a.SetDeadline(time.Now().Add(time.Second))
			c.SetDeadline(time.Now().Add(time.Second))
			s := RFBServer{SRPCredential: func() (*SRPCredential, error) {
				t.Error("malformed request reached credentials")
				return nil, errors.New("unexpected")
			}}
			done := make(chan error, 1)
			go func() { _, e := s.authenticateSRP(a); done <- e }()
			offer, e := readBytes(c, 2)
			if e != nil || !bytes.Equal(offer, []byte{1, 36}) {
				t.Fatalf("legacy downgrade advertised: %v %v", offer, e)
			}
			if _, e = c.Write(b); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("accepted malformed input")
			}
		})
	}
}

func TestSRPCredentialPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "srp.json")
	a, err := NewSRPCredential("user", []byte("password-one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveSRPCredential(path, a); err != nil {
		t.Fatal(err)
	}
	b, err := LoadSRPCredential(path)
	if err != nil || !bytes.Equal(a.Verifier, b.Verifier) {
		t.Fatalf("load: %v", err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("password-one")) {
		t.Fatal("plaintext saved")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadSRPCredential(path); err == nil {
		t.Fatal("public verifier accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, []byte("{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadSRPCredential(path); err == nil {
		t.Fatal("trailing data accepted")
	}
	a.Verifier = make([]byte, 512)
	if err = SaveSRPCredential(path, a); err == nil {
		t.Fatal("zero verifier accepted")
	}
}

// The digest is calculated independently from RFC 5054 appendix A section 5,
// not from the implementation's group constant. Native Apple clients reject
// groups outside their allowlist before sending a password proof.
func TestSRPGroupMatchesRFC5054(t *testing.T) {
	n := srpGroup()
	if n.BitLen() != 4096 {
		t.Fatalf("nonstandard group: %d bits", n.BitLen())
	}
	const want = "4ee95187682bcb230ad26a95205f6920e84708f6251b3894329b09ec23919e33"
	if got := fmt.Sprintf("%x", sha256.Sum256(n.Bytes())); got != want {
		t.Fatalf("RFC 5054 group mismatch: %s", got)
	}
}

func TestSRPRejectsObsoleteGroupCredential(t *testing.T) {
	c, err := NewSRPCredential("user", []byte("password"))
	if err != nil {
		t.Fatal(err)
	}
	c.Version = 1
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "re-enroll") {
		t.Fatalf("old group credential accepted: %v", err)
	}
}

func TestAppleSRPNegotiatedOptions(t *testing.T) {
	for _, s := range []string{srpOptions, "mda=SHA-512,replay_detection,confidentiality+integrity=ChaCha20-Poly1305,maxbuffersize=65536,kdf=SALTED-SHA512-PBKDF2"} {
		if !validSRPOptions(s) {
			t.Errorf("Apple options rejected: %s", s)
		}
	}
	for _, s := range []string{"mda=SHA-512", srpOptions + ",mda=SHA-512", strings.ReplaceAll(srpOptions, "SHA-512,", "SHA-1,"), srpOptions + ",maxbuffersize=0", srpOptions + ",unknown=1"} {
		if validSRPOptions(s) {
			t.Errorf("unsafe/invalid options accepted: %s", s)
		}
	}
}
