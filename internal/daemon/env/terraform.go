package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/envtf"
)

// Local aliases keep the daemon package's call sites terse without dragging
// the envtf-prefixed names into every terraform.go line. They are purely a
// naming convenience — the canonical definitions live in core/pkg/envtf.
type groveContext = envtf.GroveContext
type groveVolumeConfig = envtf.GroveVolumeConfig
type backendConfig = envtf.BackendConfig
type tfOutput = envtf.TfOutput

// generateEnvId is retained as a package-local shim over envtf.GenerateEnvId
// so existing tests (e.g. TestGenerateEnvId) and daemon call sites don't have
// to spell out the envtf prefix.
func generateEnvId(worktreeName string) int { return envtf.GenerateEnvId(worktreeName) }

// resolveBackend is a package-local shim that forwards to envtf.ResolveBackend.
func resolveBackend(req coreenv.EnvRequest) backendConfig { return envtf.ResolveBackend(req) }

// buildTfVarsPayload is a package-local shim that forwards to envtf.BuildTfVarsPayload.
func buildTfVarsPayload(req coreenv.EnvRequest, imageVars map[string]string, sharedOutputs map[string]interface{}) (map[string]interface{}, error) {
	return envtf.BuildTfVarsPayload(req, imageVars, sharedOutputs)
}

// writeTfVars is a package-local shim that forwards to envtf.WriteTfVars.
func writeTfVars(stateDir string, payload map[string]interface{}) (string, error) {
	return envtf.WriteTfVars(stateDir, payload)
}

// writeBackendOverride is a package-local shim that forwards to envtf.WriteBackendOverride.
func writeBackendOverride(moduleDir string, bc backendConfig) (string, error) {
	return envtf.WriteBackendOverride(moduleDir, bc)
}

// buildInitArgs is a package-local shim that forwards to envtf.BuildInitArgs.
func buildInitArgs(bc backendConfig) []string { return envtf.BuildInitArgs(bc) }

// buildImages builds container images, returning a map of image_<key> -> fullURI.
//
// Two modes:
//   - cmd: User provides a custom build command. The daemon sets GROVE_IMAGE_TAG and
//     GROVE_IMAGE_URI in the command's environment. The command is responsible for
//     building and pushing the image.
//   - docker (default): Uses local docker build + push with context/dockerfile/registry.
func (m *Manager) buildImages(ctx context.Context, req coreenv.EnvRequest) (map[string]string, error) {
	images, ok := req.Config["images"].(map[string]interface{})
	if !ok || len(images) == 0 {
		return nil, nil
	}

	result := make(map[string]string)
	worktree := req.Workspace.Name
	stateDir := req.EffectiveStateDir()

	logsDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	for key, imgCfgRaw := range images {
		imgCfg, ok := imgCfgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		registry, _ := imgCfg["registry"].(string)
		if registry == "" {
			return nil, fmt.Errorf("image %q requires registry", key)
		}

		tag := fmt.Sprintf("grove-%s-%d", worktree, time.Now().Unix())
		fullURI := registry + ":" + tag

		logFile := filepath.Join(logsDir, "build-"+key+".log")

		if cmdStr, ok := imgCfg["cmd"].(string); ok && cmdStr != "" {
			buildCmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
			buildCmd.Dir = req.Workspace.Path
			buildCmd.Env = append(os.Environ(),
				"GROVE_IMAGE_TAG="+tag,
				"GROVE_IMAGE_URI="+fullURI,
				"GROVE_IMAGE_REGISTRY="+registry,
			)
			output, err := buildCmd.CombinedOutput()
			_ = os.WriteFile(logFile, output, 0644)
			if err != nil {
				return nil, fmt.Errorf("image build command failed for %q: %w\nSee log: %s", key, err, logFile)
			}
		} else {
			contextPath, _ := imgCfg["context"].(string)
			dockerfile, _ := imgCfg["dockerfile"].(string)

			if contextPath == "" {
				return nil, fmt.Errorf("image %q requires context or cmd", key)
			}

			if !filepath.IsAbs(contextPath) {
				contextPath = filepath.Join(req.Workspace.Path, contextPath)
			}

			buildArgs := []string{"build", "-t", fullURI}
			if dockerfile != "" {
				buildArgs = append(buildArgs, "-f", dockerfile)
			}
			buildArgs = append(buildArgs, contextPath)

			buildCmd := exec.CommandContext(ctx, "docker", buildArgs...)
			buildOutput, err := buildCmd.CombinedOutput()
			_ = os.WriteFile(logFile, buildOutput, 0644)
			if err != nil {
				return nil, fmt.Errorf("docker build failed for %q: %w\nSee log: %s", key, err, logFile)
			}

			pushCmd := exec.CommandContext(ctx, "docker", "push", fullURI)
			pushOutput, err := pushCmd.CombinedOutput()
			_ = os.WriteFile(logFile, append(buildOutput, pushOutput...), 0644)
			if err != nil {
				return nil, fmt.Errorf("docker push failed for %q: %w\nSee log: %s", key, err, logFile)
			}
		}

		result["image_"+key] = fullURI
		m.ulog.Info("Built and pushed image").
			Field("image", key).
			Field("uri", fullURI).
			Log(ctx)
	}

	return result, nil
}

