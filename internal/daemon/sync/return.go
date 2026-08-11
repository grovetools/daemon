package sync

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const ReturnManifestSchema = "grove.record-return/v1"

type ReturnOperation struct {
	Type         string    `json:"type"` // create, update, delete, move
	Notespace    string    `json:"notespace_id"`
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
	Notespaces     []string          `json:"notespace_ids"`
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
// laptop's tracked state. Its generation binds the epoch, every notespace
// cursor, and every server head. A caller must rebuild immediately before a
// destructive operation; equality of Generation is the TOCTOU interlock.
func BuildReturnManifest(ctx context.Context, client *Client, db *DB, notespaces []string) (ReturnManifest, error) {
	if client == nil || db == nil {
		return ReturnManifest{}, fmt.Errorf("sync client and database are required")
	}
	if len(notespaces) == 0 {
		return ReturnManifest{}, fmt.Errorf("at least one notespace is required")
	}
	ws := append([]string(nil), notespaces...)
	sort.Strings(ws)
	ws = compactStrings(ws)
	m := ReturnManifest{Schema: ReturnManifestSchema, ServerEpoch: client.ServerEpoch(), CreatedAt: time.Now().UTC(), Notespaces: ws}
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
		_, _ = fmt.Fprintf(h, "notespace\x00%s\x00%d\x00", name, snap.Cursor)
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
			op := ReturnOperation{Notespace: name, DocumentID: d.ID, Path: d.Path, HeadHash: d.Hash, HeadVersion: d.Version, Mtime: d.Mtime}
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
			if !seen[d.DocumentID] && d.LastSyncedVersion > 0 {
				// A version-zero row was never acknowledged by the server. Its
				// absence from the server snapshot is therefore not an incoming
				// deletion. This commonly occurs when a laptop file is archived
				// before its original identity first syncs: the archived path is
				// present under a new identity while the stale original row remains.
				m.Operations = append(m.Operations, ReturnOperation{Type: "delete", Notespace: name, DocumentID: d.DocumentID, Path: d.Path, BaseHash: d.ContentHash})
			}
		}
	}
	m.Generation = hex.EncodeToString(h.Sum(nil))
	sort.Slice(m.Operations, func(i, j int) bool {
		a, b := m.Operations[i], m.Operations[j]
		if a.Notespace != b.Notespace {
			return a.Notespace < b.Notespace
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
	if reviewed.Generation != current.Generation || reviewed.ServerEpoch != current.ServerEpoch || !reflect.DeepEqual(reviewed.Notespaces, current.Notespaces) || !reflect.DeepEqual(reviewed.Operations, current.Operations) {
		return fmt.Errorf("reviewed incoming manifest is stale; review the new generation")
	}
	return nil
}

func (m ReturnManifest) Validate() error {
	if m.Schema != ReturnManifestSchema || m.OperationID == "" || m.ServerEpoch == "" || len(m.Notespaces) == 0 {
		return fmt.Errorf("invalid record-return manifest identity")
	}
	if len(m.Generation) != 64 || !validHexHash(m.Generation) || m.ManifestSHA256 != manifestHash(m) {
		return fmt.Errorf("record-return manifest hash mismatch")
	}
	notespaceSet := make(map[string]bool, len(m.Notespaces))
	for i, ws := range m.Notespaces {
		if ws == "" || notespaceSet[ws] || (i > 0 && m.Notespaces[i-1] > ws) {
			return fmt.Errorf("invalid record-return notespace set")
		}
		notespaceSet[ws] = true
	}
	documents := make(map[string]bool, len(m.Operations))
	for _, op := range m.Operations {
		if op.Notespace == "" || !notespaceSet[op.Notespace] || op.DocumentID == "" || documents[op.DocumentID] || validReturnPath(op.Path) != nil {
			return fmt.Errorf("invalid return operation")
		}
		documents[op.DocumentID] = true
		switch op.Type {
		case "create":
			if !validHexHash(op.HeadHash) || op.HeadVersion <= 0 || op.BaseHash != "" || op.PreviousPath != "" {
				return fmt.Errorf("invalid create operation")
			}
		case "update":
			if !validHexHash(op.BaseHash) || !validHexHash(op.HeadHash) || op.HeadVersion <= 0 || op.PreviousPath != "" {
				return fmt.Errorf("invalid update operation")
			}
		case "delete":
			if !validHexHash(op.BaseHash) || op.HeadHash != "" || op.PreviousPath != "" {
				return fmt.Errorf("invalid delete operation")
			}
		case "move":
			if !validHexHash(op.BaseHash) || !validHexHash(op.HeadHash) || op.HeadVersion <= 0 || validReturnPath(op.PreviousPath) != nil {
				return fmt.Errorf("invalid move operation")
			}
		default:
			return fmt.Errorf("invalid return operation type %q", op.Type)
		}
	}
	return nil
}

func validHexHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
		b, err := client.HistoryBlob(ctx, op.Notespace, op.DocumentID, op.HeadVersion)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != op.HeadHash {
			return "", fmt.Errorf("head hash mismatch for %s/%s", op.Notespace, op.Path)
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

func VerifyReturnEscrow(escrowPath, generation string) error {
	_, err := ReadReturnEscrow(escrowPath, generation)
	return err
}

// ReadReturnEscrow strictly decodes and verifies a generation-bound escrow.
// Content is returned only to the daemon apply path and is never included in
// its API response.
func ReadReturnEscrow(escrowPath, generation string) (ReturnEscrow, error) {
	f, err := os.Open(escrowPath)
	if err != nil {
		return ReturnEscrow{}, err
	}
	defer f.Close()
	var e ReturnEscrow
	dec := json.NewDecoder(io.LimitReader(f, 512<<20))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&e); err != nil {
		return ReturnEscrow{}, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return ReturnEscrow{}, fmt.Errorf("escrow has trailing data")
	}
	if err = e.Manifest.Validate(); err != nil {
		return ReturnEscrow{}, err
	}
	if e.Manifest.Generation != generation {
		return ReturnEscrow{}, fmt.Errorf("escrow generation is stale")
	}
	expectedContent := 0
	for _, op := range e.Manifest.Operations {
		if op.Type == "delete" {
			continue
		}
		expectedContent++
		data, ok := e.Content[op.DocumentID]
		if !ok {
			return ReturnEscrow{}, fmt.Errorf("escrow content missing for %s", op.DocumentID)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != op.HeadHash {
			return ReturnEscrow{}, fmt.Errorf("escrow content hash mismatch for %s", op.DocumentID)
		}
	}
	if len(e.Content) != expectedContent {
		return ReturnEscrow{}, fmt.Errorf("escrow contains unbound content")
	}
	return e, nil
}

// ReturnApplyCounts is metadata-only accounting for a batch adoption.
type ReturnApplyCounts struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Move   int `json:"move"`
	Delete int `json:"delete"`
	Noop   int `json:"noop"`
}

// ReturnApplyOptions provides the configured notespace roots and a state
// reconciliation hook. Reconcile runs after filesystem commit while backups
// still exist; an error rolls the complete filesystem batch back.
type ReturnApplyOptions struct {
	NotespaceRoots map[string]string
	Reconcile      func(ReturnEscrow) error
	BeforeCommit   func(index int, op ReturnOperation) error // test/fault-injection seam
}

type preparedReturnOp struct {
	op          ReturnOperation
	src, dst    string
	stage       string
	backup      string
	mode        os.FileMode
	noop        bool
	committed   bool
	createdDirs []string
}

func validReturnPath(name string) error {
	windowsAbs := len(name) >= 3 && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) && name[1] == ':' && name[2] == '/'
	if name == "" || windowsAbs || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || path.IsAbs(name) || filepath.IsAbs(name) {
		return fmt.Errorf("unsafe return path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe return path %q", name)
	}
	return nil
}

func secureReturnPath(root, rel string, allowMissingLeaf bool) (string, error) {
	if err := validReturnPath(rel); err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	ri, err := os.Lstat(absRoot)
	if err != nil || !ri.IsDir() || ri.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("notespace root %q is not a real directory", root)
	}
	dst := filepath.Join(absRoot, filepath.FromSlash(rel))
	if r, err := filepath.Rel(absRoot, dst); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes notespace root: %q", rel)
	}
	cur := absRoot
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, statErr := os.Lstat(cur)
		if statErr != nil {
			if os.IsNotExist(statErr) && (allowMissingLeaf || i < len(parts)-1) {
				continue
			}
			return "", statErr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink is not allowed in return path %q", rel)
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return "", fmt.Errorf("non-directory parent in return path %q", rel)
		}
	}
	return dst, nil
}

