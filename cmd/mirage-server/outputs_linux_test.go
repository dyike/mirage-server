package main

import (
	"context"
	"errors"
	"github.com/dyike/mirage-server/internal/server/input"
	"image"
	"testing"
)

func TestOutputChangesDebounce(t *testing.T) {
	current := []input.OutputInfo{{Name: "DP-2", Bounds: image.Rect(0, 0, 2400, 1350), Pixels: image.Pt(3840, 2160)}}
	changed := []input.OutputInfo{{Name: "DP-2", Bounds: image.Rect(0, 0, 3072, 1728), Pixels: image.Pt(3840, 2160)}}
	var c outputChange
	if c.observe(current, changed, nil) {
		t.Fatal("accepted transient geometry")
	}
	if c.observe(current, current, nil) {
		t.Fatal("unchanged topology triggered refresh")
	}
	if c.observe(current, changed, nil) {
		t.Fatal("debounce not reset")
	}
	if !c.observe(current, changed, nil) {
		t.Fatal("stable scale change was ignored")
	}
	c = outputChange{}
	if c.observe(current, nil, errors.New("unavailable")) {
		t.Fatal("discovery failure treated as hotplug")
	}
	if c.observe(current, nil, nil) {
		t.Fatal("transient removal triggered refresh")
	}
	if !c.observe(current, nil, nil) {
		t.Fatal("all outputs removed was ignored")
	}
	pixels := append([]input.OutputInfo(nil), current...)
	pixels[0].Pixels = image.Pt(2560, 1440)
	c = outputChange{}
	c.observe(current, pixels, nil)
	if !c.observe(current, pixels, nil) {
		t.Fatal("mode change with unchanged logical geometry was ignored")
	}
}
func TestOutputWaitCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitForOutputs(ctx, func() ([]input.OutputInfo, error) { t.Fatal("discovery after cancellation"); return nil, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if watchOutputChanges(ctx, nil, func() ([]input.OutputInfo, error) { t.Fatal("watch after cancellation"); return nil, nil }) {
		t.Fatal("shutdown triggered reload")
	}
}
