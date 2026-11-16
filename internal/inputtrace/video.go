package inputtrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Video writes metadata only, under an explicit diagnostic launch flag.
// Separate event files preserve failures even if a later connection recovers.
func Video(event string, details any) {
	if os.Getenv("MIRAGE_VIDEO_TRACE") != "1" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, "Documents", "mirage-video-trace")
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	data, err := json.Marshal(struct {
		Time    time.Time
		Details any
	}{time.Now(), details})
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, event+".tmp")
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, event+".json"))
	}
}