func fileSHA256(filename string) (string, os.FileMode, error) {
	fi, err := os.Lstat(filename)
	if err != nil {
		return "", 0, err
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("unsupported local file type at %s", filename)
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Mode().Perm(), nil
}

func mkdirParents(root, dir string) ([]string, error) {
	var missing []string
	for cur := dir; cur != root; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, cur)
	}
	created := make([]string, 0, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			return created, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// ApplyReturnEscrow validates every path and local precondition, stages all
// payloads, then commits with sibling backups and complete rollback on error.
func ApplyReturnEscrow(escrowPath, generation string, opts ReturnApplyOptions) (counts ReturnApplyCounts, err error) {
	e, err := ReadReturnEscrow(escrowPath, generation)
	if err != nil {
		return counts, err
	}
	prepared := make([]preparedReturnOp, 0, len(e.Manifest.Operations))
	seen := map[string]bool{}
	for _, op := range e.Manifest.Operations {
		root, ok := opts.NotespaceRoots[op.Notespace]
		if !ok || root == "" {
			return counts, fmt.Errorf("no configured laptop root for notespace %q", op.Notespace)
		}
		dst, err := secureReturnPath(root, op.Path, true)
		if err != nil {
			return counts, err
		}
		key := op.Notespace + "\x00" + op.Path
		if seen[key] {
			return counts, fmt.Errorf("multiple return operations target %s/%s", op.Notespace, op.Path)
		}
		seen[key] = true
		p := preparedReturnOp{op: op, dst: dst, mode: 0o644}
		switch op.Type {
		case "create":
			if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
				if statErr == nil {
					return counts, fmt.Errorf("create destination exists: %s/%s", op.Notespace, op.Path)
				}
				return counts, statErr
			}
			counts.Create++
		case "update", "delete":
			// An adopted deletion whose target is already absent is an
			// idempotent no-op rather than a failure. It is safe here because
			// the manifest was rebuilt against this exact local tracked state
			// (the generation interlock) and the caller refuses to apply while
			// any adopted path still has an unpushed outbox entry, so an
			// absent path cannot be a local edit we would be discarding.
			// Reconcile still retires the identity row.
			if op.Type == "delete" {
				if _, statErr := os.Lstat(dst); os.IsNotExist(statErr) {
					p.noop = true
					counts.Noop++
					break
				}
			}
			hash, mode, hashErr := fileSHA256(dst)
			if hashErr != nil {
				return counts, fmt.Errorf("%s precondition for %s/%s: %w", op.Type, op.Notespace, op.Path, hashErr)
			}
			if hash != op.BaseHash {
				return counts, fmt.Errorf("local hash drift for %s/%s", op.Notespace, op.Path)
			}
			p.mode = mode
			if op.Type == "update" {
				counts.Update++
			} else {
				counts.Delete++
			}
		case "move":
			if err := validReturnPath(op.PreviousPath); err != nil {
				return counts, err
			}
			p.src, err = secureReturnPath(root, op.PreviousPath, false)
			if err != nil {
				return counts, err
			}
			hash, mode, hashErr := fileSHA256(p.src)
			if hashErr != nil {
				return counts, fmt.Errorf("move source precondition for %s/%s: %w", op.Notespace, op.PreviousPath, hashErr)
			}
			if hash != op.BaseHash {
				return counts, fmt.Errorf("local hash drift for %s/%s", op.Notespace, op.PreviousPath)
			}
			if _, statErr := os.Lstat(dst); !os.IsNotExist(statErr) {
				if statErr == nil {
					return counts, fmt.Errorf("move destination exists: %s/%s", op.Notespace, op.Path)
				}
				return counts, statErr
			}
			p.mode = mode
			counts.Move++
		}
		prepared = append(prepared, p)
	}

	// Stage every payload only after all preconditions have passed.
	defer func() {
		for _, p := range prepared {
			if p.stage != "" {
				_ = os.Remove(p.stage)
			}
			if p.backup != "" {
				_ = os.Remove(p.backup)
			}
		}
	}()
	for i := range prepared {
		p := &prepared[i]
		if p.op.Type == "delete" {
			continue
		}
		root := opts.NotespaceRoots[p.op.Notespace]
		f, createErr := os.CreateTemp(root, ".record-return-stage-*")
		if createErr != nil {
			return counts, createErr
		}
		p.stage = f.Name()
		data := e.Content[p.op.DocumentID]
		if createErr = f.Chmod(p.mode); createErr == nil {
			_, createErr = f.Write(data)
		}
		if createErr == nil {
			createErr = f.Sync()
		}
		if closeErr := f.Close(); createErr == nil {
			createErr = closeErr
		}
		if createErr != nil {
			return counts, createErr
		}
	}

	rollback := func(last int) {
		for i := last; i >= 0; i-- {
			p := &prepared[i]
			if p.committed {
				_ = os.Remove(p.dst)
			}
			if p.backup != "" {
				restore := p.dst
				if p.op.Type == "move" {
					restore = p.src
				}
				_ = os.Rename(p.backup, restore)
				p.backup = ""
			}
			for j := len(p.createdDirs) - 1; j >= 0; j-- {
				_ = os.Remove(p.createdDirs[j])
			}
		}
	}
	for i := range prepared {
		p := &prepared[i]
		if opts.BeforeCommit != nil {
			if hookErr := opts.BeforeCommit(i, p.op); hookErr != nil {
				rollback(i - 1)
				return counts, hookErr
			}
		}
		root := opts.NotespaceRoots[p.op.Notespace]
		if _, secErr := secureReturnPath(root, p.op.Path, true); secErr != nil {
			rollback(i - 1)
			return counts, secErr
		}
		if p.op.Type == "move" {
			if _, secErr := secureReturnPath(root, p.op.PreviousPath, false); secErr != nil {
				rollback(i - 1)
				return counts, secErr
			}
		}
		// A no-op deletion touches nothing, so it needs no backup, no staged
		// payload, and no rollback entry — only proof that it is still absent.
		if p.noop {
			if _, statErr := os.Lstat(p.dst); !os.IsNotExist(statErr) {
				rollback(i - 1)
				return counts, fmt.Errorf("delete no-op precondition changed before commit: %s/%s", p.op.Notespace, p.op.Path)
			}
			continue
		}
		// Recheck local OCC immediately before touching this operation. If an
		// earlier operation was already committed, rollback restores it.
		switch p.op.Type {
		case "create":
			if _, statErr := os.Lstat(p.dst); !os.IsNotExist(statErr) {
				rollback(i - 1)
				return counts, fmt.Errorf("create destination changed before commit: %s/%s", p.op.Notespace, p.op.Path)
			}
		case "update", "delete":
			hash, _, hashErr := fileSHA256(p.dst)
			if hashErr != nil || hash != p.op.BaseHash {
				rollback(i - 1)
				return counts, fmt.Errorf("local precondition changed before commit: %s/%s", p.op.Notespace, p.op.Path)
			}
		case "move":
			hash, _, hashErr := fileSHA256(p.src)
			_, dstErr := os.Lstat(p.dst)
			if hashErr != nil || hash != p.op.BaseHash || !os.IsNotExist(dstErr) {
				rollback(i - 1)
				return counts, fmt.Errorf("move precondition changed before commit: %s/%s", p.op.Notespace, p.op.Path)
			}
		}
		// A surviving delete's parent necessarily exists (its target does), and
		// a no-op delete already returned above, so this only ever materializes
		// parents for an incoming file.
		p.createdDirs, err = mkdirParents(root, filepath.Dir(p.dst))
		if err != nil {
			rollback(i)
			return counts, err
		}
		if p.op.Type == "update" || p.op.Type == "delete" {
			candidate := filepath.Join(filepath.Dir(p.dst), ".record-return-backup-"+e.Manifest.OperationID+"-"+filepath.Base(p.dst))
			if _, backupErr := os.Lstat(candidate); !os.IsNotExist(backupErr) {
				rollback(i)
				return counts, fmt.Errorf("return backup path is occupied: %s", candidate)
			}
			p.backup = candidate
			err = os.Rename(p.dst, p.backup)
		} else if p.op.Type == "move" {
			candidate := filepath.Join(filepath.Dir(p.src), ".record-return-backup-"+e.Manifest.OperationID+"-"+filepath.Base(p.src))
			if _, backupErr := os.Lstat(candidate); !os.IsNotExist(backupErr) {
				rollback(i)
				return counts, fmt.Errorf("return backup path is occupied: %s", candidate)
			}
			p.backup = candidate
			err = os.Rename(p.src, p.backup)
		}
		if err == nil && p.op.Type != "delete" {
			// Clear stage only on success; a failed rename leaves the staged
			// temp file in place for the deferred cleanup to remove.
			if err = os.Rename(p.stage, p.dst); err == nil {
				p.stage = ""
			}
		}
		if err != nil {
			rollback(i)
			return counts, err
		}
		p.committed = true
		if p.op.Type != "delete" && !p.op.Mtime.IsZero() {
			_ = os.Chtimes(p.dst, p.op.Mtime, p.op.Mtime)
		}
	}
	if opts.Reconcile != nil {
		if err = opts.Reconcile(e); err != nil {
			rollback(len(prepared) - 1)
			return counts, fmt.Errorf("reconcile adopted generation: %w", err)
		}
	}
	for i := range prepared {
		if prepared[i].backup != "" {
			_ = os.Remove(prepared[i].backup)
			prepared[i].backup = ""
		}
	}
	return counts, nil
}
