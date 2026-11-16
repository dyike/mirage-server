package inputtrace

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var keyboardOnce sync.Once
var keyboardLogger *log.Logger

var KeyboardEnabled = os.Getenv("MIRAGE_KEYBOARD_TRACE") == "1"

// Keyboard records presentation state, never text or key contents. Keep a
// device-local copy so an intermittent failure survives console disconnects.
func Keyboard(format string, args ...any) {
	keyboardOnce.Do(func() {
		if !KeyboardEnabled {
			return
		}
		var output io.Writer = os.Stderr
		if home, err := os.UserHomeDir(); err == nil {
			if f, err := os.OpenFile(filepath.Join(home, "Documents", "mirage-keyboard.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				output = io.MultiWriter(output, f)
			}
		}
		keyboardLogger = log.New(output, "MIRAGE_KEYBOARD: ", log.Ldate|log.Ltime|log.Lmicroseconds)
	})
	if keyboardLogger != nil {
		keyboardLogger.Printf(format, args...)
	}
}
