package rfb

import (
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"io"
	"math/big"
	"math/bits"
)

// vncAuthResponse encrypts the 16-byte challenge with DES, keyed by the
// password's first 8 bytes with each byte's bits reversed (RFC 6143 §7.2.2;
// the reversal is a historical quirk of the reference implementation).
func vncAuthResponse(password string, challenge []byte) ([]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = bits.Reverse8(b)
	}
	c, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:16])
	return out, nil
}

// ardAuth runs Apple Remote Desktop authentication (security type 30), which
// macOS Screen Sharing offers for logging in with a macOS account.
//
// The server sends generator, key length, prime and its public key. Both
// sides derive a Diffie-Hellman secret; its MD5 is an AES-128 key that
// encrypts a 128-byte block holding the NUL-terminated username and password
// in two 64-byte fields. The client replies with the ciphertext followed by
// its own public key. The AES key is returned: the Apple dialect wraps its
// record-layer keys in it.
func ardAuth(rw io.ReadWriter, username, password string) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return nil, err
	}
	gen := be.Uint16(hdr[0:])
	keyLen := int(be.Uint16(hdr[2:]))
	if keyLen == 0 || keyLen > 1024 {
		return nil, protoErr("ARD key length %d", keyLen)
	}
	buf := make([]byte, 2*keyLen)
	if _, err := io.ReadFull(rw, buf); err != nil {
		return nil, err
	}
	prime := new(big.Int).SetBytes(buf[:keyLen])
	serverPub := new(big.Int).SetBytes(buf[keyLen:])

	privBytes := make([]byte, keyLen)
	if _, err := rand.Read(privBytes); err != nil {
		return nil, err
	}
	priv := new(big.Int).SetBytes(privBytes)
	clientPub := new(big.Int).Exp(big.NewInt(int64(gen)), priv, prime)
	shared := new(big.Int).Exp(serverPub, priv, prime)

	key := md5.Sum(shared.FillBytes(make([]byte, keyLen)))

	// Random padding, as Apple's client does, so the fields carry no
	// predictable plaintext after the terminator.
	creds := make([]byte, 128)
	if _, err := rand.Read(creds); err != nil {
		return nil, err
	}
	putCString(creds[:64], username)
	putCString(creds[64:], password)

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 128+keyLen)
	for i := 0; i < 128; i += aes.BlockSize { // ECB
		block.Encrypt(out[i:i+aes.BlockSize], creds[i:i+aes.BlockSize])
	}
	clientPub.FillBytes(out[128:])
	_, err = rw.Write(out)
	return key[:], err
}

// putCString writes s NUL-terminated into a fixed field, truncating so the
// terminator always fits.
func putCString(field []byte, s string) {
	n := copy(field[:len(field)-1], s)
	field[n] = 0
}
