package env

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grovetools/core/pkg/env/services"
)

// buildDockerServiceArgs synthesizes the `docker run` arguments for a
// docker-backed service embedded in a native (or terraform) profile.
//
// Preconditions: svcConfig has type="docker" with image and container_port set.
//
// Returns (args, containerName, error). args is suitable for
// exec.CommandContext(ctx, "docker", args...). The container name is
// deterministically derived from worktree+svcName so teardown and stale-
// cleanup can target it without extra state.
//
// As a side effect, host volume directories are created (mkdir -p) so the
// docker daemon doesn't create them as root on macOS/Linux bind mounts.
func buildDockerServiceArgs(
	worktree, svcName string,
	svcConfig map[string]interface{},
	hostPort int,
	workspacePath string,
	envVars map[string]string,
) ([]string, string, error) {
	image, _ := svcConfig["image"].(string)
	if image == "" {
		return nil, "", fmt.Errorf("service %q has type=docker but no image specified", svcName)
	}

	containerPort := toInt(svcConfig["container_port"])
	if containerPort == 0 {
		return nil, "", fmt.Errorf("service %q has type=docker but no container_port specified", svcName)
	}

	containerName := fmt.Sprintf("grove-%s-%s", worktree, svcName)

	args := []string{
		"run", "--rm",
		"--name", containerName,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, containerPort),
	}

	// Volume bind mounts. mkdir host path so docker doesn't create as root.
	// Sort volume keys for deterministic ordering (important for tests).
	if volumes, ok := svcConfig["volumes"].(map[string]interface{}); ok {
		keys := make([]string, 0, len(volumes))
		for k := range volumes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, vk := range keys {
			volCfg, ok := volumes[vk].(map[string]interface{})
			if !ok {
				continue
			}
			hostPath, _ := volCfg["host_path"].(string)
			containerPath, _ := volCfg["container_path"].(string)
			if hostPath == "" || containerPath == "" {
				continue
			}
			absPath := hostPath
			if !filepath.IsAbs(hostPath) {
				absPath = filepath.Join(workspacePath, hostPath)
			}
			_ = os.MkdirAll(absPath, 0755)
			args = append(args, "-v", fmt.Sprintf("%s:%s", absPath, containerPath))
		}
	}

	// Environment variables. Resolved against envVars (tunnel ports, prior
	// service port_envs) with fallback to process env. Sort for determinism.
	if envMap, ok := svcConfig["env"].(map[string]interface{}); ok {
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val, _ := envMap[k].(string)
			resolved := services.ExpandEnvVars(val, envVars)
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, resolved))
		}
	}

	args = append(args, image)
	return args, containerName, nil
}

// toInt coerces TOML/JSON numeric values (which arrive as float64 or int64)
// to int. Returns 0 if the value is missing or of another type.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	case int32:
		return int(n)
	}
	return 0
}

// shellJoin produces a shell-safe joined string for logging / state
// persistence. It is NOT used to actually invoke commands — exec.Command
// receives raw argv slices. Single quotes with '\'' escaping.
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if needsShellQuote(a) {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

func needsShellQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' ||
			r == '/' || r == '.' || r == ':' || r == '=' || r == ',' {
			continue
		}
		return true
	}
	return false
}
