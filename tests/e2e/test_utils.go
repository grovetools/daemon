package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// FindBinary is a helper to find the binary path for tests.
// It checks in the following order:
// 1. DAEMON_BINARY environment variable
// 2. Common relative paths from test execution directory
// 3. System PATH
func FindBinary() (string, error) {
	// Check environment variable first
	if binary := os.Getenv("DAEMON_BINARY"); binary != "" {
		return binary, nil
	}

	// Try common locations relative to test execution directory
	candidates := []string{
		"./bin/daemon",
		"../bin/daemon",
		"../../bin/daemon",
		"../../../bin/daemon",
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			absPath, err := filepath.Abs(candidate)
			if err != nil {
				return "", err
			}
			return absPath, nil
		}
	}

	// Try to find in PATH
	if path, err := exec.LookPath("daemon"); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("could not find daemon binary - please set DAEMON_BINARY environment variable or ensure daemon is built and in PATH")
}
