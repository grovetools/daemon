package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// runServiceBootstrap executes a service's optional [bootstrap] block if the
// declared missing_path does not exist. workDir + env match what the main
// service will receive, so `cd foo && npm install` and $PORT substitution
// behave the same way the service command would.
//
// Idempotency: decided freshly every call via os.Stat. No state-file caching;
// if the user rm -rf's node_modules the next env up bootstraps again.
func (m *Manager) runServiceBootstrap(
	ctx context.Context,
	svcName string,
	svcConfig map[string]interface{},
	workDir string,
	env []string,
	logDir string,
) error {
	bootRaw, ok := svcConfig["bootstrap"].(map[string]interface{})
	if !ok {
		return nil
	}
	missingPath, _ := bootRaw["missing_path"].(string)
	command, _ := bootRaw["command"].(string)
	if missingPath == "" || command == "" {
		return nil
	}
	timeoutSec := toInt(bootRaw["timeout_seconds"])
	if timeoutSec <= 0 {
		timeoutSec = 300
	}

	missingAbs := missingPath
	if !filepath.IsAbs(missingAbs) {
		missingAbs = filepath.Join(workDir, missingAbs)
	}
	if _, err := os.Stat(missingAbs); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("bootstrap stat %s: %w", missingAbs, err)
	}

	m.ulog.Info("Running service bootstrap").
		Field("service", svcName).
		Field("missing_path", missingAbs).
		Log(ctx)

	bootCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	bootCmd := exec.CommandContext(bootCtx, "sh", "-c", command)
	bootCmd.Dir = workDir
	bootCmd.Env = env

	if logDir != "" {
		bootLogPath := filepath.Join(logDir, svcName+"-bootstrap.log")
		if bf, err := os.OpenFile(bootLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			bootCmd.Stdout = bf
			bootCmd.Stderr = bf
			defer bf.Close()
		}
	}

	if err := bootCmd.Run(); err != nil {
		m.ulog.Warn("Service bootstrap failed").
			Err(err).
			Field("service", svcName).
			Log(ctx)
		return fmt.Errorf("service bootstrap failed for %s: %w", svcName, err)
	}
	m.ulog.Info("Service bootstrap completed").
		Field("service", svcName).
		Log(ctx)
	return nil
}
