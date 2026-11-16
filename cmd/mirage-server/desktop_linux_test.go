package main

import (
	"github.com/dyike/mirage-server/internal/server/portal"
	"github.com/godbus/dbus/v5"
	"image"
	"testing"
)

func TestPortalLayout(t *testing.T) {
	streams := []portal.Stream{{Node: 1, Properties: map[string]dbus.Variant{"position": dbus.MakeVariant([]int32{-1920, 0}), "size": dbus.MakeVariant([]int32{1920, 1080})}}, {Node: 2, Properties: map[string]dbus.Variant{"position": dbus.MakeVariant([]int32{0, 0}), "size": dbus.MakeVariant([]int32{1920, 1080})}}}
	screens, err := portalLayout(streams, 1920, 540)
	if err != nil {
		t.Fatal(err)
	}
	if screens[0].Bounds != image.Rect(0, 0, 960, 540) || screens[1].Bounds != image.Rect(960, 0, 1920, 540) {
		t.Fatalf("bad logical geometry %+v", screens)
	}
}
