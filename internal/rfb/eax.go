package rfb

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"sync"
)

// EAX uses domain-separated CMACs for nonce, associated data and ciphertext.
// RFB's RSA-AES transport fixes the tag and nonce sizes at 16 bytes.
type eaxCipher struct {
	block  cipher.Block
	k1, k2 [16]byte
}

func newEAX(key []byte) (*eaxCipher, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	e := &eaxCipher{block: b}
	var l [16]byte
	b.Encrypt(l[:], l[:])
	e.k1 = cmacDouble(l)
	e.k2 = cmacDouble(e.k1)
	return e, nil
}
func cmacDouble(in [16]byte) (out [16]byte) {
	for i := 0; i < 15; i++ {
		out[i] = in[i]<<1 | in[i+1]>>7
	}
	out[15] = in[15] << 1
	if in[0]&128 != 0 {
		out[15] ^= 0x87
	}
	return
}
func (e *eaxCipher) mac(domain byte, data []byte) (state [16]byte) {
	// Prefix is a complete block ending in the domain byte. When data is
	// empty it is also the last block and receives K1, without padding.
	var last [16]byte
	last[15] = domain
	if len(data) == 0 {
		for i := range last {
			last[i] ^= e.k1[i]
		}
		e.block.Encrypt(state[:], last[:])
		return
	}
	e.block.Encrypt(state[:], last[:])
	for len(data) > 16 {
		for i := range state {
			state[i] ^= data[i]
		}
		e.block.Encrypt(state[:], state[:])
		data = data[16:]
	}
	last = [16]byte{}
	copy(last[:], data)
	sub := e.k1
	if len(data) < 16 {
		last[len(data)] = 0x80
		sub = e.k2
	}
	for i := range state {
		state[i] ^= last[i] ^ sub[i]
	}
	e.block.Encrypt(state[:], state[:])
	return
}
func (e *eaxCipher) seal(nonce, header, plain []byte) []byte {
	n, h := e.mac(0, nonce), e.mac(1, header)
	out := make([]byte, len(plain)+16)
	cipher.NewCTR(e.block, n[:]).XORKeyStream(out[:len(plain)], plain)
	tag := e.mac(2, out[:len(plain)])
	for i := range tag {
		out[len(plain)+i] = tag[i] ^ n[i] ^ h[i]
	}
	return out
}
func (e *eaxCipher) open(nonce, header, data []byte) ([]byte, error) {
	if len(data) < 16 {
		return nil, errors.New("rfb: short RSA-AES tag")
	}
	n, h := e.mac(0, nonce), e.mac(1, header)
	ciphertext := data[:len(data)-16]
	tag := e.mac(2, ciphertext)
	for i := range tag {
		tag[i] ^= n[i] ^ h[i]
	}
	if subtle.ConstantTimeCompare(tag[:], data[len(ciphertext):]) != 1 {
		return nil, errors.New("rfb: invalid RSA-AES authentication tag")
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCTR(e.block, n[:]).XORKeyStream(out, ciphertext)
	return out, nil
}

type eaxConn struct {
	net.Conn
	reader            io.Reader
	rx, tx            *eaxCipher
	rn, tn            [16]byte
	readMu, writeMu   sync.Mutex
	pending           []byte
	readErr, writeErr error
}

func newEAXConn(conn net.Conn, receive, send []byte) (*eaxConn, error) {
	rx, err := newEAX(receive)
	if err != nil {
		return nil, err
	}
	tx, err := newEAX(send)
	if err != nil {
		return nil, err
	}
	return &eaxConn{Conn: conn, reader: conn, rx: rx, tx: tx}, nil
}
func incrementNonce(n *[16]byte) {
	for i := range n {
		n[i]++
		if n[i] != 0 {
			return
		}
	}
}
func (c *eaxConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		if c.readErr != nil {
			return 0, c.readErr
		}
		var header [2]byte
		if _, err := io.ReadFull(c.reader, header[:]); err != nil {
			c.readErr = err
			return 0, err
		}
		data := make([]byte, int(be.Uint16(header[:]))+16)
		if _, err := io.ReadFull(c.reader, data); err != nil {
			c.readErr = err
			return 0, err
		}
		plain, err := c.rx.open(c.rn[:], header[:], data)
		if err != nil {
			c.readErr = err
			return 0, err
		}
		incrementNonce(&c.rn)
		c.pending = plain
	}
	n := copy(p, c.pending)
	clear(c.pending[:n])
	c.pending = c.pending[n:]
	return n, nil
}
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func (c *eaxConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), 65535)
		var header [2]byte
		be.PutUint16(header[:], uint16(n))
		data := append(header[:], c.tx.seal(c.tn[:], header[:], p[:n])...)
		if err := writeAll(c.Conn, data); err != nil {
			c.writeErr = err
			return written, err
		}
		incrementNonce(&c.tn)
		written += n
		p = p[n:]
	}
	return written, nil
}
