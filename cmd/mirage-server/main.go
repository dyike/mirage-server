//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dyike/mirage-server/internal/server"
	"github.com/dyike/mirage-server/internal/server/auth"
	"github.com/dyike/mirage-server/internal/server/capture"
	"github.com/dyike/mirage-server/internal/server/input"
	"github.com/dyike/mirage-server/internal/server/portal"
)

func main() {
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for run(parent) {
	}
}

// Rebuild all capture/input state from one immutable output snapshot. A topology
// change closes the old sessions; clients reconnect to the refreshed catalog.
func run(parent context.Context) bool {
	flags := flag.NewFlagSet("mirage-server", flag.ExitOnError)
	ctx, stop := context.WithCancel(parent)
	defer stop()
	var refresh atomic.Bool

	ard := flags.Bool("ard", false, "enable the existing Mirage Mac/ARD client and SRTP video")
	ardPort := flags.Int("ard-udp-port", 5902, "first ARD video UDP port (one per client); 0 selects ephemeral ports")
	output := flags.String("output", "", "Wayland output name, or auto to discover selectable outputs")
	test := flags.Bool("test-source", false, "use synthetic source instead of desktop")
	codec := flags.String("codec", "hevc", "hevc or h264")
	width := flags.Int("width", 1920, "encoded width")
	height := flags.Int("height", 1080, "encoded height")
	fps := flags.Int("fps", 30, "maximum frames per second")
	bitrate := flags.Int("bitrate", 8000, "maximum video bitrate in kbit/s (connections start at up to 2000)")
	frames := flags.Int("probe-frames", 60, "frames to encode for capture verification")
	listen := flags.String("listen", "", "RFB/VNC listener; empty runs capture verification")
	cert := flags.String("cert", "", "TLS certificate PEM")
	key := flags.String("key", "", "TLS private key PEM")
	srpPath := flags.String("srp-file", "", "Apple SRP verifier file; require mutual SRP authentication, no legacy fallback")
	enroll := flags.Bool("enroll-srp", false, "register an SRP account using a local terminal, then exit")
	pamService := flags.String("pam-service", "", "system PAM service for account authentication")
	username := flags.String("username", "", "VeNCrypt account; empty uses classic VNC password authentication")
	passwordPath := flags.String("password-file", "", "password file: 1–8 bytes for VNC, up to 256 with username")
	restorePath := flags.String("restore-token-file", "", "persist Portal screen selection for service restarts")
	cursorMode := flags.String("cursor-mode", "auto", "auto, local or embedded cursor")
	multiple := flags.Bool("multiple", false, "select multiple screens in the desktop portal")
	resizeOutputMode := flags.Bool("resize-output-mode", false, "apply client desktop resize to compositor output modes (requires allow-resize)")
	dynamicResolution := flags.Bool("dynamic-resolution", false, "dedicated single-output desktop follows Mirage VNC client window size")
	allowResize := flags.Bool("allow-resize", false, "allow RFB clients to resize/rearrange the captured framebuffer")
	inputOutput := flags.String("input-output", "", "captured Wayland output name; empty disables remote input")
	maxClients := flags.Int("max-clients", 4, "maximum authenticated clients")
	noClipboard := flags.Bool("no-clipboard", false, "disable desktop clipboard sharing")
	noAudio := flags.Bool("no-audio", false, "disable desktop output audio")
	inputSeat := flags.String("input-seat", "", "Wayland seat name; empty selects the first seat")
	keyboardRules := flags.String("keyboard-rules", "", "XKB rules")
	keyboardModel := flags.String("keyboard-model", "", "XKB model")
	keyboardOptions := flags.String("keyboard-options", "", "XKB options, e.g. grp:alt_shift_toggle")
	keyboardLayout := flags.String("keyboard-layout", "us", "XKB keyboard layout(s)")
	keyboardVariant := flags.String("keyboard-variant", "", "XKB keyboard variant(s)")
	inputTransform := flags.Uint("input-transform", 0, "captured image rotation/flip (Wayland 0..7)")
	flags.Parse(os.Args[1:])
	if *dynamicResolution {
		if *output == "" || *output == "auto" || *multiple || *test || *ard {
			log.Fatal("dynamic-resolution requires one named output and standard VNC (no ard)")
		}
		*allowResize, *resizeOutputMode, *maxClients = true, true, 1
	}
	if *enroll {
		if *srpPath == "" {
			*srpPath = defaultSRPPath()
		}
		if err := enrollSRP(*srpPath, *username); err != nil {
			log.Fatal(err)
		}
		return false
	}
	if *srpPath != "" && !*ard {
		log.Fatal("srp-file requires ard")
	}
	var nativeNames []string
	var nativeOutputs []input.OutputInfo
	if *output == "auto" {
		if *test || *multiple || *resizeOutputMode {
			log.Fatal("auto output cannot combine with test-source, multiple or resize-output-mode")
		}
		if *inputOutput != "" {
			log.Fatal("auto output manages input-output automatically")
		}
		var err error
		nativeOutputs, err = waitForOutputs(ctx, input.DiscoverOutputs)
		for _, o := range nativeOutputs {
			nativeNames = append(nativeNames, o.Name)
		}
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			log.Fatal(err)
		}
		if len(nativeNames) == 0 || len(nativeNames) > 16 {
			log.Fatal("auto output requires 1..16 named outputs")
		}
		*output = nativeNames[0]
		// Embedded cursors follow the selected capture without a separate cursor source.
		if *cursorMode == "local" {
			log.Fatal("auto output currently requires embedded cursors")
		}
		*cursorMode = "embedded"
		log.Printf("Selectable outputs: %s", strings.Join(nativeNames, ", "))
		for _, o := range nativeOutputs {
			log.Printf("Output %s: logical=%v pixels=%v", o.Name, o.Bounds, o.Pixels)
		}
	}
	if *ardPort < 0 || *ardPort > 65471 {
		log.Fatal("ard-udp-port must be 0..65471")
	}
	if *output != "" {
		if *test || *multiple || strings.Contains(*output, ",") {
			log.Fatal("output selects one native screen and cannot be combined with test-source or multiple")
		}
		if *inputOutput != "" && *inputOutput != *output {
			log.Fatal("input-output must match output for native capture")
		}
		*inputOutput = *output
	}

	if *resizeOutputMode && !*allowResize {
		log.Fatal("resize-output-mode requires allow-resize")
	}
	if *codec != "hevc" && *codec != "h264" {
		log.Fatal("codec must be hevc or h264")
	}
	if *inputTransform > 7 {
		log.Fatal("input-transform must be 0..7")
	}
	if *frames < 1 {
		log.Fatal("probe-frames must be positive")
	}
	if *listen == "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	localCursor := false
	if *cursorMode != "auto" && *cursorMode != "local" && *cursorMode != "embedded" {
		log.Fatal("invalid cursor-mode")
	}
	if !*test && *cursorMode != "embedded" && *inputOutput != "" {
		probe, err := input.OpenCursors(strings.Split(*inputOutput, ","), *inputSeat)
		if err == nil {
			probe.Close()
			localCursor = true
		} else if *cursorMode == "local" {
			log.Fatal(err)
		} else {
			log.Printf("local cursor unavailable: %v; embedding cursor", err)
		}
	} else if !*test && *cursorMode == "local" {
		log.Fatal("local cursor requires output names via input-output")
	}
	var streams []portal.Stream
	var screens []server.DesktopScreen
	options := capture.Options{Test: *test, Codec: *codec, Width: *width, Height: *height, FPS: *fps, Bitrate: *bitrate}
	if *output != "" {
		screen, err := input.OpenScreen(ctx, *output, !localCursor)
		if err != nil {
			if len(nativeOutputs) > 0 && *listen != "" {
				log.Printf("Native output not ready; rediscovering: %v", err)
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				select {
				case <-parent.Done():
					return false
				case <-timer.C:
					return true
				}
			}
			log.Fatal(err)
		}
		log.Printf("Native output %s: %dx%d (no Portal)", *output, screen.Width, screen.Height)
		screen.Close()
		options.Output = *output
		options.EmbeddedCursor = !localCursor
		screens, err = portalLayout([]portal.Stream{{}}, *width, *height)
		if err != nil {
			log.Fatal(err)
		}
	} else if !*test {
		log.Print("Select a screen in the desktop sharing dialog")
		var restoreToken string
		if *restorePath != "" {
			b, err := os.ReadFile(*restorePath)
			if err == nil {
				restoreToken = string(b)
			} else if !os.IsNotExist(err) {
				log.Fatal(err)
			}
		}
		session, err := portal.OpenWithRestore(ctx, *multiple, !localCursor, restoreToken)
		if err != nil {
			log.Fatal(err)
		}
		defer session.Close()
		for i, stream := range session.Streams {
			log.Printf("Portal stream %d: size=%v source_type=%v", i+1, stream.Properties["size"].Value(), stream.Properties["source_type"].Value())
		}
		if *restorePath != "" && session.RestoreToken != "" {
			if err := os.MkdirAll(filepath.Dir(*restorePath), 0700); err != nil {
				log.Fatal(err)
			}
			tmp, err := os.CreateTemp(filepath.Dir(*restorePath), ".portal-token-*")
			if err != nil {
				log.Fatal(err)
			}
			_, err = tmp.WriteString(session.RestoreToken)
			closeErr := tmp.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Rename(tmp.Name(), *restorePath)
			}
			if err != nil {
				os.Remove(tmp.Name())
				log.Fatal(err)
			}
		}
		options.FD = int(session.Remote.Fd())
		options.Node = session.Node
		streams = session.Streams
		screens, err = portalLayout(streams, *width, *height)
		if err != nil {
			log.Fatal(err)
		}
		if len(streams) > 1 {
			for _, screen := range screens {
				st := streams[screen.ID-1]
				options.Layers = append(options.Layers, capture.Layer{Node: st.Node, FD: options.FD, X: screen.Bounds.Min.X, Y: screen.Bounds.Min.Y, Width: screen.Bounds.Dx(), Height: screen.Bounds.Dy()})
			}
		}
	}
	if *listen != "" {
		pair, err := tls.LoadX509KeyPair(*cert, *key)
		if err != nil {
			log.Fatal(err)
		}
		var password []byte
		var authenticate func(context.Context, string, string) error
		if *srpPath != "" {
			log.Printf("Apple SRP-only authentication enabled; credential file=%s (loaded per connection)", *srpPath)
		} else if *pamService != "" {
			authenticate, err = auth.PAM(*pamService)
		} else {
			password, err = os.ReadFile(*passwordPath)
		}
		if err != nil {
			log.Fatal(err)
		}
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		srv := server.RFBServer{
			ARD: *ard, ARDUDPPort: *ardPort,
			TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Password: strings.TrimRight(string(password), "\r\n"), Username: *username,
			Width: *width, Height: *height, PreferHEVC: *codec == "hevc", VideoBitrate: *bitrate, VideoFPS: *fps,
			OnError: func(err error) { log.Printf("session ended: %v", err) },
			Source: func(ctx context.Context, codec string, send func([]byte) error) error {
				selected := options
				selected.Codec = codec
				log.Printf("RFB negotiated codec: %s", codec)
				return capture.Run(ctx, selected, func(f capture.Frame) error { return send(f.Data) })
			},
		}
		clear(password)

		srv.Authenticate = authenticate
		if *srpPath != "" {
			srv.SRPCredential = func() (*server.SRPCredential, error) { return server.LoadSRPCredential(*srpPath) }
		}
		srv.MaxClients = *maxClients
		srv.RefreshInputOnResize = *resizeOutputMode
		srv.DynamicResolution = *dynamicResolution
		srv.Screens = screens
		if len(nativeNames) > 0 {
			srv.DisplayNames = make(map[uint32]string)
			for i := range nativeNames {
				srv.DisplayNames[uint32(i+1)] = nativeNames[i]
				srv.AvailableScreens = append(srv.AvailableScreens, server.DesktopScreen{ID: uint32(i + 1), Bounds: nativeOutputs[i].Bounds})
			}
		}
		srv.DesktopSource = func(ctx context.Context, request server.CaptureRequest, send func([]byte) error) error {
			selected := options
			selected.Codec = request.Codec

			selected.BindBitrate = request.BindBitrate
			selected.BindKeyframe = request.BindKeyframe
			selected.RepeatIdle = request.RepeatIdle
			selected.MinQP = request.MinQP
			if request.Bitrate > 0 {
				selected.Bitrate = request.Bitrate
			}
			if request.FPS > 0 {
				selected.FPS = request.FPS
			}
			if selected.Output != "" && request.BindFPS != nil {
				var fps atomic.Int32
				fps.Store(int32(selected.FPS))
				selected.FPSNow = func() int { return int(fps.Load()) }
				request.BindFPS(func(value int) { fps.Store(int32(max(1, min(value, selected.FPS)))) })
				defer request.BindFPS(nil)
			}
			selected.Width, selected.Height = request.Desktop.Size.X, request.Desktop.Size.Y
			selected.Layers = nil
			if len(nativeNames) > 0 {
				selected.Output = ""
				for _, screen := range request.Desktop.Screens {
					if screen.ID < 1 || int(screen.ID) > len(nativeNames) {
						return fmt.Errorf("unknown native output")
					}
					name := nativeNames[screen.ID-1]
					if len(request.Desktop.Screens) == 1 {
						selected.Output = name
					} else {
						selected.Layers = append(selected.Layers, capture.Layer{Output: name, X: screen.Bounds.Min.X, Y: screen.Bounds.Min.Y, Width: screen.Bounds.Dx(), Height: screen.Bounds.Dy()})
					}
				}
			}
			if len(streams) > 1 {
				for _, screen := range request.Desktop.Screens {
					if screen.ID < 1 || int(screen.ID) > len(streams) {
						return fmt.Errorf("unknown portal stream")
					}
					st := streams[screen.ID-1]
					selected.Layers = append(selected.Layers, capture.Layer{Node: st.Node, FD: options.FD, X: screen.Bounds.Min.X, Y: screen.Bounds.Min.Y, Width: screen.Bounds.Dx(), Height: screen.Bounds.Dy()})
				}
			}
			log.Printf("RFB codec=%s dimensions=%dx%d screens=%d bitrate=%d fps=%d", request.Codec, selected.Width, selected.Height, len(request.Desktop.Screens), selected.Bitrate, selected.FPS)
			return capture.Run(ctx, selected, func(f capture.Frame) error { return send(f.Data) })
		}
		if *allowResize {
			srv.SetDesktop = func(ctx context.Context, d server.DesktopLayout) error {
				if !*resizeOutputMode {
					return nil
				}
				names := strings.Split(*inputOutput, ",")
				var changes []input.OutputMode
				for _, screen := range d.Screens {
					if screen.ID < 1 || int(screen.ID) > len(names) || names[screen.ID-1] == "" {
						return fmt.Errorf("output-mode resize requires named outputs")
					}
					changes = append(changes, input.OutputMode{Name: names[screen.ID-1], Width: screen.Bounds.Dx(), Height: screen.Bounds.Dy(), X: screen.Bounds.Min.X, Y: screen.Bounds.Min.Y})
				}
				if *dynamicResolution {
					outputs, err := input.DiscoverOutputs()
					if err != nil {
						return err
					}
					for i := range changes {
						found := false
						for _, output := range outputs {
							if output.Name == changes[i].Name {
								changes[i].X, changes[i].Y = output.Bounds.Min.X, output.Bounds.Min.Y
								found = true
							}
						}
						if !found {
							return fmt.Errorf("dynamic output disappeared")
						}
					}
				}
				if *dynamicResolution {
					if len(changes) != 1 {
						return fmt.Errorf("dynamic resolution requires a single output")
					}
					return configureDynamicOutput(ctx, changes[0])
				}
				return input.ConfigureOutputs(ctx, changes)
			}
		}
		srv.EmbeddedCursor = !*test && !localCursor
		if localCursor {
			srv.OpenCursor = func() (server.Cursor, error) { return input.OpenCursors(strings.Split(*inputOutput, ","), *inputSeat) }
		}
		if !*noAudio {
			if err := capture.AudioELDAvailable(); err != nil {
				log.Printf("ARD audio unavailable: %v", err)
			} else {
				srv.ARDAudio = func(ctx context.Context, send func([]byte) error) error { return capture.AudioELD(ctx, *test, send) }
			}
			srv.Audio = func(ctx context.Context, f server.AudioFormat, send func([]byte) error) error {
				return capture.Audio(ctx, f.Format, f.Channels, f.Rate, *test, send)
			}
		}
		if !*noClipboard && !*test {
			srv.OpenClipboard = func() (server.Clipboard, error) { return input.OpenClipboard(*inputSeat) }
		}
		if *inputOutput != "" {
			srv.OpenInput = func() (server.InputSession, error) {
				names := strings.Split(*inputOutput, ",")
				if len(nativeNames) > 0 {
					names = nativeNames
				}
				if len(streams) > 0 && len(names) != len(streams) {
					return nil, fmt.Errorf("input-output requires one name per portal stream, in selection order")
				}
				return input.OpenOutputs(names, input.Options{Seat: *inputSeat, Rules: *keyboardRules, Model: *keyboardModel, XKBOptions: *keyboardOptions, Layout: *keyboardLayout, Variant: *keyboardVariant, Transform: uint32(*inputTransform)})
			}
		}
		if len(nativeOutputs) > 0 {
			go func() {
				if watchOutputChanges(ctx, nativeOutputs, input.DiscoverOutputs) {
					log.Print("Output layout changed; refreshing capture/input and reconnecting clients")
					refresh.Store(true)
					stop()
				}
			}()
		}
		log.Printf("Mirage RFB listener: %s", listener.Addr())
		if err := srv.Serve(ctx, listener); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return refresh.Load() && parent.Err() == nil
	}
	count, bytes := 0, 0
	done := errors.New("probe complete")
	start := time.Now()
	err := capture.Run(ctx, options, func(f capture.Frame) error {
		count++
		bytes += len(f.Data)
		if count >= *frames {
			return done
		}
		return nil
	})
	if !errors.Is(err, done) {
		log.Fatal(err)
	}
	fmt.Printf("codec=%s frames=%d bytes=%d dimensions=%dx%d elapsed=%s hardware_encoder=VA-API\n", *codec, count, bytes, *width, *height, time.Since(start))
	return false
}
