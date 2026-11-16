//go:build darwin && !ios && amd64

package video

import "image"

func nativeGStreamerAvailable() bool                { return false }
func configureNativeSink(uintptr)                   {}
func nativeGStreamerFactory(string) bool            { return false }
func (*Decoder) nativePicture(uintptr) error        { return ErrUnsupported }
func RetainBuffer(uintptr)                          {}
func ReleaseBuffer(uintptr)                         {}
func Snapshot(uintptr) *image.RGBA                  { return nil }
func CopyBGRA(uintptr, []byte, int, int, int) error { return ErrUnsupported }
