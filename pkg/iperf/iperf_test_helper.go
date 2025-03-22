package iperf

import (
	"sync"
	"testing"
	"time"

	"github.com/op/go-logging"
)

var (
	// Lock to ensure logging initialization happens only once
	initLogLevelOnce sync.Once
)

// InitTestLogging initializes logging for tests in a safe way
func InitTestLogging() {
	// Initialize logging settings once before any tests run
	initLogLevelOnce.Do(func() {
		// Set all log levels before any goroutines are started
		logging.SetLevel(logging.DEBUG, "iperf")
		logging.SetLevel(logging.DEBUG, "rudp")
	})
}

// Helper function for test synchronization
func WaitForServerStart(t *testing.T, serverTest *IperfTest) bool {
	// Channel to receive state notifications
	stateCh := make(chan uint, 1)

	// Listen for the server to signal IPERF_START
	go func() {
		select {
		case state := <-serverTest.ctrlChan:
			stateCh <- state
		case <-time.After(5 * time.Second):
			stateCh <- 0 // Timeout
		}
	}()

	// Wait for notification
	select {
	case state := <-stateCh:
		if state == IPERF_START {
			return true
		}
		t.Errorf("Expected IPERF_START, got %v", state)
		return false
	case <-time.After(5 * time.Second):
		t.Errorf("Timed out waiting for server to start")
		return false
	}
}
