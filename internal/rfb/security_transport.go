package rfb

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"slices"
)

// bufferedConn preserves bytes prefetched during the plaintext handshake.
type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *Client) securityTransport() net.Conn      { return &bufferedConn{Conn: c.conn, reader: c.r} }
func (c *Client) setSecurityTransport(conn net.Conn) {
	c.conn = conn
	c.r = bufio.NewReaderSize(conn, 256<<10)
}

func (c *Client) trustIdentity(data []byte) error {
	if err := c.authContext.Err(); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(sum[:])
	if c.cfg.ServerFingerprint != "" && subtle.ConstantTimeCompare([]byte(fingerprint), []byte(c.cfg.ServerFingerprint)) == 1 {
		return nil
	}
	if c.cfg.OnServerIdentity != nil && c.cfg.OnServerIdentity(c.authContext, fingerprint) && c.authContext.Err() == nil {
		return nil
	}
	return fmt.Errorf("rfb: server identity is untrusted or changed (SHA-256 %s)", fingerprint)
}

func (c *Client) vencryptAuth() error {
	var version [2]byte
	if _, err := io.ReadFull(c.r, version[:]); err != nil {
		return err
	}
	if version[0] != 0 || version[1] < 2 {
		return protoErr("unsupported VeNCrypt version %d.%d", version[0], version[1])
	}
	if _, err := c.conn.Write([]byte{0, 2}); err != nil {
		return err
	}
	ack, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	if ack != 0 {
		return protoErr("VeNCrypt version rejected")
	}
	n, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	offered := make([]uint32, int(n))
	for i := range offered {
		var b [4]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return err
		}
		offered[i] = be.Uint32(b[:])
	}
	// Never send Plain credentials over an unencrypted connection. Go TLS
	// requires a certificate; anonymous DH cipher suites are not supported.
	prefs := []uint32{261, 258, 260, 257}
	if c.cfg.Username != "" {
		prefs = []uint32{262, 259, 261, 258, 260, 257}
	}
	var selected uint32
	for _, p := range prefs {
		if slices.Contains(offered, p) {
			selected = p
			break
		}
	}
	if selected == 0 {
		return protoErr("no supported encrypted VeNCrypt subtype in %v (account authentication requires a username)", offered)
	}
	var b [4]byte
	be.PutUint32(b[:], selected)
	if _, err := c.conn.Write(b[:]); err != nil {
		return err
	}
	ack, err = c.r.ReadByte()
	if err != nil {
		return err
	}
	if ack != 1 {
		return protoErr("VeNCrypt subtype rejected")
	}
	tlsConn := tls.Client(c.securityTransport(), &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: c.cfg.ServerName,
		// Verification is mandatory below: a saved pin overrides CA trust and a
		// changed pin always requires explicit approval before sending credentials.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return protoErr("VeNCrypt server supplied no certificate")
			}
			cert := state.PeerCertificates[0]
			if c.cfg.ServerFingerprint == "" && c.cfg.ServerName != "" {
				intermediates := x509.NewCertPool()
				for _, crt := range state.PeerCertificates[1:] {
					intermediates.AddCert(crt)
				}
				if _, err := cert.Verify(x509.VerifyOptions{DNSName: c.cfg.ServerName, Intermediates: intermediates}); err == nil {
					return nil
				}
			}
			return c.trustIdentity(cert.Raw)
		},
	})
	if err := tlsConn.HandshakeContext(c.authContext); err != nil {
		return fmt.Errorf("rfb: VeNCrypt TLS handshake (certificate-based TLS 1.2+ required): %w", err)
	}
	c.setSecurityTransport(tlsConn)
	switch selected {
	case 258, 261:
		return c.passwordAuth()
	case 259, 262:
		if len(c.cfg.Username) > 1<<20 || len(c.cfg.Password) > 1<<20 {
			return protoErr("VeNCrypt credentials too long")
		}
		data := make([]byte, 8, 8+len(c.cfg.Username)+len(c.cfg.Password))
		be.PutUint32(data, uint32(len(c.cfg.Username)))
		be.PutUint32(data[4:], uint32(len(c.cfg.Password)))
		data = append(data, c.cfg.Username...)
		data = append(data, c.cfg.Password...)
		defer clear(data)
		_, err = c.conn.Write(data)
		return err
	}
	return nil
}

func (c *Client) passwordAuth() error {
	var challenge [16]byte
	if _, err := io.ReadFull(c.r, challenge[:]); err != nil {
		return err
	}
	response, err := vncAuthResponse(c.cfg.Password, challenge[:])
	if err != nil {
		return err
	}
	_, err = c.conn.Write(response)
	return err
}
