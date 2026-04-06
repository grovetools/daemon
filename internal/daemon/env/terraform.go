package env

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

// groveContext is the set of standard variables injected into Terraform as auto.tfvars.json.
type groveContext struct {
	GroveEcosystem string                       `json:"grove_ecosystem"`
	GroveProject   string                       `json:"grove_project"`
	GroveWorktree  string                       `json:"grove_worktree"`
	GroveBranch    string                       `json:"grove_branch,omitempty"`
	GrovePlanDir   string                       `json:"grove_plan_dir"`
	GroveVolumes   map[string]groveVolumeConfig `json:"grove_volumes,omitempty"`
	EnvName        string                       `json:"env_name"`
	GroveEnvId     int                          `json:"grove_env_id"`
}

// groveVolumeConfig represents a volume's configuration passed to Terraform modules.
type groveVolumeConfig struct {
	Service     string `json:"service"`
	HostPath    string `json:"host_path"`
	Persist     bool   `json:"persist,omitempty"`
	SnapshotURL string `json:"snapshot_url,omitempty"`
	RestoreCmd  string `json:"restore_command,omitempty"`
}

// backendConfig holds resolved Terraform backend configuration.
type backendConfig struct {
	Type   string // "gcs" or "local"
	Bucket string // GCS bucket name
	Prefix string // GCS state prefix
}

// generateEnvId produces a deterministic environment ID (1-255) from a worktree name.
func generateEnvId(worktreeName string) int {
	h := fnv.New32a()
	h.Write([]byte(worktreeName))
	return int(h.Sum32()%255) + 1
}

// resolveBackend determines the Terraform backend configuration from the request config.
// The GCS prefix follows the pattern: <ecosystem>/<project>/<ref> where:
//   - project = first path component of config["path"] (e.g., "kitchen-infra", "kitchen-app")
//   - ref = current git branch or worktree name
func resolveBackend(req coreenv.EnvRequest) backendConfig {
	stateBackend, _ := req.Config["state_backend"].(string)
	stateBucket, _ := req.Config["state_bucket"].(string)

	if stateBackend == "gcs" && stateBucket != "" {
		ecosystem := ""
		if req.Workspace != nil && req.Workspace.ParentEcosystemPath != "" {
			ecosystem = filepath.Base(req.Workspace.ParentEcosystemPath)
		}
		if ecosystem == "" && req.Workspace != nil && req.Workspace.RootEcosystemPath != "" {
			ecosystem = filepath.Base(req.Workspace.RootEcosystemPath)
		}

		// Derive project name from config path (first component)
		project := "default"
		if configPath, ok := req.Config["path"].(string); ok && configPath != "" {
			parts := strings.SplitN(filepath.Clean(configPath), string(filepath.Separator), 2)
			if len(parts) > 0 && parts[0] != "." {
				project = parts[0]
			}
		}

		// Use git branch (from config) or worktree name as the ref
		ref := ""
		if branch, ok := req.Config["branch"].(string); ok && branch != "" {
			ref = branch
		} else {
			ref = currentGitBranch(req.Workspace)
		}

		prefix := project + "/" + ref
		if ecosystem != "" {
			prefix = ecosystem + "/" + project + "/" + ref
		}
		return backendConfig{Type: "gcs", Bucket: stateBucket, Prefix: prefix}
	}

	return backendConfig{Type: "local"}
}

// currentGitBranch returns the current git branch for the workspace, falling back to the workspace name.
func currentGitBranch(ws *workspace.WorkspaceNode) string {
	if ws == nil {
		return "default"
	}
	// Try to get the branch from git
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = ws.Path
	out, err := cmd.Output()
	if err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" && branch != "HEAD" {
			return branch
		}
	}
	return ws.Name
}