// fetchSharedOutputs wraps envtf.FetchSharedOutputs with a log line so the
// daemon surfaces the shared-infra lookup in its structured logs. Kept as a
// method on Manager so future manager-level instrumentation (metrics, tracing)
// can hook it without touching the envtf package.
func (m *Manager) fetchSharedOutputs(ctx context.Context, sharedCfg map[string]interface{}) (map[string]interface{}, error) {
	result, err := envtf.FetchSharedOutputs(ctx, sharedCfg)
	if err != nil {
		return nil, err
	}
	m.ulog.Info("Fetched shared infrastructure outputs").
		Field("outputs", len(result)).
		Log(ctx)
	return result, nil
}

// terraformUp provisions infrastructure via Terraform and optionally starts tunnels.
func (m *Manager) terraformUp(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("terraform provider requires a workspace")
	}
	worktree := req.Workspace.Name

	m.mu.Lock()
	if _, err := m.reconcileExistingEnv(ctx, worktree); err != nil {
		return nil, err
	}

	runningEnv := newRunningEnv("terraform", worktree, req.Profile, req.StateDir)
	m.envs[worktree] = runningEnv
	m.mu.Unlock()

	// Resolve config.env (static values + cmd-based secrets)
	resolvedEnv, err := ResolveConfigEnv(ctx, req.Config, req.Workspace.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve environment variables: %w", err)
	}
	m.ulog.Info("Resolved config environment variables").
		Field("count", len(resolvedEnv)-len(os.Environ())).
		Log(ctx)

	// Resolve the TF module path (relative to workspace root)
	modulePath, _ := req.Config["path"].(string)
	if modulePath == "" {
		modulePath = "./infra"
	}
	moduleAbs := filepath.Join(req.Workspace.Path, modulePath)
	stateDir := req.EffectiveStateDir()

	// Helper to build env for terraform commands
	tfEnv := append(resolvedEnv, "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))

	// Phase 3: Build images before Terraform
	imageVars, err := m.buildImages(ctx, req)
	if err != nil {
		return nil, err
	}

	// Phase 4: Fetch shared infrastructure outputs
	var sharedOutputs map[string]interface{}
	if sharedCfg, ok := req.Config["shared_backend_config"].(map[string]interface{}); ok {
		sharedOutputs, err = m.fetchSharedOutputs(ctx, sharedCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch shared outputs: %w", err)
		}
	}

	// Phase 1: Build combined tfvars payload (grove context + user vars + images + shared)
	payload, err := buildTfVarsPayload(req, imageVars, sharedOutputs)
	if err != nil {
		return nil, err
	}

	varsPath, err := writeTfVars(stateDir, payload)
	if err != nil {
		return nil, err
	}

	// Collect var file args
	var varFileArgs []string
	varFileArgs = append(varFileArgs, "-var-file="+varsPath)
	if extraVarFile, ok := req.Config["var_file"].(string); ok && extraVarFile != "" {
		abs := filepath.Join(req.Workspace.Path, extraVarFile)
		varFileArgs = append(varFileArgs, "-var-file="+abs)
	}

	// Phase 2: Resolve backend and write override if needed
	bc := resolveBackend(req)
	overridePath, err := writeBackendOverride(moduleAbs, bc)
	if err != nil {
		return nil, err
	}
	if overridePath != "" {
		defer os.Remove(overridePath)
	}

	// terraform init
	initArgs := buildInitArgs(bc)
	initCmd := exec.CommandContext(ctx, "terraform", initArgs...)
	initCmd.Dir = moduleAbs
	initCmd.Env = tfEnv
	if output, err := initCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w\nOutput: %s", err, string(output))
	}

	// terraform apply
	applyArgs := []string{"apply", "-auto-approve", "-input=false"}
	if bc.Type == "local" {
		applyArgs = append(applyArgs, "-state="+filepath.Join(stateDir, "terraform.tfstate"))
	}
	applyArgs = append(applyArgs, varFileArgs...)
	applyCmd := exec.CommandContext(ctx, "terraform", applyArgs...)
	applyCmd.Dir = moduleAbs
	applyCmd.Env = tfEnv
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform apply failed: %w\nOutput: %s", err, string(output))
	}

	// Read terraform outputs to populate env vars and endpoints
	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
		State:   make(map[string]string),
	}

	// Persist image URIs into opaque provider state so `grove env drift` can
	// reuse them on subsequent runs instead of rebuilding (which would produce
	// a new tag and falsely flag Cloud Run services as drifted). Only the
	// image_* keys live here; actual env vars still flow through EnvVars.
	for k, v := range imageVars {
		resp.State[k] = v
	}

	outputArgs := []string{"output", "-json"}
	if bc.Type == "local" {
		outputArgs = append(outputArgs, "-state="+filepath.Join(stateDir, "terraform.tfstate"))
	}
	outputCmd := exec.CommandContext(ctx, "terraform", outputArgs...)
	outputCmd.Dir = moduleAbs
	outputCmd.Env = tfEnv
	outputBytes, err := outputCmd.Output()
	if err == nil && len(outputBytes) > 0 {
		var outputs map[string]tfOutput
		if err := json.Unmarshal(outputBytes, &outputs); err == nil {
			m.mapTerraformOutputs(outputs, req.Config, resp)
		}
	}

	// Tunnel + local-services log directory (shared with native provider).
	logDir := filepath.Join(req.Workspace.Path, ".grove", "env", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		m.ulog.Warn("Failed to create log directory; tunnel + service output will be discarded").
			Err(err).
			Log(ctx)
		logDir = ""
	}

	// Start tunnels BEFORE local services so the allocated tunnel ports / URLs
	// land in resp.EnvVars and downstream local processes (e.g. a local API
	// pointed at a tunneled cloud DB) can read CLICKHOUSE_URL et al. from their
	// environment via cmd.Env construction in startLocalServices.
	if tunnels, ok := req.Config["tunnels"].(map[string]interface{}); ok {
		for tunnelName, tunnelCfgRaw := range tunnels {
			tunnelCfg, ok := tunnelCfgRaw.(map[string]interface{})
			if !ok {
				continue
			}

			cmdStr, _ := tunnelCfg["command"].(string)
			localPortEnv, _ := tunnelCfg["local_port_env"].(string)
			urlTemplate, _ := tunnelCfg["url_template"].(string)

			port, err := m.Ports.Allocate(fmt.Sprintf("%s/tf-tunnel-%s", worktree, tunnelName))
			if err != nil {
				continue
			}
			runningEnv.Ports["tunnel-"+tunnelName] = port

			pgid, err := m.Tunnels.Start(context.Background(), worktree, tunnelName, cmdStr, port, req.Workspace.Path, resp.EnvVars, logDir)
			if err != nil {
				m.ulog.Warn("Failed to start tunnel").
					Err(err).
					Field("tunnel", tunnelName).
					Log(ctx)
				continue
			}
			if pgid > 0 {
				if runningEnv.NativePGIDs == nil {
					runningEnv.NativePGIDs = make(map[string]int)
				}
				runningEnv.NativePGIDs["tunnel-"+tunnelName] = pgid
			}

			if urlTemplate != "" {
				finalURL := strings.ReplaceAll(urlTemplate, "{{.AllocatedPort}}", fmt.Sprintf("%d", port))
				resp.EnvVars[localPortEnv] = finalURL
			} else if localPortEnv != "" {
				resp.EnvVars[localPortEnv] = fmt.Sprintf("%d", port)
			}
		}
	}

	// Hybrid mode: spawn local services after the cloud env is up. cmd.Env in
	// startLocalServices is built from resp.EnvVars (just populated by the
	// terraform outputs and tunnels above), so a local `cargo run` sees both
	// the tunneled CLICKHOUSE_URL and any cloud-side TF outputs.
	if _, hasServices := req.Config["services"].(map[string]interface{}); hasServices {
		if err := m.startLocalServices(ctx, req, runningEnv, resp, tfEnv, logDir); err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// terraformDown destroys Terraform-managed infrastructure and cleans up.
//
// If req.Config["skip_destroy"] is true, the cloud-side `terraform destroy`
// step is skipped entirely — only local processes, tunnels, and proxy routes
// are torn down. This makes hybrid profiles (local API + tunneled cloud DB)
// safe to bounce without nuking the per-worktree GCE instance. To actually
// destroy the cloud resources, run `grove env down --clean` or switch to the
// base terraform profile.
func (m *Manager) terraformDown(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("terraform provider requires a workspace")
	}
	worktree := req.Workspace.Name

	skipDestroy, _ := req.Config["skip_destroy"].(bool)
	// --clean overrides skip_destroy: the user explicitly asked for a hard wipe.
	if req.Clean {
		skipDestroy = false
	}
	// ForceDestroy also overrides skip_destroy. Set by `flow plan finish`
	// when retiring a worktree — preserving cloud state across iteration
	// no longer applies, so the semi-persistent contract is broken.
	if req.ForceDestroy && skipDestroy {
		m.ulog.Info("Cloud resources destroyed (skip_destroy overridden by flow plan finish)").
			Field("worktree", worktree).
			Log(ctx)
		skipDestroy = false
	}

	m.mu.Lock()
	runningEnv, exists := m.envs[worktree]
	if exists {
		delete(m.envs, worktree)
	}
	m.mu.Unlock()

	// Cancel any local services spawned by terraformUp's hybrid path. The
	// background goroutine in startLocalServices already calls Wait() for
	// each cmd — calling it a second time here deadlocks. Just cancel and
	// let the goroutine finish reaping. (See nativeDown for the same pattern.)
	//
	// Disk-lazy fallback: if there's no in-memory record (post-restart),
	// read state.json and reap NativePGIDs / DockerContainers. This MUST
	// happen before `terraform destroy` — destroy can take minutes against
	// a real cloud backend, and leaving the local hybrid processes running
	// during that window keeps the symptom (orphaned cargo run holding the
	// kitchen-api port) the redesign is meant to fix.
	if exists && runningEnv != nil {
		for name, cancel := range runningEnv.Cancels {
			m.ulog.Info("Stopping local service").Field("service", name).Log(ctx)
			cancel()
		}
	} else {
		stateFile, sfErr := m.readStateFile(req)
		if sfErr != nil {
			m.ulog.Warn("Failed to read env state for disk-lazy terraformDown").
				Err(sfErr).
				Field("worktree", worktree).
				Log(ctx)
		}
		if stateFile != nil {
			m.ulog.Info("Disk-lazy terraform teardown (reaping local before destroy)").
				Field("worktree", worktree).
				Field("native_pgids", len(stateFile.NativePGIDs)).
				Field("docker_containers", len(stateFile.DockerContainers)).
				Log(ctx)
			m.reapPersistedNatives(ctx, stateFile)
		}
	}

	// Stop tunnels and unregister proxy routes
	m.Tunnels.StopAll(worktree)
	m.unregisterProxyRoutes(ctx, worktree)
	m.Ports.ReleaseAll(worktree)

	if skipDestroy {
		m.ulog.Info("skip_destroy=true: stopped local processes/tunnels/proxy only; cloud resources left intact. Run `grove env down --clean` or switch to the base terraform profile to actually destroy them.").
			Field("worktree", worktree).
			Log(ctx)
		return &coreenv.EnvResponse{Status: "stopped"}, nil
	}

	// Resolve config.env for destroy (providers may need credentials)
	resolvedEnv, err := ResolveConfigEnv(ctx, req.Config, req.Workspace.Path)
	if err != nil {
		m.ulog.Warn("failed to resolve config env for destroy, proceeding with system env").
			Err(err).
			Log(ctx)
		resolvedEnv = os.Environ()
	}

	// Resolve module path
	modulePath, _ := req.Config["path"].(string)
	if modulePath == "" {
		modulePath = "./infra"
	}
	moduleAbs := filepath.Join(req.Workspace.Path, modulePath)
	stateDir := req.EffectiveStateDir()

	// Helper to build env for terraform commands
	tfEnvDown := append(resolvedEnv, "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))

	// Phase 2: Resolve backend
	bc := resolveBackend(req)

	// Determine if we have state to destroy
	hasState := false
	if bc.Type == "gcs" {
		// With remote backend, always attempt destroy (state is remote)
		hasState = true
	} else {
		// Local backend: check if state file exists
		stateFile := filepath.Join(stateDir, "terraform.tfstate")
		if _, err := os.Stat(stateFile); err == nil {
			hasState = true
		}
	}

	if hasState {
		// Phase 5: Generate tfvars for destroy (TF needs vars for count/for_each evaluation)
		payload, err := buildTfVarsPayload(req, nil, nil)
		if err != nil {
			m.ulog.Warn("failed to build tfvars for destroy, proceeding anyway").Err(err).Log(ctx)
		} else {
			if _, err := writeTfVars(stateDir, payload); err != nil {
				m.ulog.Warn("failed to write tfvars for destroy, proceeding anyway").Err(err).Log(ctx)
			}
		}

		varsPath := filepath.Join(stateDir, "grove_context.auto.tfvars.json")

		// Write backend override for GCS
		overridePath, err := writeBackendOverride(moduleAbs, bc)
		if err != nil {
			m.ulog.Warn("failed to write backend override for destroy").Err(err).Log(ctx)
		}
		if overridePath != "" {
			defer os.Remove(overridePath)
		}

		// terraform init (needed for backend config)
		initArgs := buildInitArgs(bc)
		initCmd := exec.CommandContext(ctx, "terraform", initArgs...)
		initCmd.Dir = moduleAbs
		initCmd.Env = tfEnvDown
		if output, err := initCmd.CombinedOutput(); err != nil {
			m.ulog.Warn("terraform init for destroy failed").
				Err(err).
				Field("output", string(output)).
				Log(ctx)
		}

		// terraform destroy
		destroyArgs := []string{"destroy", "-auto-approve", "-input=false"}
		if bc.Type == "local" {
			destroyArgs = append(destroyArgs, "-state="+filepath.Join(stateDir, "terraform.tfstate"))
		}
		if _, err := os.Stat(varsPath); err == nil {
			destroyArgs = append(destroyArgs, "-var-file="+varsPath)
		}
		if extraVarFile, ok := req.Config["var_file"].(string); ok && extraVarFile != "" {
			abs := filepath.Join(req.Workspace.Path, extraVarFile)
			destroyArgs = append(destroyArgs, "-var-file="+abs)
		}

		cmd := exec.CommandContext(ctx, "terraform", destroyArgs...)
		cmd.Dir = moduleAbs
		cmd.Env = tfEnvDown
		if output, err := cmd.CombinedOutput(); err != nil {
			m.ulog.Warn("terraform destroy failed").
				Err(err).
				Field("output", string(output)).
				Log(ctx)
		}
	}

	// Phase 5: Cleanup based on --clean flag
	if req.Clean {
		// Remove .terraform directory and local state
		_ = os.RemoveAll(filepath.Join(stateDir, ".terraform"))
		_ = os.Remove(filepath.Join(stateDir, "terraform.tfstate"))
		_ = os.Remove(filepath.Join(stateDir, "terraform.tfstate.backup"))
	}

	// Always clean up the tfvars file
	_ = os.Remove(filepath.Join(stateDir, "grove_context.auto.tfvars.json"))

	return &coreenv.EnvResponse{Status: "stopped"}, nil
}

// mapTerraformOutputs maps Terraform outputs to EnvResponse fields.
// If config contains "output_env_map", it maps output names to env var names.
// Otherwise, all non-sensitive string outputs are exported as uppercase env vars.
//
// Endpoints are populated in both paths. By default, any value with an http(s)
// scheme is treated as an endpoint. If config["display_endpoints"] is set,
// only env vars named in that list qualify as endpoints (the http/https scheme
// check still applies, so non-URL values are filtered out).
func (m *Manager) mapTerraformOutputs(outputs map[string]tfOutput, config map[string]interface{}, resp *coreenv.EnvResponse) {
	allowedEndpoints := parseDisplayEndpoints(config["display_endpoints"])

	isEndpoint := func(val, envName string) bool {
		if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") {
			return false
		}
		if allowedEndpoints == nil {
			return true
		}
		return allowedEndpoints[envName]
	}

	// Check for explicit mapping: output_env_map = { "db_url" = "DATABASE_URL", ... }
	if envMap, ok := config["output_env_map"].(map[string]interface{}); ok {
		for outputName, envNameRaw := range envMap {
			envName, ok := envNameRaw.(string)
			if !ok {
				continue
			}
			if out, exists := outputs[outputName]; exists {
				val := fmt.Sprintf("%v", out.Value)
				resp.EnvVars[envName] = val
				if isEndpoint(val, envName) {
					resp.Endpoints = append(resp.Endpoints, val)
				}
			}
		}
		return
	}

	// Default: export all non-sensitive string outputs as UPPER_CASE env vars
	for name, out := range outputs {
		if out.Sensitive {
			continue
		}
		val := fmt.Sprintf("%v", out.Value)
		envName := strings.ToUpper(name)
		resp.EnvVars[envName] = val

		if isEndpoint(val, envName) {
			resp.Endpoints = append(resp.Endpoints, val)
		}
	}
}

// parseDisplayEndpoints coerces a config value (as decoded from JSON/TOML) into
// a set of allowed endpoint env var names. Returns nil if the value is missing
// or malformed, signaling "no filter — accept any http(s) value as an endpoint".
func parseDisplayEndpoints(raw interface{}) map[string]bool {
	if raw == nil {
		return nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	allowed := make(map[string]bool, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			allowed[s] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}
