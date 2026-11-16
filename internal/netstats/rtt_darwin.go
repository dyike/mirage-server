//go:build darwin

package netstats

import (
	"time"

	"golang.org/x/sys/unix"
)

func socketRTT(fd uintptr) (time.Duration, bool) {
	info, err := unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	if err != nil || info.Srtt == 0 {
		return 0, false
	}
	// Darwin's TCP_CONNECTION_INFO reports tcpi_srtt in milliseconds,
	// unlike Linux TCP_INFO. This also serves iOS.
	return time.Duration(info.Srtt) * time.Millisecond, true
}