// buildTfVarsPayload creates the combined variables map from grove context, user vars,
// shared outputs, and image URIs.
func buildTfVarsPayload(req coreenv.EnvRequest, imageVars map[string]string, sharedOutputs map[string]interface{}) (map[string]interface{}, error) {
	worktree := req.Workspace.Name
	stateDir := req.EffectiveStateDir()

	// Start with grove context fields
	gctx := groveContext{
		GroveProject:  req.Workspace.Name,
		GroveWorktree: worktree,
		GrovePlanDir:  stateDir,
		EnvName:       worktree,
		GroveEnvId:    generateEnvId(worktree),
	}
	if req.Workspace.ParentEcosystemPath != "" {
		gctx.GroveEcosystem = filepath.Base(req.Workspace.ParentEcosystemPath)
	}
	if branch, ok := req.Config["branch"].(string); ok {
		gctx.GroveBranch = branch
	}

	// Collect volume configs from services
	if services, ok := req.Config["services"].(map[string]interface{}); ok {
		for svcName, svcConfigRaw := range services {
			svcConfig, ok := svcConfigRaw.(map[string]interface{})
			if !ok {
				continue
			}
			volumes, ok := svcConfig["volumes"].(map[string]interface{})
			if !ok {
				continue
			}
			for volName, volCfgRaw := range volumes {
				volCfg, ok := volCfgRaw.(map[string]interface{})
				if !ok {
					continue
				}
				hostPath, _ := volCfg["host_path"].(string)
				persist, _ := volCfg["persist"].(bool)
				vc := groveVolumeConfig{
					Service:  svcName,
					HostPath: hostPath,
					Persist:  persist,
				}
				if restoreCfg, ok := volCfg["restore"].(map[string]interface{}); ok {
					vc.RestoreCmd, _ = restoreCfg["command"].(string)
					vc.SnapshotURL, _ = restoreCfg["snapshot_url"].(string)
				}
				if gctx.GroveVolumes == nil {
					gctx.GroveVolumes = make(map[string]groveVolumeConfig)
				}
				gctx.GroveVolumes[svcName+"/"+volName] = vc
			}
		}
	}

	// Marshal grove context to a generic map
	gctxBytes, err := json.Marshal(gctx)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal grove context: %w", err)
	}
	payload := make(map[string]interface{})
	if err := json.Unmarshal(gctxBytes, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal grove context to map: %w", err)
	}

	// Merge user vars (overrides grove defaults like env_name)
	if vars, ok := req.Config["vars"].(map[string]interface{}); ok {
		for k, v := range vars {
			payload[k] = v
		}
	}

	// Inject image URIs from build step
	for k, v := range imageVars {
		payload[k] = v
	}

	// Inject shared infrastructure outputs
	if len(sharedOutputs) > 0 {
		payload["shared"] = sharedOutputs
	}

	return payload, nil
}

// writeTfVars writes the combined variables payload to grove_context.auto.tfvars.json.
func writeTfVars(stateDir string, payload map[string]interface{}) (string, error) {
	varsPath := filepath.Join(stateDir, "grove_context.auto.tfvars.json")
	varsBytes, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal tfvars: %w", err)
	}
	if err := os.WriteFile(varsPath, varsBytes, 0644); err != nil {
		return "", fmt.Errorf("failed to write tfvars: %w", err)
	}
	return varsPath, nil
}

// writeBackendOverride writes _grove_backend_override.tf.json to the module directory
// for GCS backend configuration. Returns the file path (for cleanup) or empty string for local backend.
func writeBackendOverride(moduleDir string, bc backendConfig) (string, error) {
	if bc.Type != "gcs" {
		return "", nil
	}
	overridePath := filepath.Join(moduleDir, "_grove_backend_override.tf.json")
	content := map[string]interface{}{
		"terraform": map[string]interface{}{
			"backend": map[string]interface{}{
				"gcs": map[string]interface{}{},
			},
		},
	}
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal backend override: %w", err)
	}
	if err := os.WriteFile(overridePath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write backend override: %w", err)
	}
	return overridePath, nil
}

// buildInitArgs returns the terraform init arguments based on backend config.
func buildInitArgs(bc backendConfig) []string {
	args := []string{"init", "-input=false", "-reconfigure"}
	if bc.Type == "gcs" {
		args = append(args,
			"-backend-config=bucket="+bc.Bucket,
			"-backend-config=prefix="+bc.Prefix,
		)
	}
	return args
}

