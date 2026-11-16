package rfb

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dyike/mirage-server/internal/clip"
)

func TestConfirmedClipboardBeforeFirstPicturePollsAndRejectsStaleReply(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	local.SetDeadline(time.Now().Add(3 * time.Second))
	peer.SetDeadline(time.Now().Add(3 * time.Second))
	want := clip.Content{MIME: clip.Text, Data: []byte("Android 新复制 👋")}
	old := clip.Content{MIME: clip.Text, Data: []byte("old Mac value")}
	updates := make(chan clip.Content, 4)
	c := &Client{conn: local, r: bufio.NewReader(local), cfg: Config{OnClipboard: func(value clip.Content) { updates <- value }}}
	c.clipboardActive.Store(true)
	serverDone := make(chan error, 1)
	go func() {
		var header [16]byte
		if _, err := io.ReadFull(peer, header[:]); err != nil {
			serverDone <- err
			return
		}
		if _, err := io.CopyN(io.Discard, peer, int64(be.Uint32(header[12:]))); err != nil {
			serverDone <- err
			return
		}
		for attempt := range 4 {
			var fetch [8]byte
			if _, err := io.ReadFull(peer, fetch[:]); err != nil {
				serverDone <- err
				return
			}
			if fetch[0] != msgAppleFetchClipboard {
				serverDone <- protoErr("pasteboard was rewritten while waiting for acceptance")
				return
			}
			value := old
			if attempt == 3 {
				value = want
			}
			reply, err := clipboardMessage(value)
			if err == nil {
				_, err = peer.Write(reply)
			}
			if err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	readerDone := make(chan error, 1)
	go func() {
		for range 4 {
			if _, err := c.r.ReadByte(); err != nil {
				readerDone <- err
				return
			}
			if err := c.receiveClipboard(); err != nil {
				readerDone <- err
				return
			}
		}
		readerDone <- nil
	}()
	if err := c.SetClipboardConfirmed(want); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	if c.pictureFrames.Load() != 0 {
		t.Fatal("clipboard required a video picture")
	}
	if len(updates) != 1 || (<-updates).Hash() != want.Hash() {
		t.Fatal("stale Mac reply reached the local clipboard")
	}
}

func clipboardClient() (*Client, *keyWriteConn) {
	conn := &keyWriteConn{}
	c := &Client{conn: conn, apple: true, appleExt: true, cfg: Config{MediaStream: true, OnClipboard: func(clip.Content) {}}}
	c.sealer = newRecordSealer(recordKeys{})
	c.pictureFrames.Store(1)
	for _, command := range []byte{msgAppleFetchClipboard, msgAppleAutoClipboard, msgAppleClipboard} {
		c.appleCmds[command/8] |= 0x80 >> (command % 8)
	}
	return c, conn
}

func clipboardPicture(t *testing.T) clip.Content {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.SetNRGBA(1, 0, color.NRGBA{R: 123, G: 211, B: 33, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	content, err := (clip.Content{MIME: clip.PNG, Data: data.Bytes()}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestAppleClipboardEncryptedBothDirections(t *testing.T) {
	for _, content := range []clip.Content{{MIME: clip.Text, Data: []byte("双向复制 🐈\nA → B → A")}, clipboardPicture(t)} {
		t.Run(content.MIME, func(t *testing.T) {
			c, conn := clipboardClient()
			if err := c.startClipboard(); err != nil {
				t.Fatal(err)
			}
			if err := c.SetClipboard(content); err != nil {
				t.Fatal(err)
			}
			reader := newRecordReader(&conn.Buffer, recordKeys{})
			setup := make([]byte, 16)
			if _, err := io.ReadFull(reader, setup); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(setup, []byte{0x15, 0, 0, 1, 0, 0, 0, 0, 0x0b, 0, 0, 0, 0, 0, 0, 1}) {
				t.Fatalf("clipboard negotiation = %x", setup)
			}
			var header [16]byte
			if _, err := io.ReadFull(reader, header[:]); err != nil {
				t.Fatal(err)
			}
			if header[0] != 0x1f || header[2] != 0 {
				t.Fatalf("clipboard header = %x", header)
			}
			packed := make([]byte, be.Uint32(header[12:]))
			if _, err := io.ReadFull(reader, packed); err != nil {
				t.Fatal(err)
			}
			zr, err := zlib.NewReader(bytes.NewReader(packed))
			if err != nil {
				t.Fatal(err)
			}
			archive := make([]byte, be.Uint32(header[8:]))
			if _, err := io.ReadFull(zr, archive); err != nil {
				t.Fatal(err)
			}
			zr.Close()
			wantType := "public.utf8-plain-text"
			if content.MIME == clip.PNG {
				wantType = "public.png"
			}
			if be.Uint32(archive) != 1 || !bytes.Contains(archive, []byte(wantType)) || !bytes.HasSuffix(archive, content.Data) {
				t.Fatal("clipboard flavor or payload changed")
			}
			// Feed the peer's encrypted pasteboard and a following Bell to
			// verify the receiver consumes precisely this message's bytes.
			message := append(header[:], packed...)
			message = append(message, 2)
			var got clip.Content
			c.cfg.OnClipboard = func(value clip.Content) { got = value }
			c.r = bufio.NewReader(newRecordReader(bytes.NewReader(newRecordSealer(recordKeys{}).seal(message)), recordKeys{}))
			if _, err := c.readMessage(); err != nil {
				t.Fatal(err)
			}
			if got.MIME != content.MIME || !bytes.Equal(got.Data, content.Data) {
				t.Fatal("received clipboard differs")
			}
			if kind, err := c.readMessage(); err != nil || kind != 2 {
				t.Fatal("clipboard consumed the next server message", kind, err)
			}
		})
	}
}

func TestAppleClipboardRequiresHighPerformanceAndSharing(t *testing.T) {
	for _, disabled := range []string{"ordinary", "sharing", "capability"} {
		t.Run(disabled, func(t *testing.T) {
			c, conn := clipboardClient()
			switch disabled {
			case "ordinary":
				c.cfg.MediaStream = false
			case "sharing":
				c.cfg.OnClipboard = nil
			case "capability":
				c.appleCmds = [16]byte{}
			}
			if err := c.startClipboard(); err != nil || c.RichClipboard() || conn.Len() != 0 {
				t.Fatal("disabled clipboard negotiated", err)
			}
			if c.SetClipboard(clip.Content{MIME: clip.Text, Data: []byte("private")}) == nil || conn.Len() != 0 {
				t.Fatal("disabled clipboard sent data")
			}
		})
	}
	c, conn := clipboardClient()
	if err := c.startClipboard(); err != nil {
		t.Fatal(err)
	}
	c.stopClipboard()
	if c.RichClipboard() || conn.Len() == 0 {
		t.Fatal("clipboard remained enabled after media fallback")
	}
}

func TestClipboardMalformedAndUnsupportedPayloadDoesNotReplaceLocalData(t *testing.T) {
	c, _ := clipboardClient()
	called := false
	c.cfg.OnClipboard = func(clip.Content) { called = true }
	c.clipboardActive.Store(true)
	for _, archive := range [][]byte{{0, 0, 0, 255}, {0, 0, 0, 1}, {0, 0, 0, 0}} {
		var packed bytes.Buffer
		zw := zlib.NewWriter(&packed)
		zw.Write(archive)
		zw.Close()
		message := []byte{0x1f, 0, 0, 0, 0, 0, 0, 0}
		message = be.AppendUint32(message, uint32(len(archive)))
		message = be.AppendUint32(message, uint32(packed.Len()))
		message = append(message, packed.Bytes()...)
		message = append(message, 2)
		c.r = bufio.NewReader(bytes.NewReader(message))
		if _, err := c.readMessage(); err != nil || called {
			t.Fatal("malformed clipboard replaced data or disconnected", err)
		}
		if kind, err := c.readMessage(); err != nil || kind != 2 {
			t.Fatal("malformed clipboard desynchronized reader")
		}
	}
	oversized := make([]byte, 15)
	be.PutUint32(oversized[7:], maxClipboardArchive+1)
	c.r = bufio.NewReader(bytes.NewReader(oversized))
	if err := c.receiveClipboard(); err == nil {
		t.Fatal("oversized clipboard accepted")
	}
}

func TestClipboardChangeStatusFetchesCurrentContent(t *testing.T) {
	c, conn := clipboardClient()
	c.clipboardActive.Store(true)
	c.r = bufio.NewReader(bytes.NewReader([]byte{0x14, 0, 0, 4, 0, 0, 0, 2}))
	if _, err := c.readMessage(); err != nil {
		t.Fatal(err)
	}
	reader := newRecordReader(&conn.Buffer, recordKeys{})
	var fetch [8]byte
	if _, err := io.ReadFull(reader, fetch[:]); err != nil || fetch[0] != 0x0b {
		t.Fatal("clipboard change did not trigger fetch", err)
	}
}

func TestClipboardWriteDoesNotDependOnVideoPicture(t *testing.T) {
	c, conn := clipboardClient()
	c.clipboardActive.Store(true)
	c.pictureFrames.Store(0)
	if err := c.SetClipboard(clip.Content{MIME: clip.Text, Data: []byte("ready without video")}); err != nil || conn.Len() == 0 {
		t.Fatal("video readiness blocked the clipboard channel", err)
	}
}
