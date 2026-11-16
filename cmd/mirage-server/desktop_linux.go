package main

import (
	"context"
	"fmt"
	"github.com/dyike/mirage-server/internal/server/input"
	"image"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/dyike/mirage-server/internal/server"
	"github.com/dyike/mirage-server/internal/server/portal"
	"github.com/godbus/dbus/v5"
)

func portalLayout(streams []portal.Stream, width, height int) ([]server.DesktopScreen, error) {
	if len(streams) == 1 {
		return []server.DesktopScreen{{ID: 1, Bounds: image.Rect(0, 0, width, height)}}, nil
	}
	bounds := make([]image.Rectangle, len(streams))
	var union image.Rectangle
	right := 0
	for i, s := range streams {
		var size, pos []int32
		if v, ok := s.Properties["size"]; ok {
			dbus.Store([]any{v.Value()}, &size)
		}
		if len(size) != 2 || size[0] <= 0 || size[1] <= 0 {
			return nil, fmt.Errorf("portal omitted size for stream %d", i+1)
		}
		if v, ok := s.Properties["position"]; ok {
			dbus.Store([]any{v.Value()}, &pos)
		}
		if len(pos) != 2 {
			pos = []int32{int32(right), 0}
		}
		r := image.Rect(int(pos[0]), int(pos[1]), int(pos[0]+size[0]), int(pos[1]+size[1]))
		bounds[i] = r
		union = union.Union(r)
		right = r.Max.X
	}
	result := make([]server.DesktopScreen, len(streams))
	for i, r := range bounds {
		r = r.Sub(union.Min)
		result[i] = server.DesktopScreen{ID: uint32(i + 1), Bounds: image.Rect(r.Min.X*width/union.Dx(), r.Min.Y*height/union.Dy(), r.Max.X*width/union.Dx(), r.Max.Y*height/union.Dy())}
	}
	return result, nil
}

// Hyprland reapplies monitor rules after output-management transactions. Update
// the runtime rule too, otherwise a successful resize can immediately revert.
func configureDynamicOutput(ctx context.Context, change input.OutputMode) error {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return input.ConfigureOutputs(ctx, []input.OutputMode{change})
	}
	// Output names are compositor-provided but must still be safe Lua literals.
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(change.Name) {
		return fmt.Errorf("unsupported dynamic output name %q", change.Name)
	}
	code := fmt.Sprintf(`hl.monitor({output=%q,mode="%dx%d@60",position="%dx%d",scale=1})`, change.Name, change.Width, change.Height, change.X, change.Y)
	result, err := exec.CommandContext(ctx, "hyprctl", "eval", code).CombinedOutput()
	if err != nil {
		return fmt.Errorf("configure virtual output: %w: %s", err, result)
	}
	if strings.TrimSpace(string(result)) != "ok" {
		return fmt.Errorf("configure virtual output: %s", result)
	}
	return nil
}
