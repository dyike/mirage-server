package server

import (
	"bytes"
	"image"
	"testing"

	"github.com/pion/rtp"
)

func TestAppleHEVCPacketization(t *testing.T) {
	p := &ardHEVCPayloader{width: 3840, height: 2160, don: 0xffff}
	for _, size := range []int{20, 3600} {
		nal := append([]byte{19 << 1, 1}, bytes.Repeat([]byte{17}, size)...)
		input := append([]byte{0, 0, 0, 1, 32 << 1, 1, 7, 8, 9, 0, 0, 0, 1}, nal...)
		packets := p.Payload(1150, input)
		if len(packets) < 2 {
			t.Fatal("missing parameter sets or VCL")
		}
		if p.don == 0 {
			t.Fatal("zero DON is invalid in native receiver")
		}
		var reconstructed []byte
		for _, b := range packets {
			if len(b) > 1150 {
				t.Fatal("oversized RTP payload")
			}
			typ := (b[0] >> 1) & 63
			if typ == 48 {
				if networkOrder.Uint16(b[2:]) != p.don || int(networkOrder.Uint16(b[4:])) != len(b)-6 {
					t.Fatal("invalid AP")
				}
				continue
			}
			offset := 2
			if typ == 49 {
				offset = 3
			}
			if networkOrder.Uint16(b[offset:]) != p.don {
				t.Fatal("DON missing from fragment")
			}
			if typ == 49 {
				if b[2]&0xc0 == 0xc0 {
					t.Fatal("FU cannot be both start and end")
				}
				if b[2]&0x80 != 0 {
					reconstructed = append(reconstructed, (b[0]&0x81)|((b[2]&63)<<1), b[1])
				}
				reconstructed = append(reconstructed, b[5:]...)
			} else {
				reconstructed = append(reconstructed, b[:2]...)
				reconstructed = append(reconstructed, b[4:]...)
			}
		}
		if !bytes.Equal(reconstructed[:len(nal)], nal) {
			t.Fatal("compressed image bytes changed")
		}
		// Independent emulation-prevention removal for the presentation trailer.
		var trailer []byte
		zeros := 0
		for _, v := range reconstructed[len(nal):] {
			if zeros >= 2 && v == 3 {
				zeros = 0
				continue
			}
			trailer = append(trailer, v)
			if v == 0 {
				zeros++
			} else {
				zeros = 0
			}
		}
		if len(trailer) != 19 || trailer[18] != 0x1b || networkOrder.Uint16(trailer[4:]) != 3840 || networkOrder.Uint16(trailer[6:]) != 2160 || networkOrder.Uint16(trailer[8:]) != 3840 || networkOrder.Uint16(trailer[10:]) != 2160 {
			t.Fatalf("invalid presentation trailer %x", trailer)
		}
	}
}

func TestAppleRTPFrameExtension(t *testing.T) {
	packet := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 100, Extension: true, ExtensionProfile: 0x9311}, Payload: []byte{1, 2, 3}}
	ext := []byte{0, 7, 0, 42}
	if err := packet.SetExtension(0, ext); err != nil {
		t.Fatal(err)
	}
	wire, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire[12:20], []byte{0x93, 0x11, 0, 1, 0, 7, 0, 42}) {
		t.Fatalf("incorrect Apple RTP extension: %x", wire)
	}
	var decoded rtp.Packet
	if err = decoded.Unmarshal(wire); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.GetExtension(0), ext) {
		t.Fatal("extension failed round trip")
	}
}

func TestNativeHEVCCanvasSurvivesSelectionAcknowledgment(t *testing.T) {
	p := &rfbPeer{server: &RFBServer{Width: 4608, Height: 2592, AvailableScreens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 2560, 1440)}}}, state: &rfbState{desktop: DesktopLayout{Size: image.Pt(3840, 2160), Screens: []DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, 3840, 2160)}}}}}
	before := p.state.desktop.Size
	if err := p.selectNativeDisplay(1); err != nil {
		t.Fatal(err)
	}
	if p.state.desktop.Size != before || p.state.revision != 0 {
		t.Fatal("native configured canvas reset by display acknowledgment")
	}
}
