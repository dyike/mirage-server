// Package audioroute temporarily routes desktop playback to a silent sink.
package audioroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const SinkName = "mirage_remote_audio"

type sink struct {
	Index int
	Name  string
	Owner json.RawMessage `json:"owner_module"`
}
type stream struct {
	Index      int
	Sink       int
	Properties map[string]string
}
type savedStream struct{ Sink, Serial string }
type journal struct {
	Default string
	Streams map[int]savedStream
}
type command func(...string) ([]byte, error)

func pactl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "pactl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("pactl %s: %w: %s", args[0], err, b)
	}
	return b, nil
}

type Manager struct {
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	run       command
	path      string
	lock      *os.File
	state     *journal
	refs      int
	timer     *time.Timer
	done      chan struct{}
	exited    chan struct{}
}

func Open() (*Manager, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return nil, errors.New("audio-exclusive requires XDG_RUNTIME_DIR")
	}
	dir = filepath.Join(dir, "mirage-audio")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another Mirage process owns exclusive audio")
	}
	m := &Manager{run: pactl, path: filepath.Join(dir, "restore.json"), lock: f, done: make(chan struct{}), exited: make(chan struct{})}
	if b, e := os.ReadFile(m.path); e == nil {
		m.state = &journal{}
		if err = json.Unmarshal(b, m.state); err != nil {
			f.Close()
			return nil, err
		}
		if err = m.restore(); err != nil {
			f.Close()
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		f.Close()
		return nil, e
	}
	go m.watch()
	return m, nil
}
func (m *Manager) save() error {
	b, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
func (m *Manager) list(kind string, v any) error {
	b, err := m.run("-f", "json", "list", kind)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func (m *Manager) acquire() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.state == nil {
		b, err := m.run("get-default-sink")
		if err != nil {
			return err
		}
		var sinks []sink
		if err = m.list("sinks", &sinks); err != nil {
			return err
		}
		for _, s := range sinks {
			if s.Name == SinkName {
				return errors.New("exclusive sink already exists without recovery state")
			}
		}
		m.state = &journal{Default: strings.TrimSpace(string(b)), Streams: map[int]savedStream{}}
		if err = m.save(); err != nil {
			m.state = nil
			return err
		}
		// Journal existing stream destinations BEFORE changing the default device.
		if err = m.remember(sinks); err != nil {
			_ = m.restore()
			return err
		}
		if _, err = m.run("load-module", "module-null-sink", "sink_name="+SinkName, "rate=48000", "channels=2", "sink_properties=device.description=Mirage_Remote_Audio"); err != nil {
			_ = m.restore()
			return err
		}
		if _, err = m.run("set-default-sink", SinkName); err != nil {
			_ = m.restore()
			return err
		}
	}
	if err := m.route(); err != nil {
		if m.refs == 0 {
			_ = m.restore()
		}
		return err
	}
	m.refs++
	return nil
}
func (m *Manager) remember(sinks []sink) error {
	var streams []stream
	if err := m.list("sink-inputs", &streams); err != nil {
		return err
	}
	names := map[int]string{}
	for _, s := range sinks {
		names[s.Index] = s.Name
	}
	for _, s := range streams {
		if saved, ok := m.state.Streams[s.Index]; ok && saved.Serial == s.Properties["object.serial"] {
			continue
		}
		name := names[s.Sink]
		if name == "" || name == SinkName {
			name = m.state.Default
		}
		m.state.Streams[s.Index] = savedStream{name, s.Properties["object.serial"]}
	}
	return m.save()
}
func (m *Manager) route() error {
	var sinks []sink
	if err := m.list("sinks", &sinks); err != nil {
		return err
	}
	if err := m.remember(sinks); err != nil {
		return err
	}
	var streams []stream
	if err := m.list("sink-inputs", &streams); err != nil {
		return err
	}
	id := -1
	for _, s := range sinks {
		if s.Name == SinkName {
			id = s.Index
		}
	}
	if id < 0 {
		return errors.New("exclusive audio sink disappeared")
	}
	for _, s := range streams {
		if s.Sink != id {
			if _, err := m.run("move-sink-input", strconv.Itoa(s.Index), SinkName); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *Manager) restore() error {
	if m.state == nil {
		return nil
	}
	var sinks []sink
	if err := m.list("sinks", &sinks); err != nil {
		return err
	}
	names := map[string]bool{}
	var owned *sink
	for i, s := range sinks {
		names[s.Name] = true
		if s.Name == SinkName {
			owned = &sinks[i]
		}
	}
	b, err := m.run("get-default-sink")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(b))
	fallback := m.state.Default
	if !names[fallback] {
		fallback = ""
		for _, s := range sinks {
			if s.Name != SinkName {
				fallback = s.Name
				break
			}
		}
	}
	if fallback == "" {
		return errors.New("no physical output available to restore audio")
	}
	var streams []stream
	if owned != nil {
		if err = m.list("sink-inputs", &streams); err != nil {
			return err
		}
	}
	if current == SinkName {
		if _, err = m.run("set-default-sink", fallback); err != nil {
			return err
		}
	}
	if owned != nil {
		for _, s := range streams {
			if s.Sink == owned.Index {
				target := fallback
				if saved, ok := m.state.Streams[s.Index]; ok && saved.Serial == s.Properties["object.serial"] && names[saved.Sink] {
					target = saved.Sink
				}
				if _, err = m.run("move-sink-input", strconv.Itoa(s.Index), target); err != nil {
					return err
				}
			}
		}
		module := strings.Trim(string(owned.Owner), "\"")
		if _, err = strconv.ParseUint(module, 10, 32); err != nil {
			return fmt.Errorf("invalid sink owner: %s", module)
		}
		if _, err = m.run("unload-module", module); err != nil {
			return err
		}
	}
	if err = os.Remove(m.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.state = nil
	log.Print("exclusive audio: restored local playback")
	return nil
}
func (m *Manager) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs--
	if m.refs == 0 {
		m.timer = time.AfterFunc(time.Second, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.timer = nil
			if m.refs == 0 {
				if err := m.restore(); err != nil {
					log.Printf("exclusive audio restore: %v", err)
				}
			}
		})
	}
}
func (m *Manager) watch() {
	defer close(m.exited)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-tick.C:
			m.mu.Lock()
			if m.state != nil {
				var err error
				if m.refs > 0 {
					err = m.route()
				} else if m.timer == nil {
					err = m.restore()
				}
				if err != nil {
					log.Printf("exclusive audio route: %v", err)
				}
			}
			m.mu.Unlock()
		}
	}
}

// Capture keeps the routing lease across concurrent streams and brief SRTP restarts.
func (m *Manager) Capture(ctx context.Context, capture func(context.Context, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.acquire(); err != nil {
		return err
	}
	defer m.release()
	return capture(ctx, SinkName)
}
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		<-m.exited
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.timer != nil {
			m.timer.Stop()
		}
		m.closeErr = m.restore()
		m.lock.Close()
	})
	return m.closeErr
}
