//go:build linux && !android && amd64

package eldcodec

import _ "embed"

//go:embed fdk_linux_amd64.bin
var library []byte
