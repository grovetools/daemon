package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	coreenv "github.com/grovetools/core/pkg/env"
)

// groveContext is the set of standard variables injected into Terraform as auto.tfvars.json.
type groveContext struct {
	GroveEcosystem string                       `json:"grove_ecosystem"`
	GroveProject   string                       `json:"grove_project"`
	GroveWorktree  string                       `json:"grove_worktree"`
	GroveBranch    string                       `json:"grove_branch,omitempty"`
	GrovePlanDir   string                       `json:"grove_plan_dir"`
	GroveVolumes   map[string]groveVolumeConfig `json:"grove_volumes,omitempty"`
}

// groveVolumeConfig represents a volume's configuration passed to Terraform modules.
type groveVolumeConfig struct {
	Service      string `json:"service"`
	HostPath     string `json:"host_path"`
	Persist      bool   `json:"persist,omitempty"`
	SnapshotURL  string `json:"snapshot_url,omitempty"`
	RestoreCmd   string `json:"restore_command,omitempty"`
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

	// State file lives in the plan dir to keep the workspace clean
	stateFile := filepath.Join(req.PlanDir, "terraform.tfstate")

	// Write grove context as auto.tfvars.json into the plan dir
	gctx := groveContext{
		GroveProject:  req.Workspace.Name,
		GroveWorktree: worktree,
		GrovePlanDir:  req.PlanDir,
	}
	if req.Workspace.ParentEcosystemPath != "" {
		gctx.GroveEcosystem = filepath.Base(req.Workspace.ParentEcosystemPath)
	}
	// Extract branch from config if provided
	if branch, ok := req.Config["branch"].(string); ok {
		gctx.GroveBranch = branch
	}

	// Collect volume configs from services and pass through to Terraform
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

				// Pass restore config if present
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

	varsPath := filepath.Join(req.PlanDir, "grove_context.auto.tfvars.json")
	varsBytes, err := json.MarshalIndent(gctx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal grove context: %w", err)
	}
	if err := os.WriteFile(varsPath, varsBytes, 0644); err != nil {
		return nil, fmt.Errorf("failed to write tfvars: %w", err)
	}

	// Collect any extra var files from config
	var varFileArgs []string
	varFileArgs = append(varFileArgs, "-var-file="+varsPath)
	if extraVarFile, ok := req.Config["var_file"].(string); ok && extraVarFile != "" {
		abs := filepath.Join(req.Workspace.Path, extraVarFile)
		varFileArgs = append(varFileArgs, "-var-file="+abs)
	}

	// terraform init
	initArgs := []string{"init", "-input=false"}
	initCmd := exec.CommandContext(ctx, "terraform", initArgs...)
	initCmd.Dir = moduleAbs
	initCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(req.PlanDir, ".terraform"))
	if output, err := initCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w\nOutput: %s", err, string(output))
	}

	// terraform apply
	applyArgs := []string{"apply", "-auto-approve", "-input=false", "-state=" + stateFile}
	applyArgs = append(applyArgs, varFileArgs...)
	applyCmd := exec.CommandContext(ctx, "terraform", applyArgs...)
	applyCmd.Dir = moduleAbs
	applyCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(req.PlanDir, ".terraform"))
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("terraform apply failed: %w\nOutput: %s", err, string(output))
	}

	// Read terraform outputs to populate env vars and endpoints
	resp := &coreenv.EnvResponse{
		Status:  "running",
		EnvVars: make(map[string]string),
	}

	outputCmd := exec.CommandContext(ctx, "terraform", "output", "-json", "-state="+stateFile)
	outputCmd.Dir = moduleAbs
	outputCmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(req.PlanDir, ".terraform"))
	outputBytes, err := outputCmd.Output()
	if err == nil && len(outputBytes) > 0 {
		var outputs map[string]tfOutput
		if err := json.Unmarshal(outputBytes, &outputs); err == nil {
			// Map outputs to env vars using the output_env_map config
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
	stateFile := filepath.Join(req.PlanDir, "terraform.tfstate")

	// Only run destroy if state file exists
	if _, err := os.Stat(stateFile); err == nil {
		varsPath := filepath.Join(req.PlanDir, "grove_context.auto.tfvars.json")

		destroyArgs := []string{"destroy", "-auto-approve", "-input=false", "-state=" + stateFile}
		if _, err := os.Stat(varsPath); err == nil {
			destroyArgs = append(destroyArgs, "-var-file="+varsPath)
		}
		if extraVarFile, ok := req.Config["var_file"].(string); ok && extraVarFile != "" {
			abs := filepath.Join(req.Workspace.Path, extraVarFile)
			destroyArgs = append(destroyArgs, "-var-file="+abs)
		}

		cmd := exec.CommandContext(ctx, "terraform", destroyArgs...)
		cmd.Dir = moduleAbs
		cmd.Env = append(os.Environ(), "TF_DATA_DIR="+filepath.Join(req.PlanDir, ".terraform"))
		if output, err := cmd.CombinedOutput(); err != nil {
			m.logger.WithError(err).Warnf("terraform destroy failed: %s", string(output))
		}
	}

	// Cleanup plan dir artifacts
	for _, f := range []string{"terraform.tfstate", "terraform.tfstate.backup", "grove_context.auto.tfvars.json"} {
		_ = os.Remove(filepath.Join(req.PlanDir, f))
	}
	_ = os.RemoveAll(filepath.Join(req.PlanDir, ".terraform"))

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
