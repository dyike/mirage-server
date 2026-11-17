# Mirage Server

[English](README.md) | [简体中文](README.zh-CN.md)

A Go remote desktop server for Linux/Wayland, with VNC compatibility and Mirage's Apple Remote Desktop (ARD) media path for hardware-encoded HEVC/H.264 over SRTP.

Extracted from the `mirage` repository at commit `8ed35bd`. The client lives in the sibling [`mirage`](https://github.com/dyike/mirage) repository.

The internal protocol and media packages are independent snapshots of Mirage's own source, including the client implementations used by interoperability tests. Synchronize changes between the repositories when needed. Dependencies are managed through Go Modules; do not create a `vendor/` directory.

## ARD high-performance mode

Enable `-ard -ard-udp-port 5902` to accept both Apple `RFB 003.889` and standard `RFB 003.008`. In an existing Mirage client, create a **Mac** connection, select **Auto** quality, and enter the account credentials and port `5902`. This uses the client's existing HEVC/H.264 SRTP receiver and hardware decoder. **VNC** connections continue to use VeNCrypt/TCP.

- The ARD path supports security type 30, Apple's encrypted record layer, MediaStream negotiation, and AES-256 SRTP/SRTCP. Use `-pam-service` for account authentication, or `-username` and `-password-file` for a fixed account. ARD username and password fields are each limited to 63 bytes. Standard VNC TLS certificate validation remains unchanged: do not convert an existing VNC profile with a saved certificate fingerprint to a Mac profile to bypass validation.
- The first media session uses `5902/UDP`. Subsequent sessions use consecutive ports, normally `5902–5905/UDP` for the default limit of four clients. Ports are bound during media negotiation. Use `-ard-udp-port 0` for ephemeral ports in tests. Remote access requires the corresponding UDP firewall rules and same-port forwarding; forwarding TCP alone is insufficient.
- Control messages, keyboard/mouse input, and text clipboard data retain reliable transport. Video travels separately over SRTP. Authenticated RTCP loss and queue feedback drive bitrate reduction. Keyframe requests trigger the current encoder without rebuilding capture. A bounded send queue sends complete frames without truncating them under normal rate limiting; a single lost packet does not immediately cause a large bitrate drop. Native DMA-BUF capture reuses GPU frames while the desktop is idle to keep media flowing, without mapping CPU pixels. Bitrate renegotiation on the same port accepts new SRTP keys and continues video. If UDP is unreachable, the existing client falls back to TCP pictures after its first-frame timeout.
- Tests have verified H.264/HEVC hardware decoding, zero-copy DMA-BUF operation, packet-loss recovery, and UDP-blackhole fallback with Mirage's Mac client. This implements the ARD subset used by Mirage, not every feature of Apple's native Screen Sharing.app.
- ARD system audio captures the default output monitor through PipeWire, not the microphone. The system `libfdk-aac.so.2` encodes 48 kHz stereo AAC-ELD at 128 kbit/s over a separate SRTP audio channel. If the library is missing, only ARD audio is disabled and the reason is logged. Use `-no-audio` to disable audio explicitly. Audio ports follow the video pool: `5906–5909/UDP` for four clients. Allow these through the VPN/firewall as well. Audio receives new keys and restarts when video renegotiates its bitrate. Existing Mirage clients need no update for this audio path; standard VNC audio is unaffected.
- Adaptive bitrate and renegotiation preserve the configured output resolution. Low bitrate or large frames do not automatically shrink the desktop. Large frames are sent completely at the negotiated rate, which can increase latency on slow links. Input coordinates continue to map to the native desktop. See [Display capture and selection](#display-capture-and-selection) for output switching in updated clients.

Hardware end-to-end checks require local VA-API, GStreamer, and EGL:

```bash
MIRAGE_TEST_RFB_GPU=1 go test -race -tags purego ./internal/server -run '^TestARDHardwareInterop$' -v
# Capture a real 4K output and drop one in every 200 video packets.
# Only counters and timings are logged; desktop images are not recorded.
MIRAGE_TEST_ARD_SOAK=1 MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE MIRAGE_TEST_DROP_EVERY=200 go test -race -tags purego ./internal/server -run '^TestARDLocalSoak$' -v
```

## Remote-only audio

`-audio-exclusive` temporarily routes PipeWire/PulseAudio playback streams to a silent virtual sink while remote audio is active. Mirage captures that sink explicitly, so the physical speakers do not play the redirected streams. For ARD, routing starts only after authenticated audio receiver feedback. Video-only connections do not take over sound.

The original default output and each existing stream's destination are recorded. New playback streams are routed while exclusive audio is active. The last audio stream releasing its lease restores local playback after a one-second grace period; brief SRTP renegotiations share the same lease state. Loss of authenticated ARD audio feedback for more than three seconds stops capture and releases the lease. Reconnect if the audio transport has timed out.

This optional mode requires `pactl` and PipeWire's PulseAudio compatibility server. It changes routing, not physical-device volume or mute settings. It affects the current user's desktop audio, not only the application visible remotely; direct ALSA applications that bypass PipeWire/PulseAudio are outside its scope. More than one receiver can share audio, but only one exclusive-audio server process may manage routing per user.

Recovery state is stored privately under `$XDG_RUNTIME_DIR/mirage-audio/`. Normal shutdown restores audio; the next exclusive-audio startup recovers stale state. For systemd crash recovery, add the following to the service's `[Service]` section as well as enabling `-audio-exclusive` on `ExecStart`:

```ini
ExecStopPost=%h/.local/bin/mirage-server -restore-audio
```

`mirage-server -restore-audio` can also be run manually after the exclusive-audio process exits. Recovery preserves an explicitly changed default or streams already moved away from the virtual sink. If the previous device is unavailable, it chooses another available output; if none exists, it retains recovery state rather than discarding it.

## Mirage HEVC extension

The server supports private encoding `0x4d485643` (MHVC), used only when the client explicitly advertises it. Ordinary VNC clients negotiate their existing encodings. The implementation uses VA-API HEVC Main, 8-bit, with B-frames disabled. This is not a standard RFB HEVC extension.

From the client repository, run `MIRAGE_VNC_HEVC=1 go run -tags purego ./cmd/mirage`, choose VNC and Auto quality, and connect to port `5902`. This documents the experimental opt-in used by the original client; check the client version's defaults when deploying.

## Development and quick start

The entry point is `cmd/mirage-server`; server code lives in `internal/server/`. On Linux, capture can use Wayland ext-image-copy-capture directly by output name, or obtain PipeWire streams through the desktop portal. GStreamer/VA-API is called through purego to encode HEVC/H.264. The server does not launch WayVNC or NeatVNC subprocesses.

Runtime requirements include PipeWire and GStreamer plugins for VA encoding, codec parsing, and appsink. Capture verification opens the system screen picker and counts encoded data without saving or transmitting it:

```sh
go run ./cmd/mirage-server -codec hevc -probe-frames 60
```

Add `-test-source` to use a synthetic image instead of the desktop.

To start an encrypted VNC server:

```sh
go run --tags purego ./cmd/mirage-server -listen 127.0.0.1:5910 \
  -cert server.crt -key server.key -password-file password.txt \
  -input-output MIRAGE -keyboard-layout us
```

`password.txt` contains a 1–8 byte VNC password; trailing line endings are removed. VNC challenge/response runs inside VeNCrypt X509Vnc TLS. Specifying `-username` selects X509Plain account authentication inside TLS, supporting passwords up to 256 bytes; the client must supply the same username. Existing client certificate verification and confirmation flows still apply. The server uses RFB 3.8, as described in [RFC 6143](https://www.rfc-editor.org/rfc/rfc6143.html), rather than the earlier experimental `MIRAGE/2` protocol.

Clients negotiate H.264, the enabled Mirage HEVC extension, ZRLE, Tight, or Raw. Clear quality prefers ZRLE. Tight/ZRLE maintain a persistent zlib stream within each connection. Raw supports 8/16/32-bit true-color formats in either byte order. Video format changes, size changes, or bitrate adjustments restart the applicable encoder path from an independent picture.

Connections start at up to 2 Mbit/s, with defaults of 8 Mbit/s maximum and 30 FPS. Video uses VBR and a small CPB. Drivers may still exceed the target on complex images, so the sender also limits actual encoded bytes, permits at most 50 ms of burst traffic, and does not discard interdependent encoded frames. TCP queue depth and actual write blocking trigger quick bitrate reduction followed by gradual recovery. `-bitrate` sets the maximum video bitrate. A bounded queue before encoding drops stale raw frames. Audio and protocol overhead are outside the video budget; 48 kHz stereo S16 audio alone is approximately 1.536 Mbit/s.

## Features

| Feature | Implementation |
| --- | --- |
| Authentication | RFB 3.8, VeNCrypt TLS, VNC password or account credentials; desktop control devices are created only after authentication |
| Keyboard and mouse | XKB layouts, chords, physical keycodes, key deduplication, LEDs, five mouse buttons, four-way scrolling, and input release on disconnect |
| Input extensions | QEMU Extended Key Event, QEMU LED State, and Extended Mouse Buttons, enabled only after negotiation |
| Clipboard | Bidirectional Wayland data-control text; classic Latin-1 and extended UTF-8 protocols, with size and decompression limits |
| Audio | PipeWire output audio; QEMU Audio negotiation, start/stop, and format changes; 48 kHz stereo S16LE by default; no microphone capture |
| Cursor | Native Wayland cursor capture, local cursor image and hotspot, scaling with encoded dimensions, and embedded-cursor fallback |
| Multiple displays | Portal multi-stream capture, VA-API composition, ExtendedDesktopSize layouts, input routing by screen ID, and cross-display dragging with one pointer |
| Resizing | Optional per-connection scaling/rearrangement and atomic wlr-output-management mode configuration, with failure status when rejected |
| Transport | Continuous updates, Fence replies, a shared message write lock, bounded buffering, slow-connection timeouts, and shared/exclusive connection semantics |

Go handles Wayland messages and file descriptors directly for input, clipboard, and cursor support. System libxkbcommon, PipeWire, and GStreamer/VA-API are called through purego; no C server subprocess is launched. Dependencies remain in the root `go.mod` and `go.sum`.

## Display capture and selection

### Named output

`-output MIRAGE` captures the entire named Wayland output directly, without a portal, picker, or restore token, and binds input to that output. A missing output is an error. This path requires `ext-image-copy-capture-v1` and `ext-output-image-capture-source-v1` support from the compositor. An explicit output name selects one screen.

H.264/HEVC uses GBM-allocated DMA-BUFs submitted to the compositor through `zwp_linux_dmabuf_v1` and imported by GStreamer/VA-API. There is no CPU mapping or pixel copy. A bounded six-frame pool reuses buffers after the encoder releases its final memory reference. Scaling and color conversion happen on the GPU. CPU mapping is prohibited in this path, and unsupported DMA imports fail explicitly rather than falling back. Current support is for linear XRGB/ARGB on the same DRM device and unrotated outputs; other modifiers or rotated outputs are rejected. Raw/Tight/ZRLE still use the CPU `wl_shm` path.

### Automatic discovery

`-output auto` discovers up to 16 named outputs at startup, sorts them by name, and captures the first by default. With `-ard`, Mirage's Remote Displays panel can switch outputs independently for each connection. Video and mouse mapping use the same screen ID without changing physical display modes. The service template uses this mode. Use `-output DP-1` or `-output MIRAGE` to pin a specific screen instead.

Automatic mode currently embeds the cursor and uses `-width/-height` as the encoding dimensions. Updated clients show output names and mark the default display. All Displays composes screens at their logical positions while preserving the aggregate desktop aspect ratio within the encoding limits.

Multi-display H.264/HEVC uses DMA-BUF capture, OpenGL scaling/composition, and a GPU blit to a linear GBM buffer for VA encoding. It performs no CPU pixel mapping, readback, or upload. Capture and composition buffers are reused after downstream references are released. Unsupported GPU/EGL configurations fail explicitly; there is no silent CPU-composition fallback. Raw encoding retains its CPU path.

Names and single-display restrictions are sent through a client-requested Mirage layout extension. Older clients show display numbers. Every 500 ms, automatic mode checks output positions and logical/pixel sizes. After two matching observations of a new layout, it rebuilds capture and input mapping. Scaling, resolution changes, or hotplug events briefly disconnect clients so they can reconnect, without a manual server restart. Disabled outputs or outputs without valid logical dimensions are excluded. If all outputs disappear, the server waits for one to return. Display-type identification is not implemented. All discovered outputs become selectable by authenticated clients.

### Portal capture and input options

Omitting `-output` uses the portal. `-input-output` must match the screen selected in the portal; omitting it disables keyboard/mouse input. For multiple screens, use `-multiple` and supply comma-separated output names in portal stream order, for example `-input-output DP-1,HDMI-A-1`. The portal backend must support multi-selection. Multi-screen input uses XDG logical geometry to account for different display scales. One virtual input device is shared across screens, so dragging does not re-press mouse buttons at screen boundaries.

- `-input-seat` selects a seat.
- `-keyboard-rules`, `-keyboard-model`, `-keyboard-layout`, `-keyboard-variant`, and `-keyboard-options` configure XKB.
- `-input-transform` selects the captured image's Wayland transform (`0..7`).
- `-no-clipboard` and `-no-audio` disable clipboard and desktop audio respectively.
- `-max-clients` defaults to 4. Authentication, encoding, input, and audio state are maintained separately for each connection.
- `-cursor-mode auto` selects a local cursor when an output name is provided and cursor capture is supported; otherwise the cursor is embedded. Explicit `local` and `embedded` modes are also available. Local mode requires standard Cursor encoding support, which Mirage provides.

`-allow-resize` accepts standard SetDesktopSize requests to change encoded dimensions, select authorized screens, or rearrange the connection's layout without changing physical display modes. Adding `-resize-output-mode` submits physical output mode and position changes to the compositor, leaving other outputs unchanged. Success depends on the compositor. Tests cover both successful and rejected protocol transactions; physical mode changes require validation in the target deployment.

The server cannot add protocol requests an older client does not send. The original Mirage VNC client uses classic Latin-1 clipboard messages, and its display-selection/server-scaling controls target the Mac protocol. Extended UTF-8 clipboard, standard VNC layout changes, and physical keycode extensions require a client that implements the relevant negotiation. Ordinary video, existing input, audio, and cursor support remain compatible.

## Testing

Tests cover client authentication, three pixel encodings, persistent decompression, keyboard/mouse input, bidirectional text, audio, and cursor behavior. Additional coverage includes extended UTF-8, QEMU input, per-connection scaling, concurrent clients, and cleanup on failure.

Real hardware checks have verified VA-API H.264/HEVC decoding without CPU readback, dual-stream composition, PipeWire output audio, and Hyprland cursors. Real display hotplug, portal behavior across compositors, and long-running multi-client stress still require deployment-specific validation. These checks do not establish full feature parity with WayVNC across all platforms.

```sh
go test -race -tags purego ./cmd/mirage-server ./internal/server/... ./internal/rfb
go vet -tags purego ./cmd/mirage-server ./internal/server/...
# Synthetic images and GPU encoding/decoding; no desktop capture.
MIRAGE_TEST_RFB_GPU=1 go test -tags purego ./internal/server/... -run 'TestRFBHardwareInterop|TestCompositeCapture' -v
# Real DMA-BUF capture, prohibited CPU mapping, buffer release, and client hardware decoding.
MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -race -tags purego ./internal/server/input ./internal/server/capture -run 'TestNativeDMA|TestNativeOutputCapture|TestDMAOwnership' -v
MIRAGE_TEST_RFB_GPU=1 MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -tags purego ./internal/server -run TestRFBHardwareInterop -v
# Real output audio, cursor, and clipboard connections; no input injection or clipboard writes.
MIRAGE_TEST_NATIVE_OUTPUT=MIRAGE go test -tags purego ./internal/server -run TestNativeServicesWithExistingClient -v
```

## Build and install

The build script installs to `~/.local/bin` by default. It does not start or restart the service.

```sh
bash scripts/build-mirage-server.sh
# Or specify an absolute installation prefix.
bash scripts/build-mirage-server.sh /absolute/install/prefix
```

The user-service template is [`scripts/mirage-server.service`](scripts/mirage-server.service), listening on port `5902`. Install it in `~/.config/systemd/user/`, adjust output settings, dimensions, and certificate paths, then enable it:

```sh
systemctl --user daemon-reload
systemctl --user enable --now mirage-server.service
```

Stop any old service occupying the same port first. The binary is installed at `~/.local/bin/mirage-server`. Any existing service on port `5900` is independent.

`-pam-service system-auth` uses `/etc/pam.d/system-auth` for system account authentication instead of `-password-file`. Choose the appropriate PAM policy on other distributions. The template expects the certificate at `~/.config/mirage-server/tls_cert.pem` and its private key at `~/.config/mirage-server/tls_key.pem`; keep the key at mode `0600`. Copy the original certificate and key when migrating an existing service to preserve saved client fingerprints. Clients continue to supply their system account credentials.

The template uses `-output auto`, so startup and restart capture the selected default output without a picker. In portal mode, select the whole screen on first use. `-restore-token-file` stores the portal restore token at mode `0600`; supported backends can reuse it after restart. This token is private machine state and must not be committed. If the backend rejects it, select a screen again.

## Apple SRP authentication

`-ard -srp-file /path/to/srp.json` enables Apple security type 36 (SRP-6a) and strictly forbids fallback to legacy type 30. It uses the RFC 5054 4096-bit group, SHA-512, PBKDF2-HMAC-SHA512, and constant-time modular arithmetic. The client verifies the server's M2 proof, and the server verifies the client's M1 proof. Session keys continue to protect the Apple record layer and media key exchange. Server authentication here is based on SRP password credentials, not a CA certificate, RSA host fingerprint, or Kerberos. Types 33/35 are not implemented.

Enroll from a trusted terminal on the server. Password input is hidden; do not pass it through command-line arguments or chat:

```sh
mirage-server -enroll-srp -username "$(id -un)"
```

By default, credentials are saved to `$XDG_CONFIG_HOME/mirage-server/srp.json`, or `~/.config/mirage-server/srp.json` when that variable is unset. The file is atomically replaced, uses mode `0600`, and stores only the salt, iteration count, and verifier. Override the path with `-srp-file`.

Each file represents one account. These are separate Mirage credentials: the password may match the system password, but it does not automatically follow PAM password changes or enforce PAM account-expiration policies. Re-enroll or delete the file to change or revoke credentials. Restart the service to revoke existing sessions immediately.

Add `-srp-file %h/.config/mirage-server/srp.json` to the service command and remove `-pam-service`. Credentials are read for each connection, so enrollment does not require a restart. Missing, malformed, or overly permissive credential files cause login rejection, not fallback. Credentials must be enrolled locally; they are never learned automatically from legacy remote logins.

Native macOS type-36 interoperability must be tested against the target OS version. Automated tests cover Mirage's mutual proofs, encrypted pictures, incorrect passwords, forged server proofs, invalid public keys, malformed lengths, and downgrade rejection.

Credential format v2 fixes the RFC 5054 group and verifies its digest. The earlier v1 constant was missing one hexadecimal character, causing native Mac clients to reject the challenge. Those credentials cannot be converted. After upgrading, run `mirage-server -enroll-srp` locally again; the same password may be reused.

## License

Mirage Server is released under the [MIT License](LICENSE). Third-party dependencies and bundled components retain their respective licenses and notices.
