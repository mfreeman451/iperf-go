package iperf

import (
	"os"
	"sync"

	"github.com/op/go-logging"
)

/*
Log setting
*/

var Log = logging.MustGetLogger("iperf")

// Add a mutex to protect logging operations
var logMutex sync.Mutex

// Example format string. Everything except the message has a custom color
// which is dependent on the Log level. Many fields have a custom output
// formatting too, eg. the time returns the hour down to the milli second.
var format = logging.MustStringFormatter(
	`%{color}%{time:15:04:05.000} %{shortfunc} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}`,
)

// Use sync.Once to ensure initialization happens only once
var initOnce sync.Once

func init() {
	// Initialize logging safely once
	initOnce.Do(func() {
		backend := logging.NewLogBackend(os.Stderr, "", 0)
		backendFormatter := logging.NewBackendFormatter(backend, format)

		logging.SetLevel(logging.ERROR, "iperf")
		logging.SetBackend(backendFormatter)
	})
}

// SetLogLevelSafe sets the log level in a thread-safe manner
func SetLogLevelSafe(level logging.Level, module string) {
	logMutex.Lock()
	defer logMutex.Unlock()
	logging.SetLevel(level, module)
}
