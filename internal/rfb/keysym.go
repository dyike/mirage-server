package rfb

// X11 keysyms used by KeyEvent (RFC 6143 §7.5.4).
const (
	XKBackSpace = 0xff08
	XKTab       = 0xff09
	XKReturn    = 0xff0d
	XKEscape    = 0xff1b
	XKDelete    = 0xffff
	XKHome      = 0xff50
	XKLeft      = 0xff51
	XKUp        = 0xff52
	XKRight     = 0xff53
	XKDown      = 0xff54
	XKPageUp    = 0xff55
	XKPageDown  = 0xff56
	XKEnd       = 0xff57
	XKInsert    = 0xff63
	XKKPEnter   = 0xff8d
	XKF1        = 0xffbe // F2..F12 follow consecutively
	XKShiftL    = 0xffe1
	XKControlL  = 0xffe3
	XKCapsLock  = 0xffe5
	XKMetaL     = 0xffe7
	XKAltL      = 0xffe9
	XKSuperL    = 0xffeb
)

// RuneKeysym maps a character to its keysym: Latin-1 directly, everything
// else through the Unicode keysym range.
func RuneKeysym(r rune) uint32 {
	if r >= 0x20 && r <= 0x7e || r >= 0xa0 && r <= 0xff {
		return uint32(r)
	}
	return 0x01000000 | uint32(r)
}
