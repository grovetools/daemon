package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ResolveConfigEnv parses the "env" block from provider config and returns
// a fully resolved environment slice (KEY=value format) suitable for exec.Cmd.Env.
//
// The env block supports two value types:
//   - Static string: SOME_VAR = "value"
//   - Command:       SOME_VAR = { cmd = "gcloud secrets ..." }
//
// The returned slice starts with os.Environ() and appends resolved values.
// Commands are executed via "sh -c" in the given workDir.
// If any command fails, the function returns an error immediately.
func ResolveConfigEnv(ctx context.Context, config map[string]interface{}, workDir string) ([]string, error) {
	envBlock, ok := config["env"].(map[string]interface{})
	if !ok || len(envBlock) == 0 {
		return os.Environ(), nil
	}

	baseEnv := os.Environ()
	for key, val := range envBlock {
		resolved, err := resolveEnvValue(ctx, key, val, workDir)
		if err != nil {
			return nil, err
		}
		baseEnv = append(baseEnv, fmt.Sprintf("%s=%s", key, resolved))
	}

	return baseEnv, nil
}

// resolveEnvValue resolves a single env value from config.
// If val is a string, it's used directly.
// If val is a map with a "cmd" key, the command is executed and stdout is captured.
func resolveEnvValue(ctx context.Context, key string, val interface{}, workDir string) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case map[string]interface{}:
		cmdStr, ok := v["cmd"].(string)
		if !ok || cmdStr == "" {
			return "", fmt.Errorf("env var %s: map value must contain a 'cmd' key with a non-empty string", key)
		}
		return runEnvCommand(ctx, key, cmdStr, workDir)
	default:
		return "", fmt.Errorf("env var %s: unsupported value type %T (expected string or {cmd: ...})", key, val)
	}
}

// runEnvCommand executes a shell command and returns its trimmed stdout.
func runEnvCommand(ctx context.Context, key, cmdStr, workDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to resolve env var %s: command failed: %w\nOutput: %s", key, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
