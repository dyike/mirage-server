//go:build linux && !android

package rfb

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Independent interoperability check, using a separate WayVNC server bound to
// loopback, detached from the user's desktop, with disposable credentials.
func TestWayVNCAuthentication(t *testing.T) {
	if os.Getenv("MIRAGE_TEST_WAYVNC") != "1" {
		t.Skip("opt-in WayVNC interoperability check")
	}
	path, err := exec.LookPath("wayvnc")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Mirage test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, certPath := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	cfgPath := filepath.Join(dir, "config")
	cfg := fmt.Sprintf("enable_auth=true\nusername=mirage-test\npassword=synthetic-test-password\nprivate_key_file=%s\ncertificate_file=%s\nrsa_private_key_file=%s\n", keyPath, certPath, keyPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(path, "-D", "-d", "-C", cfgPath, "-S", filepath.Join(dir, "control.sock"), addr)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ready := false
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		data, _ := os.ReadFile(log.Name())
		t.Fatalf("WayVNC startup: %s", data)
	}
	for _, sec := range []uint8{SecRA256, SecRA2, SecVeNCrypt} {
		t.Run(fmt.Sprint(sec), func(t *testing.T) {
			identity := rsaPublicWire(&key.PublicKey)
			if sec == SecVeNCrypt {
				identity = cert
			}
			sum := sha256.Sum256(identity)
			pin := hex.EncodeToString(sum[:])
			for _, mode := range []string{"trust", "pin", "wrong-password", "reject", "changed-pin"} {
				t.Run(mode, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					calls := 0
					cfg := Config{Username: "mirage-test", Password: "synthetic-test-password", Security: []uint8{sec}, OnServerIdentity: func(_ context.Context, got string) bool {
						calls++
						if got != pin {
							t.Error("unexpected fingerprint")
						}
						return mode == "trust" || mode == "wrong-password"
					}}
					if mode == "pin" {
						cfg.ServerFingerprint = pin
					}
					if mode == "changed-pin" {
						cfg.ServerFingerprint = "different"
					}
					if mode == "wrong-password" {
						cfg.Password = "wrong"
					}
					client, err := Dial(ctx, addr, cfg)
					if client != nil {
						defer client.Close()
					}
					if mode == "trust" || mode == "pin" {
						if err != nil {
							t.Fatal(err)
						}
						if mode == "pin" && calls != 0 {
							t.Fatal("saved pin prompted again")
						}
					} else if err == nil {
						t.Fatal("invalid credentials or untrusted identity accepted")
					}
				})
			}
		})
	}
}
