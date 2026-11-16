package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // ARD security type 30 specifies MD5 of the DH secret.
	"crypto/rand"
	"crypto/sha1" // Apple record format; not a new cryptographic protocol.
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
)

// RFC 3526 group 14. ARD permits a variable DH key length; use 2048 bits.
const ardPrimeHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF"

type ardConn struct {
	net.Conn
	key     [16]byte
	pending []byte // bounded native SetEncodings received before the rekey
	reader  *ardRecordReader
	writer  *ardRecordWriter
}

func (c *ardConn) Read(b []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(b, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.reader != nil {
		return c.reader.Read(b)
	}
	return c.Conn.Read(b)
}
func (c *ardConn) Write(b []byte) (int, error) {
	if c.writer != nil {
		return c.writer.Write(b)
	}
	return c.Conn.Write(b)
}

func (s *RFBServer) authenticateARD(ctx context.Context, c net.Conn) (net.Conn, error) {
	if s.Authenticate == nil && (s.Username == "" || len(s.Username) > 63 || len(s.Password) > 63) {
		return nil, errors.New("ARD requires an account and credentials of at most 63 bytes")
	}
	if err := writeBytes(c, []byte{1, 30}); err != nil {
		return nil, err
	}
	selected, err := readBytes(c, 1)
	if err != nil {
		return nil, err
	}
	if selected[0] != 30 {
		return nil, errors.New("ARD authentication required")
	}
	prime, _ := new(big.Int).SetString(ardPrimeHex, 16)
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	secret[0] |= 128
	exponent := new(big.Int).SetBytes(secret)
	clear(secret)
	pub := new(big.Int).Exp(big.NewInt(2), exponent, prime)
	challenge := []byte{0, 2, 1, 0}
	challenge = append(challenge, prime.FillBytes(make([]byte, 256))...)
	challenge = append(challenge, pub.FillBytes(make([]byte, 256))...)
	if err = writeBytes(c, challenge); err != nil {
		return nil, err
	}
	response, err := readBytes(c, 128+256)
	if err != nil {
		return nil, err
	}
	defer clear(response)
	peer := new(big.Int).SetBytes(response[128:])
	one := big.NewInt(1)
	upper := new(big.Int).Sub(prime, one)
	if peer.Cmp(one) <= 0 || peer.Cmp(upper) >= 0 {
		return nil, errors.New("invalid ARD DH public value")
	}
	q := new(big.Int).Rsh(new(big.Int).Set(upper), 1)
	if new(big.Int).Exp(peer, q, prime).Cmp(one) != 0 {
		return nil, errors.New("ARD public value outside subgroup")
	}
	shared := new(big.Int).Exp(peer, exponent, prime).FillBytes(make([]byte, 256))
	key := md5.Sum(shared)
	clear(shared)
	block, _ := aes.NewCipher(key[:])
	for i := 0; i < 128; i += 16 {
		block.Decrypt(response[i:i+16], response[i:i+16])
	}
	userEnd, passEnd := bytes.IndexByte(response[:64], 0), bytes.IndexByte(response[64:128], 0)
	valid := false
	if userEnd >= 0 && passEnd >= 0 {
		username, password := string(response[:userEnd]), string(response[64:64+passEnd])
		if s.Authenticate != nil {
			valid = s.Authenticate(ctx, username, password) == nil
		} else {
			u, wu := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(s.Username))
			p, wp := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(s.Password))
			valid = subtle.ConstantTimeCompare(u[:], wu[:])&subtle.ConstantTimeCompare(p[:], wp[:]) == 1
		}
	}
	if !valid {
		reason := []byte("authentication failed")
		writeBytes(c, append(append(u32(1), u32(uint32(len(reason)))...), reason...))
		return nil, errors.New("ARD authentication failed")
	}
	if err = writeBytes(c, u32(0)); err != nil {
		return nil, err
	}
	return &ardConn{Conn: c, key: key}, nil
}

const ardRecordMax = 65520
const ardRecordBody = ardRecordMax - 22

type ardRecordReader struct {
	source io.Reader
	cbc    cipher.BlockMode
	seq    uint64
	left   []byte
}

func (r *ardRecordReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(r.left) == 0 {
		h, err := readBytes(r.source, 2)
		if err != nil {
			return 0, err
		}
		n := int(networkOrder.Uint16(h))
		if r.seq == 0 {
			log.Printf("ARD inbound first record: length=%d", n)
		}
		if n < 32 || n > ardRecordMax || n%16 != 0 || r.seq > 0xffffffff {
			return 0, errors.New("invalid ARD record")
		}
		raw, err := readBytes(r.source, n)
		if err != nil {
			return 0, err
		}
		r.cbc.CryptBlocks(raw, raw)
		end := n - sha1.Size
		sum := sha1.New()
		sum.Write(u32(uint32(r.seq)))
		sum.Write(raw[:end])
		if subtle.ConstantTimeCompare(sum.Sum(nil), raw[end:]) != 1 {
			return 0, errors.New("ARD record checksum mismatch")
		}
		size := int(networkOrder.Uint16(raw))
		if size+2 > end {
			return 0, errors.New("ARD record body exceeds length")
		}
		r.seq++
		r.left = raw[2 : 2+size]
	}
	n := copy(b, r.left)
	r.left = r.left[n:]
	return n, nil
}

type ardRecordWriter struct {
	target io.Writer
	cbc    cipher.BlockMode
	seq    uint64
}

