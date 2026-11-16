package rfb

import "syscall"

func reuseMediaPort(fd uintptr) error {
	// Winsock uses a Handle and has no SO_REUSEPORT. Address reuse is the
	// available equivalent for Apple's same-port UDP media connection.
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