// buildImages builds and pushes Docker images, returning a map of image_<key> -> fullURI.
func (m *Manager) buildImages(ctx context.Context, req coreenv.EnvRequest) (map[string]string, error) {
	images, ok := req.Config["images"].(map[string]interface{})
	if !ok || len(images) == 0 {
		return nil, nil
	}

	result := make(map[string]string)
	worktree := req.Workspace.Name
	stateDir := req.EffectiveStateDir()

	// Create logs directory
	logsDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	for key, imgCfgRaw := range images {
		imgCfg, ok := imgCfgRaw.(map[string]interface{})
		if !ok {
			continue
		}

		contextPath, _ := imgCfg["context"].(string)
		registry, _ := imgCfg["registry"].(string)
		dockerfile, _ := imgCfg["dockerfile"].(string)

		if contextPath == "" || registry == "" {
			return nil, fmt.Errorf("image %q requires context and registry", key)
		}

		// Resolve context path relative to workspace
		if !filepath.IsAbs(contextPath) {
			contextPath = filepath.Join(req.Workspace.Path, contextPath)
		}

		// Generate unique tag
		tag := fmt.Sprintf("grove-%s-%d", worktree, time.Now().Unix())
		fullURI := registry + ":" + tag

		// Build
		buildArgs := []string{"build", "-t", fullURI}
		if dockerfile != "" {
			buildArgs = append(buildArgs, "-f", dockerfile)
		}
		buildArgs = append(buildArgs, contextPath)

		logFile := filepath.Join(logsDir, "build-"+key+".log")
		buildCmd := exec.CommandContext(ctx, "docker", buildArgs...)
		buildOutput, err := buildCmd.CombinedOutput()
		_ = os.WriteFile(logFile, buildOutput, 0644)
		if err != nil {
			return nil, fmt.Errorf("docker build failed for %q: %w\nSee log: %s", key, err, logFile)
		}

		// Push
		pushCmd := exec.CommandContext(ctx, "docker", "push", fullURI)
		pushOutput, err := pushCmd.CombinedOutput()
		_ = os.WriteFile(logFile, append(buildOutput, pushOutput...), 0644)
		if err != nil {
			return nil, fmt.Errorf("docker push failed for %q: %w\nSee log: %s", key, err, logFile)
		}

		result["image_"+key] = fullURI
		m.logger.WithField("image", key).Infof("Built and pushed %s", fullURI)
	}

	return result, nil
}

// fetchSharedOutputs reads Terraform outputs from a shared infrastructure project's remote state.
// It creates a temporary directory, configures the backend, runs init + output, and returns the values.
func (m *Manager) fetchSharedOutputs(ctx context.Context, sharedCfg map[string]interface{}) (map[string]interface{}, error) {
	bucket, _ := sharedCfg["state_bucket"].(string)
	prefix, _ := sharedCfg["state_prefix"].(string)
	backend, _ := sharedCfg["state_backend"].(string)

	if backend != "gcs" || bucket == "" || prefix == "" {
		return nil, fmt.Errorf("shared backend config requires state_backend=gcs, state_bucket, and state_prefix")
	}

	tmpDir, err := os.MkdirTemp("", "grove-shared-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for shared outputs: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Write backend override to temp dir
	overrideContent := map[string]interface{}{
		"terraform": map[string]interface{}{
			"backend": map[string]interface{}{
				"gcs": map[string]interface{}{},
			},
		},
	}
	overrideBytes, err := json.MarshalIndent(overrideContent, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal shared backend override: %w", err)
	}
	overridePath := filepath.Join(tmpDir, "_grove_backend_override.tf.json")
	if err := os.WriteFile(overridePath, overrideBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write shared backend override: %w", err)
	}

	// terraform init in temp dir
	initArgs := []string{"init", "-input=false", "-reconfigure",
		"-backend-config=bucket=" + bucket,
		"-backend-config=prefix=" + prefix,
	}
	initCmd := exec.CommandContext(ctx, "terraform", initArgs...)
	initCmd.Dir = tmpDir
	if output, err := initCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform init for shared outputs failed: %w\nOutput: %s", err, string(output))
	}

	// terraform output -json
	outputCmd := exec.CommandContext(ctx, "terraform", "output", "-json")
	outputCmd.Dir = tmpDir
	outputBytes, err := outputCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform output for shared infra failed: %w", err)
	}

	var rawOutputs map[string]tfOutput
	if err := json.Unmarshal(outputBytes, &rawOutputs); err != nil {
		return nil, fmt.Errorf("failed to parse shared terraform outputs: %w", err)
	}

	// Extract values (skip sensitive)
	result := make(map[string]interface{})
	for name, out := range rawOutputs {
		if !out.Sensitive {
			result[name] = out.Value
		}
	}

	m.logger.WithField("outputs", len(result)).Info("Fetched shared infrastructure outputs")
	return result, nil
}

