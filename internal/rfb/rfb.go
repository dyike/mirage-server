// Package rfb is a client for the Remote Framebuffer protocol (RFC 6143),
// the protocol behind VNC and macOS Screen Sharing.
//
// The client decodes straight into one RGBA framebuffer and reports the
// rectangles each update changed. It never draws: callers copy the dirty
// regions out (see frame.Framebuffer) on their own schedule.
package rfb

import "fmt"

// Encoding numbers from RFC 6143 §7.7 and the community registry.
const (
	EncRaw        int32 = 0
	EncCopyRect   int32 = 1
	EncRRE        int32 = 2
	EncHextile    int32 = 5
	EncTight      int32 = 7
	EncZRLE       int32 = 16
	EncH264       int32 = 50
	EncMirageHEVC int32 = 0x4d485643 // Private opt-in server extension.

	// EncAppleRGB555 is macOS Screen Sharing's 15-bit encoding (0x3ea),
	// one of its "adaptive quality" tiers; see apple555.go.
	EncAppleRGB555 int32 = 1002

	EncPseudoDesktopSize         int32 = -223
	EncPseudoLastRect            int32 = -224
	EncPseudoCursor              int32 = -239
	EncPseudoExtendedDesktopSize int32 = -308
	EncPseudoContinuousUpdates   int32 = -313
	EncPseudoJPEGQuality7        int32 = -25 // Tight JPEG quality hint: -32 + 7
)

// Security types (RFC 6143 §7.2 and Apple Remote Desktop).
const (
	SecInvalid  uint8 = 0
	SecNone     uint8 = 1
	SecVNC      uint8 = 2
	SecRA2      uint8 = 5
	SecVeNCrypt uint8 = 19
	SecRA256    uint8 = 129
	SecARD      uint8 = 30 // Apple Remote Desktop: Diffie-Hellman + AES-128
)

// Pointer button mask bits for PointerEvent.
const (
	ButtonLeft       uint8 = 1 << 0
	ButtonMiddle     uint8 = 1 << 1
	ButtonRight      uint8 = 1 << 2
	ButtonWheelUp    uint8 = 1 << 3
	ButtonWheelDown  uint8 = 1 << 4
	ButtonWheelLeft  uint8 = 1 << 5
	ButtonWheelRight uint8 = 1 << 6
)

// PixelFormat is the wire pixel layout (RFC 6143 §7.4).
type PixelFormat struct {
	BPP, Depth                      uint8
	BigEndian, TrueColor            bool
	RedMax, GreenMax, BlueMax       uint16
	RedShift, GreenShift, BlueShift uint8
}

// rgbaFormat makes the server send bytes in image.RGBA order (R, G, B, pad):
// 32 bpp little-endian with red in the lowest byte. Decoders then copy
// pixels without conversion and only force the alpha byte. noVNC asks for
// the same format, so macOS Screen Sharing and common servers honour it.
var rgbaFormat = PixelFormat{
	BPP: 32, Depth: 24, TrueColor: true,
	RedMax: 255, GreenMax: 255, BlueMax: 255,
	RedShift: 0, GreenShift: 8, BlueShift: 16,
}

func (p PixelFormat) marshal(b []byte) {
	b[0], b[1] = p.BPP, p.Depth
	b[2], b[3] = boolByte(p.BigEndian), boolByte(p.TrueColor)
	be.PutUint16(b[4:], p.RedMax)
	be.PutUint16(b[6:], p.GreenMax)
	be.PutUint16(b[8:], p.BlueMax)
	b[10], b[11], b[12] = p.RedShift, p.GreenShift, p.BlueShift
	b[13], b[14], b[15] = 0, 0, 0
}

func parsePixelFormat(b []byte) PixelFormat {
	return PixelFormat{
		BPP: b[0], Depth: b[1],
		BigEndian: b[2] != 0, TrueColor: b[3] != 0,
		RedMax: be.Uint16(b[4:]), GreenMax: be.Uint16(b[6:]), BlueMax: be.Uint16(b[8:]),
		RedShift: b[10], GreenShift: b[11], BlueShift: b[12],
	}
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// ProtocolError reports a malformed or unsupported server message.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "rfb: " + e.Msg }

func protoErr(format string, args ...any) error {
	return &ProtocolError{Msg: fmt.Sprintf(format, args...)}
}

// AuthError reports that the server rejected the credentials.
type AuthError struct{ Reason string }

func (e *AuthError) Error() string {
	if e.Reason == "" {
		return "rfb: authentication failed"
	}
	return "rfb: authentication failed: " + e.Reason
}
