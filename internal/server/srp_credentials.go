package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/bigmod"
)

// RFC 5054 appendix A, 4096-bit group (generator 5).
const srpPrimeHex = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A92108011A723C12A787E6D788719A10BDBA5B2699C327186AF4E23C1A946834B6150BDA2583E9CA2AD44CE8DBBBC2DB04DE8EF92E8EFC141FBECAA6287C59474E6BC05D99B2964FA090C3A2233BA186515BE7ED1F612970CEE2D7AFB81BDD762170481CD0069127D5B05AA993B4EA988D8FDDC186FFB7DC90A6C08F4DF435C934063199FFFFFFFFFFFFFFFF"

func srpGroup() *big.Int       { n, _ := new(big.Int).SetString(srpPrimeHex, 16); return n }
func srpPad(n *big.Int) []byte { return n.FillBytes(make([]byte, 512)) }
func srpHash(parts ...[]byte) []byte {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// SRPCredential is a password-equivalent verifier, never a plaintext password.
// Protect this file like a private key. Password changes require re-enrollment.
type SRPCredential struct {
	Version    int    `json:"version"`
	Username   string `json:"username"`
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	Verifier   []byte `json:"verifier"`
}

func NewSRPCredential(username string, password []byte) (*SRPCredential, error) {
	if username == "" || len(username) > 255 || strings.ContainsRune(username, 0) || len(password) == 0 || len(password) > 1024 {
		return nil, errors.New("invalid SRP username or password length")
	}
	c := &SRPCredential{Version: 2, Username: username, Salt: make([]byte, 32), Iterations: 100000}
	if _, err := rand.Read(c.Salt); err != nil {
		return nil, err
	}
	p, err := pbkdf2.Key(sha512.New, string(password), c.Salt, c.Iterations, 128)
	if err != nil {
		return nil, err
	}
	defer clear(p)
	x := srpHash(c.Salt, srpHash([]byte(":"), p))
	defer clear(x)
	m := srpModulus()
	g, _ := bigmod.NewNat().SetBytes([]byte{5}, m)
	c.Verifier = bigmod.NewNat().Exp(g, x, m).Bytes(m)
	return c, nil
}
func (c *SRPCredential) validate() error {
	if c != nil && c.Version == 1 {
		return errors.New("obsolete SRP group: re-enroll locally with mirage-server -enroll-srp")
	}
	if c == nil || c.Version != 2 || c.Username == "" || len(c.Username) > 255 || strings.ContainsRune(c.Username, 0) || len(c.Salt) != 32 || c.Iterations < 10000 || c.Iterations > 1000000 || len(c.Verifier) != 512 {
		return errors.New("invalid SRP credential")
	}
	v := new(big.Int).SetBytes(c.Verifier)
	if v.Cmp(big.NewInt(1)) <= 0 || v.Cmp(srpGroup()) >= 0 {
		return errors.New("invalid SRP verifier")
	}
	return nil
}
func LoadSRPCredential(path string) (*SRPCredential, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 8192 {
		return nil, errors.New("SRP credential must be a private regular file (mode 0600)")
	}
	var c SRPCredential
	d := json.NewDecoder(io.LimitReader(f, 8193))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing SRP credential data")
	}
	return &c, c.validate()
}
func SaveSRPCredential(path string, c *SRPCredential) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".srp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(c); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func srpModulus() *bigmod.Modulus {
	m, err := bigmod.NewModulus(srpPad(srpGroup()))
	if err != nil {
		panic(err)
	}
	return m
}
