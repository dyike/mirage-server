package rfb

import (
	"bytes"
	"compress/zlib"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/dyike/mirage-server/internal/clip"
)

type clipboardConfirmation struct {
	hash     [32]byte
	done     chan struct{}
	accepted bool // clipboardMu
}

const (
	msgAppleFetchClipboard = 0x0b
	msgAppleAutoClipboard  = 0x15
	msgAppleClipboard      = 0x1f
	maxClipboardArchive    = clip.MaxBytes + 64<<10
)

// RichClipboard is negotiated only for Mac high-performance connections with
// sharing enabled. Other protocols retain their existing clipboard path.
func (c *Client) RichClipboard() bool { return c.clipboardActive.Load() }

func (c *Client) startClipboard() error {
	if !c.cfg.MediaStream || c.cfg.OnClipboard == nil || !c.appleAccepts(msgAppleAutoClipboard) || !c.appleAccepts(msgAppleFetchClipboard) || !c.appleAccepts(msgAppleClipboard) {
		return nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.send([]byte{msgAppleAutoClipboard, 0, 0, 1, 0, 0, 0, 0}); err != nil {
		return err
	}
	c.clipboardActive.Store(true)
	c.clipboardSequence = 1
	return c.send(be.AppendUint32([]byte{msgAppleFetchClipboard, 0, 0, 0}, c.clipboardSequence))
}

func (c *Client) fetchClipboard() error {
	if !c.RichClipboard() {
		return nil
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.clipboardSequence++
	return c.send(be.AppendUint32([]byte{msgAppleFetchClipboard, 0, 0, 0}, c.clipboardSequence))
}

func (c *Client) stopClipboard() {
	if !c.clipboardActive.Swap(false) {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.send([]byte{msgAppleAutoClipboard, 0, 0, 2, 0, 0, 0, 0})
}

// SetClipboard sends one complete text/image representation in the encrypted
// Apple record stream; compression happens before acquiring the input lock.
func (c *Client) SetClipboard(content clip.Content) error {
	if !c.RichClipboard() {
		return clip.ErrUnsupported
	}
	content, err := content.Normalize()
	if err != nil {
		return err
	}
	message, err := clipboardMessage(content)
	if err != nil {
		return err
	}
	return c.sendClipboard(message)
}

// SetClipboardConfirmed waits for the Mac to return the offered value before
// a paste key is sent. Clipboard readiness is independent of video output:
// the first decoded picture can arrive much later on a mobile client.
func (c *Client) SetClipboardConfirmed(content clip.Content) error {
	c.clipboardSendMu.Lock()
	defer c.clipboardSendMu.Unlock()
	if !c.RichClipboard() {
		return clip.ErrUnsupported
	}
	content, err := content.Normalize()
	if err != nil {
		return err
	}
	message, err := clipboardMessage(content)
	if err != nil {
		return err
	}
	offer := &clipboardConfirmation{hash: content.Hash(), done: make(chan struct{})}
	c.clipboardMu.Lock()
	c.clipboardOffer = offer
	c.clipboardMu.Unlock()
	defer func() {
		c.clipboardMu.Lock()
		c.clipboardOffer = nil
		c.clipboardMu.Unlock()
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	// TCP carries the complete offer. Rewriting the entire pasteboard
	// on each poll can race the agent's asynchronous publication, and floods
	// the control channel for images. Poll for acceptance without rewriting.
	if err := c.sendClipboard(message); err != nil {
		return err
	}
	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()
	for {
		if err := c.fetchClipboard(); err != nil {
			return err
		}
		select {
		case <-offer.done:
			return nil
		case <-deadline.C:
			return errors.New("Mac has not accepted the clipboard yet; try Paste again")
		case <-poll.C:
		}
	}
}

func clipboardMessage(content clip.Content) ([]byte, error) {
	// Callers have already validated and normalized the clipboard.
	flavor := "public.utf8-plain-text"
	if content.MIME == clip.PNG {
		flavor = "public.png"
	}
	archive := be.AppendUint32(nil, 1) // one flavor, one item
	archive = be.AppendUint32(archive, uint32(len(flavor)))
	archive = append(archive, flavor...)
	archive = be.AppendUint32(archive, 0) // flags
	archive = be.AppendUint32(archive, 0) // tags
	archive = be.AppendUint32(archive, uint32(len(content.Data)))
	archive = append(archive, content.Data...)
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(archive); err != nil {
		return nil, err
	}
	// Apple's pasteboard uses a flushed zlib stream without a final trailer.
	if err := zw.Flush(); err != nil {
		return nil, err
	}
	message := []byte{msgAppleClipboard, 0, 0, 0, 0, 0, 0, 0}
	message = be.AppendUint32(message, uint32(len(archive)))
	message = be.AppendUint32(message, uint32(compressed.Len()))
	message = append(message, compressed.Bytes()...)
	return message, nil
}

func (c *Client) sendClipboard(message []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.inputBlocked() || !c.RichClipboard() {
		return clip.ErrUnsupported
	}
	return c.send(message)
}

func (c *Client) receiveClipboard() error {
	var header [15]byte
	if _, err := io.ReadFull(c.r, header[:]); err != nil {
		return err
	}
	plainSize, packedSize := be.Uint32(header[7:]), be.Uint32(header[11:])
	if plainSize > maxClipboardArchive || packedSize > maxClipboardArchive {
		return protoErr("oversized Apple clipboard")
	}
	if !c.RichClipboard() {
		_, err := c.r.Discard(int(packedSize))
		return err
	}
	packed := make([]byte, int(packedSize))
	if _, err := io.ReadFull(c.r, packed); err != nil {
		return err
	}
	if header[1]&1 != 0 {
		return c.fetchClipboard()
	} // advertised types; fetch their data
	if plainSize == 0 {
		return nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil
	} // malformed clipboard must not disconnect desktop input
	plain, err := io.ReadAll(io.LimitReader(zr, int64(plainSize)+1))
	zr.Close()
	if len(plain) != int(plainSize) || (err != nil && !errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil
	}
	if content, err := decodeClipboard(plain); err == nil {
		c.clipboardMu.Lock()
		offer := c.clipboardOffer
		stale := offer != nil && content.Hash() != offer.hash
		if offer != nil && !stale && !offer.accepted {
			offer.accepted = true
			close(offer.done)
		}
		c.clipboardMu.Unlock()
		if stale {
			return nil
		}
		c.cfg.OnClipboard(content)
	}
	return nil
}

func decodeClipboard(archive []byte) (clip.Content, error) {
	// Each item starts with a flavor count; all fields are big-endian counted
	// byte strings. Tags and unsupported representations are consumed, not used.
	r := bytes.NewReader(archive)
	word := func() (uint32, error) { var b [4]byte; _, err := io.ReadFull(r, b[:]); return be.Uint32(b[:]), err }
	field := func(limit uint32) ([]byte, error) {
		n, err := word()
		if err != nil || n > limit || uint64(n) > uint64(r.Len()) {
			return nil, io.ErrUnexpectedEOF
		}
		b := make([]byte, n)
		_, err = io.ReadFull(r, b)
		return b, err
	}
	var text, picture clip.Content
	for items := 0; r.Len() > 0; items++ {
		count, err := word()
		if err != nil || count > 1024 || items >= 256 {
			return clip.Content{}, protoErr("invalid clipboard item")
		}
		for range count {
			kind, err := field(4096)
			if err != nil {
				return clip.Content{}, err
			}
			if _, err = word(); err != nil {
				return clip.Content{}, err
			}
			tags, err := word()
			if err != nil || tags > 256 {
				return clip.Content{}, protoErr("invalid clipboard tags")
			}
			for range tags {
				if _, err = field(4096); err != nil {
					return clip.Content{}, err
				}
				if _, err = field(4096); err != nil {
					return clip.Content{}, err
				}
			}
			data, err := field(clip.MaxBytes)
			if err != nil {
				return clip.Content{}, err
			}
			if len(data) == 0 {
				continue
			} // a promised flavor has no bytes yet
			switch string(kind) {
			case "public.utf8-plain-text":
				text = clip.Content{MIME: clip.Text, Data: bytes.TrimSuffix(data, []byte{0})}
			case "public.utf16-plain-text":
				if text.MIME != "" || len(data)%2 != 0 {
					continue
				}
				u := make([]uint16, len(data)/2)
				for i := range u {
					u[i] = be.Uint16(data[2*i:])
				}
				text = clip.Content{MIME: clip.Text, Data: []byte(strings.TrimSuffix(string(utf16.Decode(u)), "\x00"))}
			case "public.png", "public.tiff", "public.jpeg", "com.compuserve.gif", "org.webmproject.webp", "com.microsoft.bmp":
				mime := map[string]string{"public.png": clip.PNG, "public.tiff": "image/tiff", "public.jpeg": "image/jpeg", "com.compuserve.gif": "image/gif", "org.webmproject.webp": "image/webp", "com.microsoft.bmp": "image/bmp"}[string(kind)]
				if content, err := (clip.Content{MIME: mime, Data: data}).Normalize(); err == nil {
					picture = content
				}
			}
		}
	}
	if picture.MIME != "" {
		return picture, nil
	}
	if text.MIME != "" {
		return text.Normalize()
	}
	return clip.Content{}, clip.ErrUnsupported
}
