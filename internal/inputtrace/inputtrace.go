// Package inputtrace records pointer geometry for diagnosing remote click
// offsets. It is disabled unless MIRAGE_INPUT_TRACE names a local file.
package inputtrace

import (
	"log"
	"os"
	"sync"
)

var once sync.Once
var logger *log.Logger

func Printf(format string, args ...any) {
	once.Do(func() {
		if path := os.Getenv("MIRAGE_INPUT_TRACE"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				log.Printf("MIRAGE_INPUT_TRACE: %v", err)
				return
			}
			logger = log.New(f, "", log.Ldate|log.Ltime|log.Lmicroseconds)
		}
	})
	if logger != nil {
		logger.Printf(format, args...)
	}
}