func (w *ardRecordWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		if w.seq > 0xffffffff {
			return written, errors.New("ARD record sequence exhausted")
		}
		n := min(len(b), ardRecordBody)
		size := (n + 22 + 15) &^ 15
		packet := make([]byte, size+2)
		networkOrder.PutUint16(packet, uint16(size))
		raw := packet[2:]
		networkOrder.PutUint16(raw, uint16(n))
		copy(raw[2:], b[:n])
		sum := sha1.New()
		sum.Write(u32(uint32(w.seq)))
		sum.Write(raw[:size-20])
		copy(raw[size-20:], sum.Sum(nil))
		w.cbc.CryptBlocks(raw, raw)
		if err := writeBytes(w.target, packet); err != nil {
			return written, err
		}
		w.seq++
		written += n
		b = b[n:]
	}
	return written, nil
}

// Complete the plaintext setup before any capture/clipboard goroutine writes.
func (c *ardConn) startRecords() error {
	configured := false
	encodingsSeen, displaySeen := false, false
	for messages := 0; messages < 8; messages++ {
		t, err := readBytes(c.Conn, 1)
		if err != nil {
			return err
		}
		log.Printf("ARD setup: peer=%s index=%d type=0x%02x configured=%t", c.RemoteAddr(), messages, t[0], configured)
		switch t[0] {
		case 0x21: // ViewerInfo, length excludes the first four bytes.
			h, err := readBytes(c.Conn, 3)
			if err != nil {
				return err
			}
			n := int(networkOrder.Uint16(h[1:]))
			if n > 1024 {
				return errors.New("ARD ViewerInfo too large")
			}
			if _, err = readBytes(c.Conn, n); err != nil {
				return err
			}
		case 0x1d: // Native High Performance configures the canvas before SetEncodings.
			if !configured || displaySeen {
				return errors.New("invalid ARD setup display configuration")
			}
			h, err := readBytes(c.Conn, 3)
			if err != nil {
				return err
			}
			body, err := readBytes(c.Conn, int(networkOrder.Uint16(h[1:])))
			if err != nil {
				return err
			}
			if _, err = ardRequestedCanvas(body); err != nil {
				return err
			}
			// Replay through the regular dispatcher after the encryption boundary.
			// No layout replies may overtake the plaintext rekey.
			c.pending = append(c.pending, 0x1d)
			c.pending = append(c.pending, h...)
			c.pending = append(c.pending, body...)
			displaySeen = true
		case 0x02:
			// Native Screen Sharing sends SetEncodings after requesting AES,
			// before the command-2 acknowledgment. Retain the complete
			// bounded message for normal dispatch after installing the keys.
			if !configured {
				return errors.New("ARD encodings before encryption request")
			}
			h, err := readBytes(c.Conn, 3)
			if err != nil {
				return err
			}
			count := int(networkOrder.Uint16(h[1:]))
			if h[0] != 0 || count > 4096 {
				return errors.New("invalid ARD setup encodings")
			}
			body, err := readBytes(c.Conn, count*4)
			if err != nil {
				return err
			}
			if encodingsSeen {
				return errors.New("duplicate ARD setup encodings")
			}
			encodingsSeen = true
			c.pending = append(c.pending, 2)
			c.pending = append(c.pending, h...)
			c.pending = append(c.pending, body...)
			if err := c.activateRecords(); err != nil {
				return err
			}
			// Native sends the plaintext command-2 acknowledgment after receiving
			// the rekey. Keep parsing the raw prelude until that explicit boundary.
			continue
		case 0x0a:
			if _, err = readBytes(c.Conn, 3); err != nil {
				return err
			}
		case 0x12:
			h, err := readBytes(c.Conn, 3)
			if err != nil {
				return err
			}
			kind := networkOrder.Uint16(h[1:])
			log.Printf("ARD setup encryption: peer=%s command=%d", c.RemoteAddr(), kind)
			switch kind {
			case 1:
				b, err := readBytes(c.Conn, 8)
				if err != nil {
					return err
				}
				if networkOrder.Uint16(b) != 1 || networkOrder.Uint16(b[2:]) != 1 || networkOrder.Uint32(b[4:]) != 1 {
					return errors.New("unsupported ARD record suite")
				}
				configured = true
			case 2:
				b, err := readBytes(c.Conn, 4)
				if err != nil {
					return err
				}
				if !configured || networkOrder.Uint16(b) != 1 {
					return errors.New("invalid ARD record activation")
				}
				if c.reader != nil {
					return nil
				} // rekey already sent for native prelude
				return c.activateRecords()
			default:
				return errors.New("unsupported ARD encryption operation")
			}
		default:
			return fmt.Errorf("unexpected ARD setup message: type=0x%02x index=%d peer=%s", t[0], messages, c.RemoteAddr())
		}
	}
	return errors.New("ARD setup did not activate records")
}

func (c *ardConn) activateRecords() error {
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		return err
	}
	wrap, _ := aes.NewCipher(c.key[:])
	body := make([]byte, 36)
	networkOrder.PutUint32(body, 1)
	wrap.Encrypt(body[4:20], material[:16])
	wrap.Encrypt(body[20:], material[16:])
	if err := writeBytes(c.Conn, updateMessage(append(rectHeaderEmpty(1103), body...))); err != nil {
		return err
	}
	copy(c.key[:], material[:16])
	block, _ := aes.NewCipher(c.key[:])
	c.reader = &ardRecordReader{source: c.Conn, cbc: cipher.NewCBCDecrypter(block, material[16:])}
	c.writer = &ardRecordWriter{target: c.Conn, cbc: cipher.NewCBCEncrypter(block, material[16:])}
	clear(material)
	return nil
}
