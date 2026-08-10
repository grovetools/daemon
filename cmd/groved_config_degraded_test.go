package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const degradedBootHelperEnv = "GROVED_CONFIG_DEGRADED_HELPER"

func TestGrovedMalformedRecordedConfigBindsStatusOnly(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "config", "grove")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootsPath := filepath.Join(configDir, "roots.toml")
	if err := os.WriteFile(rootsPath, []byte("[roots.broken\npath = 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	shortDir, err := os.MkdirTemp("", "gdg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(shortDir) }()
	sock := filepath.Join(shortDir, "d.sock")
	pidfile := filepath.Join(shortDir, "d.pid")
	logPath := filepath.Join(shortDir, "child.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestGrovedConfigDegradedHelperProcess$")
	child.Env = append(os.Environ(),
		degradedBootHelperEnv+"=1",
		"GROVE_HOME="+home,
		"GROVE_SCOPE=",
		"GROVE_DAEMON_PAIR_PID=",
		"GROVED_TEST_SOCKET="+sock,
		"GROVED_TEST_PIDFILE="+pidfile,
	)
	child.Stdout = logFile
	child.Stderr = logFile
	if err := child.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
		_ = logFile.Close()
	}()

	client := unixSocketHTTPClient(sock)

	var healthBody []byte
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, reqErr := client.Get("http://unix/health")
		if reqErr == nil {
			healthBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("health = %d, want 503: %s", resp.StatusCode, healthBody)
			}
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if len(healthBody) == 0 {
		_ = logFile.Sync()
		logs, _ := os.ReadFile(logPath)
		t.Fatalf("degraded daemon never served health; logs:\n%s", logs)
	}
	var health struct {
		Degraded    bool `json:"degraded"`
		ConfigError struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			Recovery string `json:"recovery"`
		} `json:"config_error"`
	}
	if err := json.Unmarshal(healthBody, &health); err != nil {
		t.Fatalf("health JSON: %v: %s", err, healthBody)
	}
	if !health.Degraded || health.ConfigError.Code != "config_load_failed" ||
		!strings.Contains(health.ConfigError.Message, rootsPath) ||
		health.ConfigError.Recovery != "fix the configuration and restart groved" {
		t.Fatalf("untruthful degradation: %#v", health)
	}

	bootResp, err := client.Get("http://unix/api/system/boot")
	if err != nil {
		t.Fatalf("GET boot status: %v", err)
	}
	bootBody, _ := io.ReadAll(bootResp.Body)
	_ = bootResp.Body.Close()
	var boot struct {
		Done bool   `json:"done"`
		Err  string `json:"err"`
	}
	if bootResp.StatusCode != http.StatusOK || json.Unmarshal(bootBody, &boot) != nil || !boot.Done || !strings.Contains(boot.Err, rootsPath) {
		t.Fatalf("boot status did not retain config error: status=%d body=%s", bootResp.StatusCode, bootBody)
	}

	stateResp, err := client.Get("http://unix/api/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	stateBody, _ := io.ReadAll(stateResp.Body)
	_ = stateResp.Body.Close()
	if stateResp.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(stateBody, []byte(`"code":"config_load_failed"`)) {
		t.Fatalf("state route bypassed status-only mux: status=%d body=%s", stateResp.StatusCode, stateBody)
	}

	for _, tc := range []struct{ path, body string }{
		{"/api/jobs", `{}`},
		{"/api/build/submit", `{}`},
		{"/api/env/up", `{}`},
		{"/api/agents/spawn", `{}`},
		{"/api/repos/ensure", `{}`},
		{"/api/tasks", `{}`},
		{"/api/refresh", `{}`},
		{"/api/channels/send", `{}`},
		{"/api/sync/allow", `{}`},
		{"/api/sync/apply", `{}`},
		{"/api/sync/maintenance", `{}`},
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://unix"+tc.path, bytes.NewBufferString(tc.body))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", tc.path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte(`"code":"config_load_failed"`)) {
			t.Fatalf("POST %s = %d %s", tc.path, resp.StatusCode, raw)
		}
	}

	// Status-only boot must not create any database, queue persistence, or
	// other topology pipeline state under the isolated Grove home.
	var forbidden []string
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr == nil && !d.IsDir() {
			name := strings.ToLower(d.Name())
			if strings.HasSuffix(name, ".db") || strings.Contains(name, "job-queue") {
				forbidden = append(forbidden, path)
			}
		}
		return nil
	})
	if len(forbidden) != 0 {
		t.Fatalf("status-only boot started persistent pipelines: %v", forbidden)
	}

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			_ = logFile.Sync()
			logs, _ := os.ReadFile(logPath)
			t.Fatalf("degraded daemon shutdown: %v\n%s", err, logs)
		}
	case <-time.After(8 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("degraded daemon did not shut down")
	}
}

func TestGrovedConfigDegradedHelperProcess(t *testing.T) {
	if os.Getenv(degradedBootHelperEnv) != "1" {
		return
	}
	cmd := NewGrovedCmd()
	cmd.SetArgs([]string{
		"start",
		"--socket", os.Getenv("GROVED_TEST_SOCKET"),
		"--pidfile", os.Getenv("GROVED_TEST_PIDFILE"),
		"--collectors", "all",
	})
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func unixSocketHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		}},
	}
}
