package sync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

const ReturnManifestSchema = "grove.record-return/v1"

type ReturnOperation struct {
	Type         string    `json:"type"` // create, update, delete, move
	Workspace    string    `json:"workspace"`
	DocumentID   string    `json:"document_id"`
	Path         string    `json:"path"`
	PreviousPath string    `json:"previous_path,omitempty"`
	BaseHash     string    `json:"base_hash,omitempty"`
	HeadHash     string    `json:"head_hash,omitempty"`
	HeadVersion  int64     `json:"head_version,omitempty"`
	Mtime        time.Time `json:"mtime,omitzero"`
}

type ReturnManifest struct {
	Schema         string            `json:"schema"`
	OperationID    string            `json:"operation_id"`
	ServerEpoch    string            `json:"server_epoch"`
	Generation     string            `json:"generation"`
	CreatedAt      time.Time         `json:"created_at"`
	Workspaces     []string          `json:"workspaces"`
	Operations     []ReturnOperation `json:"operations"`
	ManifestSHA256 string            `json:"manifest_sha256"`
}

type ReturnEscrow struct {
	Manifest ReturnManifest    `json:"manifest"`
	Content  map[string][]byte `json:"content"` // document id -> exact server-head bytes
}

func newOperationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// BuildReturnManifest compares one coherent set of server snapshots with the
// laptop's tracked state. Its generation binds the epoch, every workspace
// cursor, and every server head. A caller must rebuild immediately before a
// destructive operation; equality of Generation is the TOCTOU interlock.
func BuildReturnManifest(ctx context.Context, client *Client, db *DB, workspaces []string) (ReturnManifest, error) {
	if client == nil || db == nil {
		return ReturnManifest{}, fmt.Errorf("sync client and database are required")
	}
	if len(workspaces) == 0 {
		return ReturnManifest{}, fmt.Errorf("at least one workspace is required")
	}
	ws := append([]string(nil), workspaces...)
	sort.Strings(ws)
	ws = compactStrings(ws)
	m := ReturnManifest{Schema: ReturnManifestSchema, ServerEpoch: client.ServerEpoch(), CreatedAt: time.Now().UTC(), Workspaces: ws}
	if m.ServerEpoch == "" {
		return ReturnManifest{}, fmt.Errorf("sync server did not advertise an epoch")
	}
	id, err := newOperationID()
	if err != nil {
		return ReturnManifest{}, err
	}
	m.OperationID = id
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "epoch\x00%s\x00", m.ServerEpoch)
	for _, name := range ws {
		snap, err := client.Snapshot(ctx, name)
		if err != nil {
			return ReturnManifest{}, fmt.Errorf("snapshot %s: %w", name, err)
		}
		sort.Slice(snap.Documents, func(i, j int) bool { return snap.Documents[i].ID < snap.Documents[j].ID })
		_, _ = fmt.Fprintf(h, "workspace\x00%s\x00%d\x00", name, snap.Cursor)
		local, err := db.ListDocuments(name)
		if err != nil {
			return ReturnManifest{}, err
		}
		for _, d := range local {
			_, _ = fmt.Fprintf(h, "local\x00%s\x00%s\x00%s\x00", d.DocumentID, d.Path, d.ContentHash)
		}
		byID := make(map[string]*Document, len(local))
		for _, d := range local {
			byID[d.DocumentID] = d
		}
		seen := map[string]bool{}
		for _, d := range snap.Documents {
			_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s\x00", d.ID, d.Path, d.Version, d.Hash)
			ld := byID[d.ID]
			seen[d.ID] = true
			op := ReturnOperation{Workspace: name, DocumentID: d.ID, Path: d.Path, HeadHash: d.Hash, HeadVersion: d.Version, Mtime: d.Mtime}
			switch {
			case ld == nil:
				op.Type = "create"
			case ld.Path != d.Path:
				op.Type, op.PreviousPath, op.BaseHash = "move", ld.Path, ld.ContentHash
			case ld.ContentHash != d.Hash:
				op.Type, op.BaseHash = "update", ld.ContentHash
			default:
				continue
			}
			m.Operations = append(m.Operations, op)
		}
		for _, d := range local {
			if !seen[d.DocumentID] {
				m.Operations = append(m.Operations, ReturnOperation{Type: "delete", Workspace: name, DocumentID: d.DocumentID, Path: d.Path, BaseHash: d.ContentHash})
			}
		}
	}
	m.Generation = hex.EncodeToString(h.Sum(nil))
	sort.Slice(m.Operations, func(i, j int) bool {
		a, b := m.Operations[i], m.Operations[j]
		if a.Workspace != b.Workspace {
			return a.Workspace < b.Workspace
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.DocumentID < b.DocumentID
	})
	m.ManifestSHA256 = manifestHash(m)
	return m, nil
}

func compactStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s != "" && (len(out) == 0 || out[len(out)-1] != s) {
			out = append(out, s)
		}
	}
	return out
}
func manifestHash(m ReturnManifest) string {
	m.ManifestSHA256 = ""
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func ValidateReviewedManifest(reviewed, current ReturnManifest) error {
	if err := reviewed.Validate(); err != nil {
		return err
	}
	if err := current.Validate(); err != nil {
		return err
	}
	if reviewed.Generation != current.Generation || reviewed.ServerEpoch != current.ServerEpoch || !reflect.DeepEqual(reviewed.Workspaces, current.Workspaces) || !reflect.DeepEqual(reviewed.Operations, current.Operations) {
		return fmt.Errorf("reviewed incoming manifest is stale; review the new generation")
	}
	return nil
}

func (m ReturnManifest) Validate() error {
	if m.Schema != ReturnManifestSchema || m.OperationID == "" || m.ServerEpoch == "" || len(m.Workspaces) == 0 {
		return fmt.Errorf("invalid record-return manifest identity")
	}
	if len(m.Generation) != 64 || m.ManifestSHA256 != manifestHash(m) {
		return fmt.Errorf("record-return manifest hash mismatch")
	}
	for _, op := range m.Operations {
		if op.Workspace == "" || op.DocumentID == "" || op.Path == "" {
			return fmt.Errorf("invalid return operation")
		}
		switch op.Type {
		case "create", "update", "delete", "move":
		default:
			return fmt.Errorf("invalid return operation type %q", op.Type)
		}
	}
	return nil
}

// WriteReturnEscrow fetches and verifies every required server-head blob, then
// atomically writes and fsyncs a self-contained escrow on the laptop. Delete
// operations intentionally carry no content.
func WriteReturnEscrow(ctx context.Context, client *Client, manifest ReturnManifest, dir string) (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("escrow directory is required")
	}
	escrow := ReturnEscrow{Manifest: manifest, Content: map[string][]byte{}}
	for _, op := range manifest.Operations {
		if op.Type == "delete" {
			continue
		}
		b, err := client.HistoryBlob(ctx, op.Workspace, op.DocumentID, op.HeadVersion)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != op.HeadHash {
			return "", fmt.Errorf("head hash mismatch for %s/%s", op.Workspace, op.Path)
		}
		escrow.Content[op.DocumentID] = b
	}
	b, err := json.MarshalIndent(escrow, "", "  ")
	if err != nil {
		return "", err
	}
	b = append(b, '\n')
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".record-return-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, manifest.OperationID+".json")
	if err = os.Rename(tmpName, path); err != nil {
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return "", err
	}
	if err = VerifyReturnEscrow(path, manifest.Generation); err != nil {
		return "", err
	}
	return path, nil
}

func VerifyReturnEscrow(path, generation string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var e ReturnEscrow
	if err = json.Unmarshal(b, &e); err != nil {
		return err
	}
	if err = e.Manifest.Validate(); err != nil {
		return err
	}
	if e.Manifest.Generation != generation {
		return fmt.Errorf("escrow generation is stale")
	}
	for _, op := range e.Manifest.Operations {
		if op.Type == "delete" {
			continue
		}
		data, ok := e.Content[op.DocumentID]
		if !ok {
			return fmt.Errorf("escrow content missing for %s", op.DocumentID)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != op.HeadHash {
			return fmt.Errorf("escrow content hash mismatch for %s", op.DocumentID)
		}
	}
	return nil
}
