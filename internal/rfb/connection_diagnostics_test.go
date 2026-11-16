package rfb

import (
	"bytes"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
)

func TestHandshakeEOFIdentifiesStageAndPreservesCause(t *testing.T) {
	version := func(s *fakeServer) { s.write([]byte("RFB 003.008\n")); s.read(12) }
	none := func(s *fakeServer) { version(s); s.write([]byte{1, SecNone}); s.read(1) }
	for _, test := range []struct {
		stage   string
		partial bool
		serve   func(*fakeServer)
	}{
		{"server-greeting", false, func(s *fakeServer) {}},
		{"server-greeting", true, func(s *fakeServer) { s.write([]byte("RFB ")) }},
		{"security-offer", false, version},
		{"vnc-authentication", false, func(s *fakeServer) { version(s); s.write([]byte{1, SecVNC}); s.read(1) }},
		{"security-result", false, none},
		{"server-init", false, func(s *fakeServer) { none(s); s.write(u32(0)); s.read(1) }},
		{"desktop-name", false, func(s *fakeServer) {
			none(s)
			s.write(u32(0))
			s.read(1)
			header := make([]byte, 24)
			be.PutUint16(header, 1)
			be.PutUint16(header[2:], 1)
			be.PutUint32(header[20:], 4)
			s.write(header)
		}},
	} {
		t.Run(test.stage+map[bool]string{true: "/partial", false: ""}[test.partial], func(t *testing.T) {
			c, err := pipe(t, Config{}, test.serve)
			var failed *HandshakeError
			if c != nil || !errors.As(err, &failed) || failed.Stage != test.stage {
				t.Fatalf("stage not preserved: client=%v error=%v", c, err)
			}
			cause := io.EOF
			if test.partial {
				cause = io.ErrUnexpectedEOF
			}
			if !errors.Is(err, cause) || failed.Elapsed < 0 || failed.StageElapsed < 0 {
				t.Fatalf("transport cause/timing lost: %v", err)
			}
		})
	}
}

func TestConnectionLogsDoNotExposeServerReasonOrCredentials(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	c := &Client{cfg: Config{ConnectionDiagnostics: true, Username: "private-account", Password: "private-password"}}
	p := newHandshakeProgress(c, "srp-authentication")
	err := p.finish(&AuthError{Reason: "private-account private-password"})
	if _, ok := err.(*AuthError); !ok {
		t.Fatal("authentication error type changed")
	}
	for _, secret := range []string{"private-account", "private-password"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("connection diagnostic leaked authentication material")
		}
	}
	if !strings.Contains(output.String(), "error=authentication_rejected") || !strings.Contains(output.String(), "stage=srp-authentication") {
		t.Fatalf("missing safe stage/classification: %s", output.String())
	}
}

func TestRunEOFBeforeFirstPictureIsDiagnosed(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(old)
	c, err := pipe(t, Config{ConnectionDiagnostics: true, InFlight: 1}, func(s *fakeServer) {
		s.write([]byte("RFB 003.008\n"))
		s.read(12)
		s.write([]byte{1, SecNone})
		s.read(1)
		s.write(u32(0))
		s.init(1, 1)
		s.expectRequest(false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Run(); !errors.Is(err, io.EOF) {
		t.Fatalf("lost stream EOF: %v", err)
	}
	if !strings.Contains(output.String(), "stage=first-picture status=failed") || !strings.Contains(output.String(), "error=eof") {
		t.Fatalf("missing first-picture diagnosis: %s", output.String())
	}
}