// terraformUp provisions infrastructure via Terraform and optionally starts tunnels.
func (m *Manager) terraformUp(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("terraform provider requires a workspace")
	}
	worktree := req.Workspace.Name

	m.mu.Lock()
	if _, exists := m.envs[worktree]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("environment already running for worktree: %s", worktree)
	}

	runningEnv := &RunningEnv{
		Provider: "terraform",
		Worktree: worktree,
		Ports:    make(map[string]int),
	}
	m.envs[worktree] = runningEnv
	m.mu.Unlock()

	// Resolve the TF module path (relative to workspace root)
	modulePath, _ := req.Config["path"].(string)
	if modulePath == "" {
		modulePath = "./infra"
	}
	moduleAbs := filepath.Join(req.Workspace.Path, modulePath)
	stateDir := req.EffectiveStateDir()

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
	initCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))
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
	applyCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform apply failed: %w\nOutput: %s", err, string(output))
	}

	// Read terraform outputs to populate env vars and endpoints
	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
	}

	outputArgs := []string{"output", "-json"}
	if bc.Type == "local" {
		outputArgs = append(outputArgs, "-state="+filepath.Join(stateDir, "terraform.tfstate"))
	}
	outputCmd := exec.CommandContext(ctx, "terraform", outputArgs...)
	outputCmd.Dir = moduleAbs
	outputCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))
	outputBytes, err := outputCmd.Output()
	if err == nil && len(outputBytes) > 0 {
		var outputs map[string]tfOutput
		if err := json.Unmarshal(outputBytes, &outputs); err == nil {
			m.mapTerraformOutputs(outputs, req.Config, resp)
		}
	}

	// Start tunnels if configured
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

			if err := m.Tunnels.Start(context.Background(), worktree, tunnelName, cmdStr, port); err != nil {
				m.logger.WithError(err).Warnf("Failed to start tunnel %s", tunnelName)
				continue
			}

			if urlTemplate != "" {
				finalURL := strings.ReplaceAll(urlTemplate, "{{.AllocatedPort}}", fmt.Sprintf("%d", port))
				resp.EnvVars[localPortEnv] = finalURL
			} else if localPortEnv != "" {
				resp.EnvVars[localPortEnv] = fmt.Sprintf("%d", port)
			}
		}
	}

	return resp, nil
}

// terraformDown destroys Terraform-managed infrastructure and cleans up.
func (m *Manager) terraformDown(ctx context.Context, req coreenv.EnvRequest) (*coreenv.EnvResponse, error) {
	if req.Workspace == nil {
		return nil, fmt.Errorf("terraform provider requires a workspace")
	}
	worktree := req.Workspace.Name

	m.mu.Lock()
	_, exists := m.envs[worktree]
	if exists {
		delete(m.envs, worktree)
	}
	m.mu.Unlock()

	// Stop tunnels first
	m.Tunnels.StopAll(worktree)
	m.Ports.ReleaseAll(worktree)

	// Resolve module path
	modulePath, _ := req.Config["path"].(string)
	if modulePath == "" {
		modulePath = "./infra"
	}
	moduleAbs := filepath.Join(req.Workspace.Path, modulePath)
	stateDir := req.EffectiveStateDir()

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
			m.logger.WithError(err).Warn("failed to build tfvars for destroy, proceeding anyway")
		} else {
			if _, err := writeTfVars(stateDir, payload); err != nil {
				m.logger.WithError(err).Warn("failed to write tfvars for destroy, proceeding anyway")
			}
		}

		varsPath := filepath.Join(stateDir, "grove_context.auto.tfvars.json")

		// Write backend override for GCS
		overridePath, err := writeBackendOverride(moduleAbs, bc)
		if err != nil {
			m.logger.WithError(err).Warn("failed to write backend override for destroy")
		}
		if overridePath != "" {
			defer os.Remove(overridePath)
		}

		// terraform init (needed for backend config)
		initArgs := buildInitArgs(bc)
		initCmd := exec.CommandContext(ctx, "terraform", initArgs...)
		initCmd.Dir = moduleAbs
		initCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))
		if output, err := initCmd.CombinedOutput(); err != nil {
			m.logger.WithError(err).Warnf("terraform init for destroy failed: %s", string(output))
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
		cmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(stateDir, ".terraform"))
		if output, err := cmd.CombinedOutput(); err != nil {
			m.logger.WithError(err).Warnf("terraform destroy failed: %s", string(output))
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

// tfOutput represents a single Terraform output value.
type tfOutput struct {
	Value     interface{} `json:"value"`
	Type      interface{} `json:"type"`
	Sensitive bool        `json:"sensitive"`
}

// mapTerraformOutputs maps Terraform outputs to EnvResponse fields.
// If config contains "output_env_map", it maps output names to env var names.
// Otherwise, all non-sensitive string outputs are exported as uppercase env vars.
func (m *Manager) mapTerraformOutputs(outputs map[string]tfOutput, config map[string]interface{}, resp *coreenv.EnvResponse) {
	// Check for explicit mapping: output_env_map = { "db_url" = "DATABASE_URL", ... }
	if envMap, ok := config["output_env_map"].(map[string]interface{}); ok {
		for outputName, envNameRaw := range envMap {
			envName, ok := envNameRaw.(string)
			if !ok {
				continue
			}
			if out, exists := outputs[outputName]; exists {
				resp.EnvVars[envName] = fmt.Sprintf("%v", out.Value)
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

		// If the output looks like a URL, also add it as an endpoint
		if strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://") {
			resp.Endpoints = append(resp.Endpoints, val)
		}
	}
}
