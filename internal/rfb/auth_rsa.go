package rfb

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"hash"
	"io"
	"math/big"
)

func readRSAPublic(r io.Reader) (*rsa.PublicKey, []byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, nil, err
	}
	bits := be.Uint32(header[:])
	if bits < 2048 || bits > 8192 {
		return nil, nil, protoErr("RSA-AES key size %d outside supported 2048–8192 bits", bits)
	}
	n := int((bits + 7) / 8)
	wire := make([]byte, 4+2*n)
	copy(wire, header[:])
	if _, err := io.ReadFull(r, wire[4:]); err != nil {
		return nil, nil, err
	}
	modulus := new(big.Int).SetBytes(wire[4 : 4+n])
	exponent := new(big.Int).SetBytes(wire[4+n:])
	if modulus.BitLen() != int(bits) || modulus.Bit(0) == 0 || !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 2147483647 || exponent.Bit(0) == 0 {
		return nil, nil, protoErr("invalid RSA-AES public key")
	}
	return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, wire, nil
}
func rsaPublicWire(pub *rsa.PublicKey) []byte {
	n := pub.Size()
	b := make([]byte, 4+n*2)
	be.PutUint32(b, uint32(pub.N.BitLen()))
	pub.N.FillBytes(b[4 : 4+n])
	big.NewInt(int64(pub.E)).FillBytes(b[4+n:])
	return b
}
func rsaDigest(strong bool, parts ...[]byte) []byte {
	var h hash.Hash = sha1.New()
	if strong {
		h = sha256.New()
	}
	for _, part := range parts {
		h.Write(part)
	}
	return h.Sum(nil)
}

func (c *Client) rsaAESAuth(strong bool) error {
	server, serverWire, err := readRSAPublic(c.r)
	if err != nil {
		return err
	}
	if err := c.trustIdentity(serverWire); err != nil {
		return err
	}
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	clientWire := rsaPublicWire(&private.PublicKey)
	if err := writeAll(c.conn, clientWire); err != nil {
		return err
	}
	size := 16
	if strong {
		size = 32
	}
	clientRandom := make([]byte, size)
	serverRandom := make([]byte, size)
	defer clear(clientRandom)
	defer clear(serverRandom)
	if _, err := rand.Read(clientRandom); err != nil {
		return err
	}
	if _, err := rand.Read(serverRandom); err != nil {
		return err
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, server, clientRandom)
	if err != nil {
		return err
	}
	var length [2]byte
	be.PutUint16(length[:], uint16(len(encrypted)))
	if err := writeAll(c.conn, append(length[:], encrypted...)); err != nil {
		return err
	}
	if _, err := io.ReadFull(c.r, length[:]); err != nil {
		return err
	}
	if int(be.Uint16(length[:])) != private.Size() {
		return protoErr("invalid RSA-AES random length")
	}
	encrypted = make([]byte, private.Size())
	if _, err := io.ReadFull(c.r, encrypted); err != nil {
		return err
	}
	// Invalid PKCS#1 padding leaves a random key; the authenticated transcript
	// then fails without exposing a padding oracle.
	if err := rsa.DecryptPKCS1v15SessionKey(rand.Reader, private, encrypted, serverRandom); err != nil {
		return protoErr("invalid RSA-AES key exchange")
	}
	send := rsaDigest(strong, serverRandom, clientRandom)[:size]
	receive := rsaDigest(strong, clientRandom, serverRandom)[:size]
	defer clear(send)
	defer clear(receive)
	secured, err := newEAXConn(c.securityTransport(), receive, send)
	if err != nil {
		return err
	}
	c.setSecurityTransport(secured)
	clientHash := rsaDigest(strong, clientWire, serverWire)
	if err := writeAll(c.conn, clientHash); err != nil {
		return err
	}
	serverHash := make([]byte, len(clientHash))
	if _, err := io.ReadFull(c.r, serverHash); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(serverHash, rsaDigest(strong, serverWire, clientWire)) != 1 {
		return protoErr("RSA-AES transcript verification failed")
	}
	subtype, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	user := ""
	switch subtype {
	case 1:
		user = c.cfg.Username
		if user == "" {
			return &AuthError{Reason: "server requires a username"}
		}
	case 2:
	default:
		return protoErr("unsupported RSA-AES credential subtype %d", subtype)
	}
	if len(user) > 255 || len(c.cfg.Password) > 255 {
		return protoErr("RSA-AES credentials exceed 255 UTF-8 bytes")
	}
	creds := []byte{byte(len(user))}
	creds = append(creds, user...)
	creds = append(creds, byte(len(c.cfg.Password)))
	creds = append(creds, c.cfg.Password...)
	defer clear(creds)
	return writeAll(c.conn, creds)
}
