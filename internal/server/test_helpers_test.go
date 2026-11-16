package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"github.com/dyike/mirage-server/internal/server/input"
	"math/big"
	"sync/atomic"
	"testing"
	"time"
)

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "localhost", RootCAs: pool}
}

type testInput struct {
	events chan input.Event
	closed chan struct{}
	led    atomic.Uint32
}

func (i *testInput) Apply(e input.Event) error { i.events <- e; i.led.Store(4); return nil }
func (i *testInput) LEDs() uint32              { return i.led.Load() }
func (i *testInput) Close() error              { close(i.closed); return nil }
