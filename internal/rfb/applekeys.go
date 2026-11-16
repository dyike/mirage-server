package rfb

import "crypto/aes"

// In the Apple dialect keys go out as message 0x10, encrypted under the
// record key like button changes, carrying the Mac virtual key code next to
// the keysym, as Apple's viewer sends them: the key code lets macOS apply
// its own keyboard layout and input source. Plain KeyEvents also reached
// the lock screen's password field in a test, so this is not about secure
// input.

const (
	keyFlagCodeValid  = 1
	keyFlagPreferCode = 2
	keyboardANSI      = 40
)

// macKeyCodes maps X11 keysyms to kVK_* virtual key codes (HIToolbox
// Events.h) for an ANSI keyboard. Shifted symbols use their key's code;
// the client sends Shift itself.
var macKeyCodes = func() map[uint32]uint16 {
	m := map[uint32]uint16{
		0xff0d: 0x24, 0xff09: 0x30, 0x20: 0x31, 0xff08: 0x33, 0xff1b: 0x35, // Return Tab Space BackSpace Escape
		// Super is Command. Meta is Option: appleKeysym turns Alt into it.
		0xffeb: 0x37, 0xffec: 0x36, // Super L/R: Command
		0xffe7: 0x3a, 0xffe8: 0x3d, 0xffe9: 0x3a, 0xffea: 0x3d, // Meta, Alt L/R: Option
		0xffe1: 0x38, 0xffe2: 0x3c, 0xffe5: 0x39, // Shift L/R, Caps Lock
		0xffe3: 0x3b, 0xffe4: 0x3e, // Control L/R
		0xff50: 0x73, 0xff55: 0x74, 0xffff: 0x75, 0xff57: 0x77, 0xff56: 0x79, // Home PgUp Delete End PgDn
		0xff51: 0x7b, 0xff53: 0x7c, 0xff54: 0x7d, 0xff52: 0x7e, // arrows
		0xffbe: 0x7a, 0xffbf: 0x78, 0xffc0: 0x63, 0xffc1: 0x76, 0xffc2: 0x60, 0xffc3: 0x61, // F1-F6
		0xffc4: 0x62, 0xffc5: 0x64, 0xffc6: 0x65, 0xffc7: 0x6d, 0xffc8: 0x67, 0xffc9: 0x6f, // F7-F12
	}
	keys := []struct {
		plain, shifted rune
		code           uint16
	}{
		{'a', 'A', 0x00}, {'s', 'S', 0x01}, {'d', 'D', 0x02}, {'f', 'F', 0x03}, {'h', 'H', 0x04},
		{'g', 'G', 0x05}, {'z', 'Z', 0x06}, {'x', 'X', 0x07}, {'c', 'C', 0x08}, {'v', 'V', 0x09},
		{'b', 'B', 0x0b}, {'q', 'Q', 0x0c}, {'w', 'W', 0x0d}, {'e', 'E', 0x0e}, {'r', 'R', 0x0f},
		{'y', 'Y', 0x10}, {'t', 'T', 0x11}, {'1', '!', 0x12}, {'2', '@', 0x13}, {'3', '#', 0x14},
		{'4', '$', 0x15}, {'6', '^', 0x16}, {'5', '%', 0x17}, {'=', '+', 0x18}, {'9', '(', 0x19},
		{'7', '&', 0x1a}, {'-', '_', 0x1b}, {'8', '*', 0x1c}, {'0', ')', 0x1d}, {']', '}', 0x1e},
		{'o', 'O', 0x1f}, {'u', 'U', 0x20}, {'[', '{', 0x21}, {'i', 'I', 0x22}, {'p', 'P', 0x23},
		{'l', 'L', 0x25}, {'j', 'J', 0x26}, {'\'', '"', 0x27}, {'k', 'K', 0x28}, {';', ':', 0x29},
		{'\\', '|', 0x2a}, {',', '<', 0x2b}, {'/', '?', 0x2c}, {'n', 'N', 0x2d}, {'m', 'M', 0x2e},
		{'.', '>', 0x2f}, {'`', '~', 0x32},
	}
	for _, k := range keys {
		m[uint32(k.plain)], m[uint32(k.shifted)] = k.code, k.code
	}
	return m
}()

// appleKeyMessage builds an encrypted key event. wmu held (it reads the
// record key).
func (c *Client) appleKeyMessage(keysym uint32, down bool) ([]byte, error) {
	m := make([]byte, 18)
	m[0] = msgApplePointer // 0x10 carries both; block byte 0 tells them apart
	blk := m[2:]
	blk[0], blk[1] = 0xff, boolByte(down)
	be.PutUint32(blk[2:], keysym)
	if code, ok := macKeyCodes[keysym]; ok {
		m[1] = keyFlagCodeValid | keyFlagPreferCode
		be.PutUint16(blk[12:], keyboardANSI)
		be.PutUint16(blk[14:], code)
	}
	b, err := aes.NewCipher(c.wrapKey[:])
	if err != nil {
		return nil, err
	}
	b.Encrypt(blk, blk)
	return m, nil
}
