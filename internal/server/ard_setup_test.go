package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"io"
	"net"
	"testing"
	"time"
)

func TestNativeARDSetupWithEarlyEncodings(t *testing.T) {
	for _, highPerformance := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "high-performance"}[highPerformance], func(t *testing.T) { nativeARDSetup(t, highPerformance) })
	}
}
func nativeARDSetup(t *testing.T, highPerformance bool) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	server.SetDeadline(time.Now().Add(3 * time.Second))
	client.SetDeadline(time.Now().Add(3 * time.Second))
	c := &ardConn{Conn: server}
	for i := range c.key {
		c.key[i] = byte(i + 1)
	}
	wrapKey := c.key
	// Observed native order: ViewerInfo, SetEncryption(1), SetMode,
	// SetEncodings; command 2 arrives only after receiving the rekey.
	enc := []byte{2, 0, 0, 2, 0, 0, 0, 0, 0, 0, 4, 0x51}
	prelude := []byte{0x21, 0, 0, 0, 0x12, 0, 0, 1, 0, 1, 0, 1, 0, 0, 0, 1, 0x0a, 0, 0, 1}
	if highPerformance {
		config := make([]byte, 4+8+156+28)
		config[0] = 29
		networkOrder.PutUint16(config[2:], uint16(len(config)-4))
		networkOrder.PutUint16(config[4:], 1)
		networkOrder.PutUint16(config[6:], 1)
		networkOrder.PutUint16(config[12:], 184)
		networkOrder.PutUint16(config[12+154:], 1)
		networkOrder.PutUint32(config[12+156:], 3840)
		networkOrder.PutUint32(config[12+160:], 2160)
		enc = append(config, enc...)
	}
	prelude = append(prelude, enc...)
	done := make(chan error, 1)
	go func() { done <- c.startRecords() }()
	if _, err := client.Write(prelude); err != nil {
		t.Fatal(err)
	}
	rekey, err := readBytes(client, 52)
	if err != nil {
		t.Fatal(err)
	}
	if networkOrder.Uint32(rekey[12:16]) != 1103 {
		t.Fatalf("wrong rekey rectangle: %x", rekey[:20])
	}
	if _, err := client.Write([]byte{0x12, 0, 0, 2, 0, 1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	material := make([]byte, 32)
	wrap, _ := aes.NewCipher(wrapKey[:])
	wrap.Decrypt(material[:16], rekey[20:36])
	wrap.Decrypt(material[16:], rekey[36:52])
	got := make([]byte, len(enc))
	if _, err = io.ReadFull(c, got); err != nil || !bytes.Equal(got, enc) {
		t.Fatalf("lost prelude encodings: %x %v", got, err)
	}
	block, _ := aes.NewCipher(material[:16])
	w := ardRecordWriter{target: client, cbc: cipher.NewCBCEncrypter(block, material[16:])}
	payload := []byte{3, 0, 0, 0, 0, 0, 0, 64, 0, 64}
	go func() { _, e := w.Write(payload); done <- e }()
	got = make([]byte, len(payload))
	if _, err = io.ReadFull(c, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("record transition failed: %x %v", got, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
