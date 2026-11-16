//go:build linux && !android && arm64

package eldcodec

import _ "embed"

//go:embed fdk_linux_arm64.bin
var library []byte
