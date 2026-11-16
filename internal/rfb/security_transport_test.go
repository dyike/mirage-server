package rfb

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestVeNCryptCertificateSubtypes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	sum := sha256.Sum256(der)
	for _, subtype := range []uint32{257, 258, 259, 260, 261, 262} {
		t.Run(fmt.Sprint(subtype), func(t *testing.T) {
			user, password := "测试者", "long-password-超过八字节"
			c, err := pipe(t, Config{Username: user, Password: password, ServerFingerprint: hex.EncodeToString(sum[:])}, func(s *fakeServer) {
				s.write([]byte("RFB 003.008\n"))
				s.read(12)
				s.write([]byte{1, SecVeNCrypt})
				if s.read(1)[0] != SecVeNCrypt {
					t.Error("wrong security selection")
					return
				}
				s.write([]byte{0, 2})
				if !bytes.Equal(s.read(2), []byte{0, 2}) {
					t.Error("wrong version")
					return
				}
				s.write([]byte{0, 1}, u32(subtype))
				if be.Uint32(s.read(4)) != subtype {
					t.Error("wrong subtype")
					return
				}
				s.write([]byte{1})
				secured := tls.Server(s.conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				if err := secured.Handshake(); err != nil {
					t.Error(err)
					return
				}
				s.conn = secured
				switch subtype {
				case 258, 261:
					challenge := bytes.Repeat([]byte{0xab}, 16)
					s.write(challenge)
					want, _ := vncAuthResponse(password, challenge)
					if !bytes.Equal(s.read(16), want) {
						t.Error("wrong VNC response")
					}
				case 259, 262:
					header := s.read(8)
					if be.Uint32(header) != uint32(len(user)) || be.Uint32(header[4:]) != uint32(len(password)) {
						t.Error("credential lengths must count UTF-8 bytes")
						return
					}
					if string(s.read(len(user))) != user || string(s.read(len(password))) != password {
						t.Error("wrong TLS credentials")
					}
				}
				s.write(u32(0))
				s.init(8, 8)
			})
			if err != nil {
				t.Fatal(err)
			}
			if c.Width() != 8 {
				t.Fatal("encrypted ServerInit failed")
			}
		})
	}
}

func TestVeNCryptRejectsUnsafeAndMalformedOffers(t *testing.T) {
	for _, mode := range []string{"old-version", "bad-version-ack", "empty-offer", "plaintext", "unknown-subtype", "bad-subtype-ack"} {
		t.Run(mode, func(t *testing.T) {
			_, err := pipe(t, Config{Username: "user", Password: "never-send-this"}, func(s *fakeServer) {
				s.write([]byte("RFB 003.008\n"))
				s.read(12)
				s.write([]byte{1, SecVeNCrypt})
				s.read(1)
				if mode == "old-version" {
					s.write([]byte{0, 1})
					return
				}
				s.write([]byte{0, 2})
				s.read(2)
				if mode == "bad-version-ack" {
					s.write([]byte{1})
					return
				}
				if mode == "empty-offer" {
					s.write([]byte{0, 0})
					return
				}
				subtype := uint32(262)
				if mode == "plaintext" {
					subtype = 256
				}
				if mode == "unknown-subtype" {
					subtype = 9999
				}
				s.write([]byte{0, 1}, u32(subtype))
				if mode == "bad-subtype-ack" {
					s.read(4)
					s.write([]byte{0})
				}
			})
			if err == nil {
				t.Fatal("invalid offer accepted")
			}
		})
	}
}

func TestVNCEncryptedSecurityPreferences(t *testing.T) {
	for _, tc := range []struct {
		offered []uint8
		pin     string
		want    uint8
	}{
		{[]uint8{19, 129, 5}, "", 129}, {[]uint8{5, 19}, "", 5}, {[]uint8{19, 2}, "", 19}, {[]uint8{2}, "", 2},
		{[]uint8{2, 1}, "saved-pin", 0},
	} {
		c := &Client{cfg: Config{ServerFingerprint: tc.pin}}
		got, err := c.chooseSecurity(tc.offered)
		if tc.want == 0 {
			if err == nil {
				t.Fatal("pinned server downgraded to unverified security")
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("selection %d %v", got, err)
		}
	}
}

func TestRSARejectsUnboundedKeys(t *testing.T) {
	for _, bits := range []uint32{0, 1024, 8193, 0xffffffff} {
		if _, _, err := readRSAPublic(bytes.NewReader(u32(bits))); err == nil {
			t.Fatal("invalid key size accepted")
		}
	}
	key := append(u32(2048), make([]byte, 512)...)
	if _, _, err := readRSAPublic(bytes.NewReader(key)); err == nil {
		t.Fatal("zero RSA key accepted")
	}
}

func TestIdentityRequiresExplicitTrust(t *testing.T) {
	sum := sha256.Sum256([]byte("identity"))
	pin := hex.EncodeToString(sum[:])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		pin      string
		callback func(context.Context, string) bool
		ctx      context.Context
		ok       bool
	}{
		{pin: pin, ctx: context.Background(), ok: true},
		{ctx: context.Background()},
		{pin: strings.Repeat("a", 64), ctx: context.Background()},
		{callback: func(context.Context, string) bool { return true }, ctx: ctx},
	} {
		c := &Client{cfg: Config{ServerFingerprint: tc.pin, OnServerIdentity: tc.callback}, authContext: tc.ctx}
		if err := c.trustIdentity([]byte("identity")); (err == nil) != tc.ok {
			t.Fatal("identity verification result incorrect")
		}
	}
}

func TestEAXRejectsTamperedRecordBeforeDeliveringBytes(t *testing.T) {
	key := make([]byte, 16)
	cipher, _ := newEAX(key)
	header := []byte{0, 3}
	sealed := cipher.seal(make([]byte, 16), header, []byte("abc"))
	sealed[len(sealed)-1] ^= 1
	c := &eaxConn{reader: bytes.NewReader(append(header, sealed...)), rx: cipher}
	p := make([]byte, 3)
	n, err := c.Read(p)
	if n != 0 || err == nil {
		t.Fatal("tampered ciphertext exposed plaintext")
	}
	if _, next := c.Read(p); next != err {
		t.Fatal("authentication failure was not sticky")
	}
	if _, err := cipher.open(nil, nil, nil); err == nil {
		t.Fatal("truncated tag accepted")
	}
}
