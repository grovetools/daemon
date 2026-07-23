package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
)

func (s *Server) handleJobArtifacts(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	info := s.engine.Store().GetJob(jobID)
	if info == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if info.Origin != "" {
		http.Error(w, "cross-origin job artifacts are not local", http.StatusForbidden)
		return
	}
	if info.ID != jobID || info.PlanDir == "" {
		http.Error(w, "job identity or plan directory unavailable", http.StatusConflict)
		return
	}
	if info.Status != "completed" && info.Status != "failed" && info.Status != "cancelled" {
		http.Error(w, "job is not terminal", http.StatusConflict)
		return
	}
	bundle, err := buildArtifactBundle(info.PlanDir, jobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if info.Type == models.JobType("interactive_agent") || info.Type == models.JobType("headless_agent") || info.Type == models.JobType("isolated_agent") {
		if err := validateAgentArtifactContents(bundle); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bundle)
}

func validateAgentArtifactContents(bundle *models.ArtifactBundle) error {
	hasMetadata, hasReport, hasTranscript := false, false, false
	for _, entry := range bundle.Manifest.Files {
		switch entry.Path {
		case "metadata.json":
			hasMetadata = true
		case "final-report.md":
			hasReport = true
		case "transcript.jsonl":
			hasTranscript = true
		default:
			if strings.HasPrefix(entry.Path, "sessions/") && strings.HasSuffix(entry.Path, ".jsonl") {
				hasTranscript = true
			}
		}
	}
	if !hasMetadata || !hasReport || !hasTranscript {
		return fmt.Errorf("agent artifact publication is incomplete (metadata=%t final_report=%t transcript=%t); retry after archival finishes", hasMetadata, hasReport, hasTranscript)
	}
	return nil
}

func buildArtifactBundle(planDir, jobID string) (*models.ArtifactBundle, error) {
	if jobID == "" || filepath.Base(jobID) != jobID || jobID == "." || jobID == ".." {
		return nil, fmt.Errorf("invalid job identity %q", jobID)
	}
	artifactBase := filepath.Join(planDir, ".artifacts")
	baseInfo, err := os.Lstat(artifactBase)
	if err != nil {
		return nil, fmt.Errorf("artifact base unavailable: %w", err)
	}
	if baseInfo.Mode()&os.ModeSymlink != 0 || !baseInfo.IsDir() {
		return nil, fmt.Errorf("artifact base is not a real directory")
	}
	root := filepath.Join(artifactBase, jobID)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("artifact root unavailable: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, fmt.Errorf("artifact root is not a real directory")
	}

	bundle := &models.ArtifactBundle{Manifest: models.ArtifactManifest{
		SchemaVersion: models.ArtifactBundleSchemaVersion,
		JobID:         jobID,
		Files:         []models.ArtifactManifestEntry{},
	}, Files: []models.ArtifactFile{}}

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("artifact path escapes job root: %q", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact %q is a symlink", filepath.ToSlash(rel))
		}
		if entry.IsDir() {
			return nil
		}
		if len(bundle.Files) >= models.ArtifactBundleMaxFiles {
			return fmt.Errorf("artifact count exceeds %d", models.ArtifactBundleMaxFiles)
		}
		// Reject FIFOs/devices/sockets before open; opening a FIFO would block.
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			return fmt.Errorf("artifact %q is not a regular file", filepath.ToSlash(rel))
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return err
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return fmt.Errorf("artifact %q is not a regular file", filepath.ToSlash(rel))
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
			_ = f.Close()
			return fmt.Errorf("artifact %q has %d hard links", filepath.ToSlash(rel), stat.Nlink)
		}
		if info.Size() < 0 || info.Size() > models.ArtifactBundleMaxBytes-bundle.Manifest.TotalBytes {
			_ = f.Close()
			return fmt.Errorf("artifact bundle exceeds %d bytes", models.ArtifactBundleMaxBytes)
		}
		data, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) != info.Size() {
			return fmt.Errorf("artifact %q changed while publishing", filepath.ToSlash(rel))
		}
		name := filepath.ToSlash(rel)
		sum := sha256.Sum256(data)
		bundle.Manifest.Files = append(bundle.Manifest.Files, models.ArtifactManifestEntry{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		bundle.Files = append(bundle.Files, models.ArtifactFile{Path: name, Data: data})
		bundle.Manifest.TotalBytes += int64(len(data))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := coredaemon.ValidateArtifactBundle(bundle, "", jobID); err != nil {
		return nil, err
	}
	return bundle, nil
}

// handleSatelliteArtifactFetch is laptop-only. It forwards through the same
// pinned SSH/direct-streamlocal transport as satellite dispatch and overwrites
// origin identity after validating the untrusted guest response.
func (s *Server) handleSatelliteArtifactFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cm := s.satelliteCM.Load()
	if cm == nil {
		http.Error(w, "satellite transport unavailable", http.StatusServiceUnavailable)
		return
	}
	var req models.SatelliteArtifactFetchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Origin == "" || req.JobID == "" {
		http.Error(w, "origin and job_id are required", http.StatusBadRequest)
		return
	}
	client, err := coredaemon.NewRemoteClientWithDialer(func(context.Context) (net.Conn, error) {
		return cm.DialSatelliteSocket(req.Origin)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer client.Close()
	bundle, err := client.GetJobArtifacts(r.Context(), req.JobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if bundle.Manifest.Origin != "" {
		http.Error(w, "guest attempted to assert artifact origin", http.StatusBadGateway)
		return
	}
	bundle.Manifest.Origin = req.Origin
	if err := coredaemon.ValidateArtifactBundle(bundle, req.Origin, req.JobID); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bundle)
}
