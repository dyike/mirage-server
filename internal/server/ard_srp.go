package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"filippo.io/bigmod"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
)

const srpOptions = "mda=SHA-512,replay_detection,conf+int=ChaCha20-Poly1305,kdf=SALTED-SHA512-PBKDF2"

// Apple type 36 is mutual password authentication: the client checks M2 before
// accepting ServerInit. No unauthenticated DH fallback is offered in this mode.
func (s *RFBServer) authenticateSRP(c net.Conn) (net.Conn, error) {
	if err := writeBytes(c, []byte{1, 36}); err != nil {
		return nil, err
	}
	selected, err := readBytes(c, 1)
	if err != nil {
		return nil, err
	}
	if selected[0] != 36 {
		return nil, errors.New("Apple SRP required; legacy authentication disabled")
	}
	fail := func() (net.Conn, error) {
		reason := []byte("authentication failed")
		_ = writeBytes(c, append(append(u32(1), u32(uint32(len(reason)))...), reason...))
		return nil, errors.New("Apple SRP authentication failed")
	}
	id, err := readSRPFrame(c)
	if err != nil {
		return nil, fmt.Errorf("SRP identity: %w", err)
	}
	f := srpFields{b: id}
	kind := f.u16()
	name := f.take(f.u16())
	empty := f.u16()
	option := f.u8()
	if f.bad || len(f.b) != 0 || kind != 0 || empty != 0 || option != 0 || len(name) == 0 || len(name) > 255 {
		return fail()
	}
	credential, err := s.SRPCredential()
	if err != nil {
		log.Printf("SRP credential unavailable: %v", err)
		return fail()
	}
	if err := credential.validate(); err != nil {
		log.Printf("SRP credential invalid: %v", err)
		return fail()
	}
	validUser := subtle.ConstantTimeCompare(name, []byte(credential.Username)) == 1
	N := srpPad(srpGroup())
	m := srpModulus()
	g, _ := bigmod.NewNat().SetBytes([]byte{5}, m)
	v, err := bigmod.NewNat().SetBytes(credential.Verifier, m)
	if err != nil {
		return fail()
	}
	secret := make([]byte, 64)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	defer clear(secret)
	secret[0] |= 128
	k, _ := bigmod.NewNat().SetBytes(srpHash(N, g.Bytes(m)), m)
	B := bigmod.NewNat().Exp(g, secret, m)
	B.Add(k.Mul(v, m), m)
	if B.IsZero() == 1 {
		return fail()
	}
	challenge := []byte{0}
	challenge = srpAppend16(challenge, N)
	challenge = srpAppend16(challenge, []byte{5})
	challenge = append(challenge, byte(len(credential.Salt)))
	challenge = append(challenge, credential.Salt...)
	challenge = srpAppend16(challenge, B.Bytes(m))
	challenge = networkOrder.AppendUint64(challenge, uint64(credential.Iterations))
	challenge = srpAppend16(challenge, []byte(srpOptions))
	if err = writeSRPFrame(c, challenge); err != nil {
		return nil, err
	}
	response, err := readSRPFrame(c)
	if err != nil {
		return nil, fmt.Errorf("SRP client proof: %w", err)
	}
	f = srpFields{b: response}
	ab := f.take(f.u16())
	proof := f.take(f.u8())
	options := f.take(f.u16())
	nonce := f.take(f.u8())
	if f.bad || len(f.b) != 0 || (len(ab) == 0 || len(ab) > 512) || len(proof) != 64 || len(nonce) != 16 || !validSRPOptions(string(options)) {
		return fail()
	}
	A, err := bigmod.NewNat().SetBytes(ab, m)
	if err != nil || A.IsZero() == 1 {
		return fail()
	}
	// Apple MPI serializes without leading zeros; SRP hashes padded values.
	ab = A.Bytes(m)
	u := srpHash(ab, B.Bytes(m))
	if bytes.Equal(u, make([]byte, 64)) {
		return fail()
	}
	S := bigmod.NewNat().Exp(v, u, m)
	S.Mul(A, m).Exp(S, secret, m)
	K := srpHash(S.Bytes(m))
	defer clear(K)
	hn, hg := srpHash(N), srpHash(g.Bytes(m))
	for i := range hn {
		hn[i] ^= hg[i]
	}
	expected := srpHash(hn, srpHash(nil), credential.Salt, ab, B.Bytes(m), K)
	if subtle.ConstantTimeCompare(proof, expected) != 1 || !validUser {
		return fail()
	}
	m2 := srpHash(ab, proof, K)
	reply := append([]byte{64}, m2...)
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return nil, err
	}
	reply = append(reply, 16)
	reply = append(reply, random...)
	reply = append(reply, 0, 0, 0, 0, 0, 0)
	if err = writeSRPFrame(c, reply); err != nil {
		return nil, err
	}
	if err = writeBytes(c, u32(0)); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(K)
	secured := &ardConn{Conn: c}
	copy(secured.key[:], sum[:16])
	log.Printf("Apple SRP authentication succeeded: client proof verified, server proof sent, peer=%s", c.RemoteAddr())
	return secured, nil
}
func srpAppend16(b, p []byte) []byte {
	b = networkOrder.AppendUint16(b, uint16(len(p)))
	return append(b, p...)
}
func readSRPFrame(c net.Conn) ([]byte, error) {
	h, err := readBytes(c, 4)
	if err != nil {
		return nil, err
	}
	n := networkOrder.Uint32(h)
	if n < 4 || n > 8192 {
		return nil, errors.New("invalid SRP frame length")
	}
	b, err := readBytes(c, int(n))
	if err != nil {
		return nil, err
	}
	if networkOrder.Uint32(b) != n-4 {
		return nil, errors.New("invalid SRP inner length")
	}
	return b[4:], nil
}
func writeSRPFrame(c net.Conn, b []byte) error {
	return writeBytes(c, append(append(u32(uint32(len(b)+4)), u32(uint32(len(b)))...), b...))
}

type srpFields struct {
	b   []byte
	bad bool
}

func (f *srpFields) take(n int) []byte {
	if f.bad || n < 0 || n > len(f.b) {
		f.bad = true
		return nil
	}
	b := f.b[:n]
	f.b = f.b[n:]
	return b
}
func (f *srpFields) u8() int {
	b := f.take(1)
	if b == nil {
		return 0
	}
	return int(b[0])
}
func (f *srpFields) u16() int {
	b := f.take(2)
	if b == nil {
		return 0
	}
	return int(networkOrder.Uint16(b))
}

// Apple SASL creates its own option list instead of echoing the challenge.
// Accept semantic equivalents, but never missing/unknown protection algorithms.
func validSRPOptions(options string) bool {
	seen := make(map[string]bool)
	for _, option := range strings.Split(strings.ToLower(options), ",") {
		name, value, _ := strings.Cut(option, "=")
		if name == "confidentiality+integrity" {
			name = "conf+int"
		}
		if seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "mda":
			if value != "sha-512" {
				return false
			}
		case "kdf":
			if value != "salted-sha512-pbkdf2" {
				return false
			}
		case "replay_detection":
			if value != "" {
				return false
			}
		case "conf+int":
			if value != "chacha20-poly1305" {
				return false
			}
		case "maxbuffersize":
			n, err := strconv.ParseUint(value, 10, 31)
			if err != nil || n == 0 || n > 2147483643 {
				return false
			}
		default:
			return false
		}
	}
	return seen["mda"] && seen["kdf"] && seen["replay_detection"] && seen["conf+int"]
}
