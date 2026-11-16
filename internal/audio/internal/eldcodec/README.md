# Native AAC-ELD decoder

Windows and Linux amd64/arm64 load the bundled FDK AAC 2.0.3 native decoder
through purego. The application does not use CGO, WASM, FFmpeg, or an installed
codec DLL for this path. Each connection owns an independent decoder handle.
The library is installed in a content-addressed user cache before loading;
its bytes are verified against the embedded copy. Android instead uses the
public MediaCodec NDK interface; Apple platforms retain system AudioQueue.

Unmodified corresponding source is provided in `source/fdk-aac-2.0.3.tar.gz`.
Its SHA256 is
`e25671cd96b10bad896aa42ab91a695a9e573395262baed4e4a2ff178d6a3a78`.
The upstream source is also available at
https://github.com/mstorsjo/fdk-aac/releases/tag/v2.0.3.
See `NOTICE` for the complete upstream license. Preserve this notice and the
corresponding source when distributing the codec binaries.

To regenerate a binary, extract the provided source, install the build-only
Zig 0.14.1 toolchain, and run from the repository root:

```text
python scripts/build-aaceld.py --zig /path/to/zig --source /path/to/fdk-aac-2.0.3 --platform windows_amd64
```

Other targets: `windows_arm64`, `linux_amd64`, `linux_arm64`. This build
disables build-date strings and links only the decoder libraries. No encoder
or third-party runtime is required by the application.
