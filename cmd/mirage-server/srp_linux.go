//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/charmbracelet/x/term"
	"github.com/dyike/mirage-server/internal/server"
)

func defaultSRPPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mirage-server", "srp.json")
}

func enrollSRP(path, username string) error {
	if username == "" {
		u, err := user.Current()
		if err != nil {
			return err
		}
		username = u.Username
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("SRP enrollment needs a local terminal: %w", err)
	}
	defer tty.Close()
	if !term.IsTerminal(tty.Fd()) {
		return fmt.Errorf("SRP enrollment requires a terminal")
	}
	fmt.Fprintf(tty, "Register Mirage SRP account %q. This password is independent of PAM; it may match your Linux password.\nPassword: ", username)
	password, err := term.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return err
	}
	defer clear(password)
	fmt.Fprint(tty, "Confirm password: ")
	confirm, err := term.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return err
	}
	defer clear(confirm)
	if !bytes.Equal(password, confirm) {
		return fmt.Errorf("passwords do not match")
	}
	credential, err := server.NewSRPCredential(username, password)
	if err != nil {
		return err
	}
	if err = server.SaveSRPCredential(path, credential); err != nil {
		return err
	}
	fmt.Fprintf(tty, "SRP verifier saved to %s (0600). Connect as %s. The running server reads it on the next connection.\n", path, username)
	return nil
}
