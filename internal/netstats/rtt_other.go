//go:build !linux && !darwin

package netstats

import "time"

func socketRTT(uintptr) (time.Duration, bool) { return 0, false }
