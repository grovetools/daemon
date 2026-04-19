package env

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDockerServiceArgs_Basic(t *testing.T) {
	svcConfig := map[string]interface{}{
		"type":           "docker",
		"image":          "clickhouse/clickhouse-server:latest",
		"container_port": float64(8123),
	}
	args, name, err := buildDockerServiceArgs("tier1-c", "clickhouse", svcConfig, 49200, "/tmp/kitchen-env", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "grove-tier1-c-clickhouse" {
		t.Errorf("container name = %q, want grove-tier1-c-clickhouse", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "run --rm --name grove-tier1-c-clickhouse") {
		t.Errorf("expected run --rm --name, got: %s", joined)
	}
	if !strings.Contains(joined, "-p 127.0.0.1:49200:8123") {
		t.Errorf("expected port mapping 127.0.0.1:49200:8123, got: %s", joined)
	}
	if !strings.HasSuffix(joined, "clickhouse/clickhouse-server:latest") {
		t.Errorf("expected image at end, got: %s", joined)
	}
}

func TestBuildDockerServiceArgs_MissingImage(t *testing.T) {
	svcConfig := map[string]interface{}{
		"type":           "docker",
		"container_port": float64(8123),
	}
	_, _, err := buildDockerServiceArgs("wt", "svc", svcConfig, 1234, "/tmp", map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("expected error about missing image, got: %v", err)
	}
}

func TestBuildDockerServiceArgs_MissingContainerPort(t *testing.T) {
	svcConfig := map[string]interface{}{
		"type":  "docker",
		"image": "nginx:latest",
	}
	_, _, err := buildDockerServiceArgs("wt", "svc", svcConfig, 1234, "/tmp", map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing container_port, got nil")
	}
	if !strings.Contains(err.Error(), "container_port") {
		t.Errorf("expected error about missing container_port, got: %v", err)
	}
}

func TestBuildDockerServiceArgs_VolumesAndEnv(t *testing.T) {
	tmp := t.TempDir()
	svcConfig := map[string]interface{}{
		"type":           "docker",
		"image":          "clickhouse/clickhouse-server:latest",
		"container_port": float64(8123),
		"volumes": map[string]interface{}{
			"data": map[string]interface{}{
				"host_path":      ".grove/volumes/clickhouse",
				"container_path": "/var/lib/clickhouse",
			},
		},
		"env": map[string]interface{}{
			"CLICKHOUSE_PASSWORD":                  "",
			"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1",
		},
	}
	args, _, err := buildDockerServiceArgs("tier1-c", "clickhouse", svcConfig, 49200, tmp, map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")

	expectedHostPath := filepath.Join(tmp, ".grove/volumes/clickhouse")
	expectedVolumeArg := expectedHostPath + ":/var/lib/clickhouse"
	if !strings.Contains(joined, expectedVolumeArg) {
		t.Errorf("expected volume binding %s, got: %s", expectedVolumeArg, joined)
	}

	if !strings.Contains(joined, "-e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1") {
		t.Errorf("expected CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1, got: %s", joined)
	}
	if !strings.Contains(joined, "-e CLICKHOUSE_PASSWORD=") {
		t.Errorf("expected CLICKHOUSE_PASSWORD=, got: %s", joined)
	}
}

func TestBuildDockerServiceArgs_EnvVarSubstitution(t *testing.T) {
	svcConfig := map[string]interface{}{
		"type":           "docker",
		"image":          "nginx:latest",
		"container_port": float64(80),
		"env": map[string]interface{}{
			"UPSTREAM": "http://backend:$API_PORT",
		},
	}
	envVars := map[string]string{"API_PORT": "3000"}
	args, _, err := buildDockerServiceArgs("wt", "svc", svcConfig, 49200, "/tmp", envVars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-e UPSTREAM=http://backend:3000") {
		t.Errorf("expected UPSTREAM substitution, got: %s", joined)
	}
}

func TestBuildDockerServiceArgs_Int64ContainerPort(t *testing.T) {
	// TOML-decoded numbers may arrive as int64; ensure both paths work.
	svcConfig := map[string]interface{}{
		"type":           "docker",
		"image":          "nginx:latest",
		"container_port": int64(8080),
	}
	args, _, err := buildDockerServiceArgs("wt", "svc", svcConfig, 49200, "/tmp", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "-p 127.0.0.1:49200:8080") {
		t.Errorf("expected port 8080, got: %v", args)
	}
}

func TestShellJoin(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"docker", "run"}, "docker run"},
		{[]string{"docker", "run", "--name", "grove-wt-svc"}, "docker run --name grove-wt-svc"},
		{[]string{"-e", "KEY=val with space"}, "-e 'KEY=val with space'"},
		{[]string{"-e", ""}, "-e ''"},
	}
	for _, c := range cases {
		got := shellJoin(c.in)
		if got != c.want {
			t.Errorf("shellJoin(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
