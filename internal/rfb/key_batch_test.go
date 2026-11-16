package rfb

import (
	"bytes"
	"crypto/aes"
	"fmt"
	"io"
	"net"
	"testing"
)

type keyWriteConn struct {
	net.Conn
	bytes.Buffer
	writes int
}

func (c *keyWriteConn) Read(b []byte) (int, error) { return c.Buffer.Read(b) }

func (c *keyWriteConn) Write(b []byte) (int, error) {
	c.writes++
	return c.Buffer.Write(b)
}

func TestKeyBatchPreservesWireOrderAndModifiers(t *testing.T) {
	for _, dialect := range []string{"vnc", "apple", "apple-plain"} {
		t.Run(dialect, func(t *testing.T) {
			conn := &keyWriteConn{}
			c := &Client{conn: conn}
			var record recordKeys
			if dialect != "vnc" {
				c.apple, c.appleExt = true, true
				c.sealer = newRecordSealer(record)
				c.appleCmds[msgApplePointer/8] |= 0x80 >> (msgApplePointer % 8)
				c.cfg.PlainKeys = dialect == "apple-plain"
			}
			keys := []Key{{XKControlL, true}, {'c', true}, {'c', false}, {XKControlL, false}, {XKControlL + 1, true}, {'d', true}, {'d', false}, {XKControlL + 1, false}, {XKSuperL, true}, {'q', true}, {'q', false}, {XKSuperL, false}, {XKAltL, true}, {XKShiftL, true}, {'A', true}, {'A', false}, {XKShiftL, false}, {XKAltL, false}, {RuneKeysym('你'), true}, {RuneKeysym('你'), false}, {XKControlL, true}}
			if err := c.KeyEvents(keys); err != nil {
				t.Fatal(err)
			}
			wantWrites := 1
			if c.apple {
				wantWrites = len(keys)
			}
			if conn.writes != wantWrites {
				t.Fatalf("keyboard writes=%d, want %d", conn.writes, wantWrites)
			}
			if c.apple && c.sealer.seq != uint32(len(keys)) {
				t.Fatalf("Apple keys were merged into %d records; want %d", c.sealer.seq, len(keys))
			}
			var reader io.Reader = &conn.Buffer
			if c.apple {
				reader = newRecordReader(reader, record)
			}
			block, _ := aes.NewCipher(c.wrapKey[:])
			for _, key := range keys {
				sym := key.Keysym
				if c.apple {
					sym = appleKeysym(sym)
				}
				size := 8
				if dialect == "apple" {
					size = 18
				}
				message := make([]byte, size)
				if _, err := io.ReadFull(reader, message); err != nil {
					t.Fatal(err)
				}
				if dialect == "apple" {
					if message[0] != msgApplePointer {
						t.Fatalf("wrong Apple message: %x", message)
					}
					block.Decrypt(message[2:], message[2:])
					message = message[2:]
					if message[0] != 0xff || message[1] != boolByte(key.Down) || be.Uint32(message[2:]) != sym {
						t.Fatalf("wrong Apple key: %x, want %+v", message, key)
					}
				} else if message[0] != 4 || message[1] != boolByte(key.Down) || be.Uint32(message[4:]) != sym {
					t.Fatalf("wrong plain key: %x, want %+v", message, key)
				}
			}
			if len(c.held) != 1 || !c.held[XKControlL] {
				t.Fatalf("batch lost held modifier state: %v", c.held)
			}
		})
	}
}

func TestKeyBatchBoundsWritesAndHonorsHandshake(t *testing.T) {
	conn := &keyWriteConn{}
	c := &Client{conn: conn, apple: true}
	keys := make([]Key, 300)
	for i := range keys {
		keys[i] = Key{'a', i%2 == 0}
	}
	if err := c.KeyEvents(keys); err != nil || conn.writes != 0 || len(c.held) != 0 {
		t.Fatal("input escaped before Apple record setup")
	}
	c.apple = false
	if err := c.KeyEvents(keys); err != nil {
		t.Fatal(err)
	}
	if conn.writes != 3 || conn.Len() != len(keys)*8 || len(c.held) != 0 {
		t.Fatalf("bounded batch lost input: writes=%d bytes=%d held=%v", conn.writes, conn.Len(), c.held)
	}
}

func BenchmarkKeyCommit(b *testing.B) {
	keys := []Key{{XKShiftL, true}, {'A', true}, {'A', false}, {XKShiftL, false}, {'b', true}, {'b', false}}
	for _, batch := range []bool{false, true} {
		b.Run(fmt.Sprintf("batch=%v", batch), func(b *testing.B) {
			conn := &keyWriteConn{}
			c := &Client{conn: conn, apple: true, appleExt: true, sealer: newRecordSealer(recordKeys{})}
			c.appleCmds[msgApplePointer/8] |= 0x80 >> (msgApplePointer % 8)
			b.ReportAllocs()
			for b.Loop() {
				conn.Reset()
				if batch {
					if err := c.KeyEvents(keys); err != nil {
						b.Fatal(err)
					}
				} else {
					for _, key := range keys {
						if err := c.KeyEvent(key.Keysym, key.Down); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
			b.ReportMetric(float64(conn.writes)/float64(b.N), "writes/commit")
		})
	}
}
