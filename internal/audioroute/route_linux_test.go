package audioroute

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type fakePulse struct {
	def     string
	sinks   []sink
	streams []stream
	loads   int
}

func (f *fakePulse) command(a ...string) ([]byte, error) {
	switch a[0] {
	case "get-default-sink":
		return []byte(f.def), nil
	case "-f":
		if a[3] == "sinks" {
			return json.Marshal(f.sinks)
		}
		return json.Marshal(f.streams)
	case "load-module":
		f.loads++
		f.sinks = append(f.sinks, sink{99, SinkName, json.RawMessage("77")})
		return []byte("77"), nil
	case "set-default-sink":
		f.def = a[1]
		return nil, nil
	case "move-sink-input":
		id, _ := strconv.Atoi(a[1])
		for i, s := range f.streams {
			if s.Index == id {
				for _, target := range f.sinks {
					if target.Name == a[2] {
						f.streams[i].Sink = target.Index
						return nil, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("missing stream")
	case "unload-module":
		f.sinks = f.sinks[:2]
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected %v", a)
}
func testManager(t *testing.T) (*Manager, *fakePulse) {
	t.Helper()
	f := &fakePulse{def: "speaker", sinks: []sink{{Index: 1, Name: "speaker"}, {Index: 2, Name: "headphones"}}, streams: []stream{{Index: 10, Sink: 1, Properties: map[string]string{"object.serial": "a"}}, {Index: 11, Sink: 2, Properties: map[string]string{"object.serial": "b"}}}}
	return &Manager{run: f.command, path: filepath.Join(t.TempDir(), "restore.json")}, f
}
func TestLeaseAndRestore(t *testing.T) {
	m, f := testManager(t)
	if err := m.acquire(); err != nil {
		t.Fatal(err)
	}
	if f.def != SinkName || f.streams[0].Sink != 99 || f.streams[1].Sink != 99 {
		t.Fatal("not routed", f)
	}
	if err := m.acquire(); err != nil {
		t.Fatal(err)
	}
	m.release()
	if m.refs != 1 || f.def != SinkName {
		t.Fatal("one client disconnected too early")
	}
	m.release()
	// A media restart inside the grace period reuses the sink.
	if err := m.acquire(); err != nil {
		t.Fatal(err)
	}
	if f.loads != 1 {
		t.Fatal("media restart rebuilt the sink")
	}
	// Newly opened apps and stale stream IDs should restore to their own device.
	f.streams = append(f.streams, stream{Index: 12, Sink: 99, Properties: map[string]string{"object.serial": "c"}})
	if err := m.route(); err != nil {
		t.Fatal(err)
	}
	if err := m.restore(); err != nil {
		t.Fatal(err)
	}
	if f.def != "speaker" || f.streams[0].Sink != 1 || f.streams[1].Sink != 2 || f.streams[2].Sink != 1 {
		t.Fatalf("bad restore: %+v", f)
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatal("journal not removed")
	}
}
func TestCrashRecoveryAndManualOverride(t *testing.T) {
	m, f := testManager(t)
	if err := m.acquire(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	recovered := &Manager{run: f.command, path: m.path, state: &journal{}}
	if err = json.Unmarshal(b, recovered.state); err != nil {
		t.Fatal(err)
	}
	// User explicitly moved one stream and selected another default.
	f.def = "headphones"
	f.streams[0].Sink = 2
	if err = recovered.restore(); err != nil {
		t.Fatal(err)
	}
	if f.def != "headphones" || f.streams[0].Sink != 2 || f.streams[1].Sink != 2 {
		t.Fatal("overrode user's settings", f)
	}
}

func TestLastLeaseRestoresAfterGrace(t *testing.T) {
	m, f := testManager(t)
	if err := m.acquire(); err != nil {
		t.Fatal(err)
	}
	m.release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		restored := m.state == nil
		def := f.def
		m.mu.Unlock()
		if restored {
			if def != "speaker" {
				t.Fatal(def)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("last lease did not restore playback")
}
