// Package pidfile provides PID file management for the grove daemon.
package pidfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/grovetools/core/pkg/process"
)

// Acquire writes the current PID to the file.
// It returns an error if another instance is already running.
func Acquire(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create pid directory: %w", err)
	}

	// Check if file exists
	if content, err := os.ReadFile(path); err == nil {
		pidStr := strings.TrimSpace(string(content))
		if pid, err := strconv.Atoi(pidStr); err == nil {
			if process.IsProcessAlive(pid) {
				return fmt.Errorf("daemon already running with PID %d", pid)
			}
			// Process is dead, cleanup stale file
			_ = os.Remove(path)
		}
	}

	// Write current PID
	pid := os.Getpid()
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("failed to write pid file: %w", err)
	}

	return nil
}

// Release removes the PID file.
func Release(path string) error {
	return os.Remove(path)
}

// Read returns the PID from the file, or 0 if not found/invalid.
func Read(path string) (int, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(content))
	return strconv.Atoi(pidStr)
}

// IsRunning checks if the daemon described by the pidfile is active.
func IsRunning(path string) (bool, int, error) {
	pid, err := Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	return process.IsProcessAlive(pid), pid, nil
}
