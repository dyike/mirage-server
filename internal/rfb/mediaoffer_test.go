package rfb

import (
	"bytes"
	"compress/zlib"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryPlist checks the encoder against macOS's plutil.
func TestBinaryPlist(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	long := strings.Repeat("x", 300)
	b := binaryPlist([]plistEntry{
		{"data", bytes.Repeat([]byte{1}, 20)},
		{"small", int64(7)},
		{"big", int64(70000)},
		{"name", long},
	})
	path := filepath.Join(t.TempDir(), "p.plist")
	os.WriteFile(path, b, 0o600)
	out, err := exec.Command("plutil", "-convert", "xml1", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil: %v\n%s", err, out)
	}
	for _, want := range []string{"<key>data</key>", "<integer>7</integer>", "<integer>70000</integer>", long} {
		if !strings.Contains(string(out), want) {
			t.Errorf("plutil output lacks %q:\n%s", want, out)
		}
	}
}

func TestMediaOfferMessage(t *testing.T) {
	m := newMediaOffer(negotiatorNearby, []int{payloadHEVC}, true).message()
	if m[0] != msgAppleMediaStream || int(be.Uint16(m[2:])) != len(m)-4 {
		t.Fatalf("header % x", m[:8])
	}
	audio, video := int(be.Uint16(m[0x0a:])), int(be.Uint16(m[0x0c:]))
	if len(m) != 0x80+audio+2*mediaKeyLen+video {
		t.Errorf("length %d, want %d", len(m), 0x80+audio+2*mediaKeyLen+video)
	}
	if !bytes.HasPrefix(m[0x80:], []byte("bplist00")) || !bytes.HasPrefix(m[0x80+audio+2*mediaKeyLen:], []byte("bplist00")) {
		t.Error("offers are not binary plists")
	}
}

// The daemon forwards this word in host (little-endian) order to the
// agent. The no-cursor bit must be in byte 6, not byte 9, or the remote
// video can contain a second cursor alongside the viewer's local one.
func TestMediaOfferDisablesEmbeddedCursor(t *testing.T) {
	for _, fps60 := range []bool{false, true} {
		m := newMediaOffer(negotiatorNearby, []int{payloadHEVC}, fps60).message()
		want := byte(mediaFlagNoCursor)
		if fps60 {
			want |= byte(mediaFlag60FPS)
		}
		if !bytes.Equal(m[6:10], []byte{want, 0, 0, 0}) {
			t.Errorf("fps60=%v: media flags % x, want %02x 00 00 00", fps60, m[6:10], want)
		}
	}
}

func TestPlistDataRoundTrip(t *testing.T) {
	blob := bytes.Repeat([]byte{9}, 300)
	b := binaryPlist([]plistEntry{{"a", int64(5)}, {"avcMediaStreamNegotiatorMediaBlob", blob}, {"z", "x"}})
	got, ok := plistData(b, "avcMediaStreamNegotiatorMediaBlob")
	if !ok || !bytes.Equal(got, blob) {
		t.Fatalf("plistData = %v, %d bytes", ok, len(got))
	}
	if _, ok := plistData(b, "missing"); ok {
		t.Error("found a missing key")
	}
}

func TestParseMediaAnswer(t *testing.T) {
	var s pbuf
	s.uint(1, 0x1234)
	s.uint(4, 3840)
	s.uint(5, 2160)
	s.uint(6, 1)
	s.uint(7, 1)
	var blob pbuf
	blob.uint(1, 1)
	blob.bytes(5, s)
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write(blob)
	w.Close()
	video := binaryPlist([]plistEntry{{"avcMediaStreamNegotiatorMediaBlob", z.Bytes()}})
	body := make([]byte, 18)
	be.PutUint16(body, 2)
	be.PutUint16(body[10:], uint16(len(video)))
	a, err := parseMediaAnswer(append(body, video...))
	if err != nil || a.ssrc != 0x1234 || a.width != 3840 || a.height != 2160 || a.tiles != 1 || !a.ltrp {
		t.Fatalf("answer %+v, %v", a, err)
	}
}
