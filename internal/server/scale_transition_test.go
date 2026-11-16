package server

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"image"
	"io"
	"testing"
)

func TestScaleTransitionDropsStaleFrameWithoutAdvancingZlib(t *testing.T) {
	c := new(progressConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &rfbPeer{conn: c, ctx: ctx, cancel: cancel, state: &rfbState{encoding: encZlib, revision: 1}}
	e := new(pixelEncoder)
	defer e.close()
	var compressed, want []byte
	send := func(size image.Point, revision uint64, value byte) {
		t.Helper()
		pixels := bytes.Repeat([]byte{value, 17, 33, 255}, size.X*size.Y)
		c.data.Reset()
		if _, err := p.sendPixelFrame(e, encZlib, defaultRFBFormat, pixels, size, revision); err != nil {
			t.Fatal(err)
		}
		wire := c.data.Bytes()
		n := int(networkOrder.Uint16(wire[2:]))
		wire = wire[4:]
		for i := 0; i < n; i++ {
			if int(networkOrder.Uint16(wire[4:])) > size.X || int(networkOrder.Uint16(wire[6:])) > size.Y {
				t.Fatal("stale rectangle dimensions")
			}
			length := int(networkOrder.Uint32(wire[12:]))
			compressed = append(compressed, wire[16:16+length]...)
			wire = wire[16+length:]
		}
		want = append(want, bytes.Repeat([]byte{value, 17, 33, 0}, size.X*size.Y)...)
	}
	send(image.Pt(128, 128), 1, 7)
	p.state.revision = 2
	c.data.Reset()
	if _, err := p.sendPixelFrame(e, encZlib, defaultRFBFormat, bytes.Repeat([]byte{99}, 128*128*4), image.Pt(128, 128), 1); !errors.Is(err, renegotiate) {
		t.Fatalf("stale frame: %v", err)
	}
	if c.data.Len() != 0 {
		t.Fatal("old-size frame transmitted after layout transition")
	}
	send(image.Pt(64, 64), 2, 9)
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual := make([]byte, len(want))
	if _, err = io.ReadFull(reader, actual); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, want) {
		t.Fatal("scale transition corrupted continuous Zlib stream")
	}
}

func TestARDLayoutWaitsForViewerBeforeEncodingPixels(t *testing.T) {
	c := new(progressConn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &rfbState{encoding: encZlib, revision: 4, ardReady: make(chan struct{}), ardAwaitingRefresh: true}
	p := &rfbPeer{conn: c, ctx: ctx, cancel: cancel, state: state}
	encoder := new(pixelEncoder)
	defer encoder.close()
	pixels := bytes.Repeat([]byte{12, 34, 56, 255}, 64*64)
	if _, err := p.sendPixelFrame(encoder, encZlib, defaultRFBFormat, pixels, image.Pt(64, 64), 4); !errors.Is(err, renegotiate) {
		t.Fatalf("unready viewer received pixels: %v", err)
	}
	if c.data.Len() != 0 || encoder.plain != nil {
		t.Fatal("pixels/compression advanced before layout acknowledgment")
	}
	state.Lock()
	if !state.armARDFrameLocked() {
		t.Fatal("first request did not arm receiver")
	}
	if state.armARDFrameLocked() {
		t.Fatal("duplicate request rearmed the same layout")
	}
	state.Unlock()
	select {
	case <-state.ardReady:
	default:
		t.Fatal("capture remains blocked")
	}
	if _, err := p.sendPixelFrame(encoder, encZlib, defaultRFBFormat, pixels, image.Pt(64, 64), 4); err != nil {
		t.Fatal(err)
	}
	wire := c.data.Bytes()
	n := int(networkOrder.Uint32(wire[16:]))
	reader, err := zlib.NewReader(bytes.NewReader(wire[20 : 20+n]))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded := make([]byte, 64*64*4)
	if _, err = io.ReadFull(reader, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, bytes.Repeat([]byte{12, 34, 56, 0}, 64*64)) {
		t.Fatal("first displayed frame is corrupt")
	}
}
