package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

const busName = "org.freedesktop.portal.Desktop"
const screenCast = "org.freedesktop.portal.ScreenCast"
const desktopPath = dbus.ObjectPath("/org/freedesktop/portal/desktop")

type Stream struct {
	Node       uint32
	Properties map[string]dbus.Variant
}
type Session struct {
	conn         *dbus.Conn
	path         dbus.ObjectPath
	Remote       *os.File
	Node         uint32
	Streams      []Stream
	RestoreToken string
}

func (s *Session) Close() {
	if s.Remote != nil {
		s.Remote.Close()
	}
	if s.conn != nil {
		if s.path.IsValid() {
			s.conn.Object(busName, s.path).Call("org.freedesktop.portal.Session.Close", 0)
		}
		s.conn.Close()
	}
}
func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "mirage_" + hex.EncodeToString(b[:])
}

func request(ctx context.Context, c *dbus.Conn, method string, args ...any) (map[string]dbus.Variant, error) {
	opts := args[len(args)-1].(map[string]dbus.Variant)
	handle := token()
	opts["handle_token"] = dbus.MakeVariant(handle)
	sender := strings.ReplaceAll(strings.TrimPrefix(c.Names()[0], ":"), ".", "_")
	expected := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + sender + "/" + handle)
	signals := make(chan *dbus.Signal, 8)
	c.Signal(signals)
	defer c.RemoveSignal(signals)
	rules := []dbus.MatchOption{dbus.WithMatchObjectPath(expected), dbus.WithMatchInterface("org.freedesktop.portal.Request"), dbus.WithMatchMember("Response")}
	if err := c.AddMatchSignal(rules...); err != nil {
		return nil, err
	}
	defer c.RemoveMatchSignal(rules...)
	var path dbus.ObjectPath
	if err := c.Object(busName, desktopPath).CallWithContext(ctx, screenCast+"."+method, 0, args...).Store(&path); err != nil {
		return nil, err
	}
	if path != expected {
		return nil, fmt.Errorf("portal returned unexpected request path")
	}
	for {
		select {
		case <-ctx.Done():
			c.Object(busName, path).Call("org.freedesktop.portal.Request.Close", 0)
			return nil, ctx.Err()
		case sig := <-signals:
			if sig == nil {
				return nil, fmt.Errorf("portal disconnected")
			}
			if sig.Path != path {
				continue
			}
			var code uint32
			var result map[string]dbus.Variant
			if err := dbus.Store(sig.Body, &code, &result); err != nil {
				return nil, err
			}
			if code != 0 {
				return nil, fmt.Errorf("screen sharing request declined or failed (%d)", code)
			}
			return result, nil
		}
	}
}

// Open prompts the local user to select a screen through the desktop portal.
// The session is kept alive until Close, which revokes the PipeWire stream.
func Open(ctx context.Context) (*Session, error) { return OpenMultiple(ctx, false) }
func OpenMultiple(ctx context.Context, multiple bool) (*Session, error) {
	return OpenWithCursor(ctx, multiple, true)
}
func OpenWithCursor(ctx context.Context, multiple, embedded bool) (*Session, error) {
	return OpenWithRestore(ctx, multiple, embedded, "")
}
func OpenWithRestore(ctx context.Context, multiple, embedded bool, restoreToken string) (out *Session, err error) {
	c, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	s := &Session{conn: c}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	result, err := request(ctx, c, "CreateSession", map[string]dbus.Variant{"session_handle_token": dbus.MakeVariant(token())})
	if err != nil {
		return nil, err
	}
	v, ok := result["session_handle"]
	if !ok {
		return nil, fmt.Errorf("portal omitted session")
	}
	value, ok := v.Value().(string)
	if !ok {
		return nil, fmt.Errorf("invalid portal session")
	}
	s.path = dbus.ObjectPath(value)
	cursorMode := uint32(1)
	if embedded {
		cursorMode = 2
	}
	options := map[string]dbus.Variant{"types": dbus.MakeVariant(uint32(1)), "multiple": dbus.MakeVariant(multiple), "cursor_mode": dbus.MakeVariant(cursorMode), "persist_mode": dbus.MakeVariant(uint32(2))}
	if restoreToken != "" {
		options["restore_token"] = dbus.MakeVariant(restoreToken)
	}
	_, err = request(ctx, c, "SelectSources", s.path, options)
	if err != nil {
		return nil, err
	}
	result, err = request(ctx, c, "Start", s.path, "", map[string]dbus.Variant{})
	if err != nil {
		return nil, err
	}
	var streams []Stream
	if err = dbus.Store([]any{result["streams"].Value()}, &streams); err != nil {
		return nil, err
	}
	if len(streams) < 1 || len(streams) > 16 || (!multiple && len(streams) != 1) || streams[0].Node == 0 {
		return nil, fmt.Errorf("expected one portal stream")
	}
	for _, stream := range streams {
		if source, ok := stream.Properties["source_type"]; ok && source.Value() != uint32(1) {
			return nil, fmt.Errorf("select an entire monitor in the sharing dialog, not a window (source_type=%v)", source.Value())
		}
	}
	if v, ok := result["restore_token"]; ok {
		s.RestoreToken, _ = v.Value().(string)
	}
	s.Node = streams[0].Node
	s.Streams = streams
	var fd dbus.UnixFD
	if err = c.Object(busName, desktopPath).CallWithContext(ctx, screenCast+".OpenPipeWireRemote", 0, s.path, map[string]dbus.Variant{}).Store(&fd); err != nil {
		return nil, err
	}
	s.Remote = os.NewFile(uintptr(fd), "portal-pipewire")
	return s, nil
}
