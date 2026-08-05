package cmd

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

func TestLogFileDate(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"system-2026-07-01.log", "2026-07-01", true},
		{"workspace-2026-06-15.log", "2026-06-15", true},
		{"2026-01-02.log", "2026-01-02", true},
		{"system.log", "", false},
		{"system-2026-13-40.log", "", false}, // invalid date
		{"notes.txt", "", false},             // not a .log file
		{"x.log", "", false},                 // too short for a date
	}
	for _, c := range cases {
		got, ok := logFileDate(c.name)
		if ok != c.ok {
			t.Errorf("logFileDate(%q) ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got.Format("2006-01-02") != c.want {
			t.Errorf("logFileDate(%q) = %s, want %s", c.name, got.Format("2006-01-02"), c.want)
		}
	}
}

func TestLogFileExpired(t *testing.T) {
	now := mustDate(t, "2026-07-01").Add(10 * time.Hour) // mid-day
	const retention = 14
	oldMod := now.AddDate(0, 0, -30)

	cases := []struct {
		name    string
		modTime time.Time
		want    bool
		why     string
	}{
		{"system-2026-06-01.log", now, true, "well past retention by filename date"},
		{"system-2026-06-16.log", now, true, "one day past the cutoff"},
		{"system-2026-06-17.log", now, false, "exactly at the cutoff is kept"},
		{"system-2026-06-30.log", now, false, "inside retention"},
		{"system-2026-07-01.log", oldMod, false, "today's file is never expired (even with old mtime)"},
		{"system-2026-07-02.log", oldMod, false, "future-dated file is kept"},
		{"custom.log", oldMod, true, "undated name falls back to mtime"},
		{"custom.log", now, false, "undated recent file is kept"},
	}
	for _, c := range cases {
		if got := logFileExpired(c.name, c.modTime, now, retention); got != c.want {
			t.Errorf("logFileExpired(%q, mod=%s) = %v, want %v (%s)",
				c.name, c.modTime.Format("2006-01-02"), got, c.want, c.why)
		}
	}
}

func TestApplyDefaultDaemonMemoryLimit(t *testing.T) {
	var set int64
	if !applyDefaultDaemonMemoryLimit(func(string) string { return "" }, func(v int64) int64 { set = v; return 0 }) {
		t.Fatal("unset GOMEMLIMIT did not apply default")
	}
	if set != defaultDaemonMemoryLimit {
		t.Fatalf("memory limit=%d want=%d", set, defaultDaemonMemoryLimit)
	}
	set = 0
	if applyDefaultDaemonMemoryLimit(func(string) string { return "off" }, func(v int64) int64 { set = v; return 0 }) || set != 0 {
		t.Fatal("explicit GOMEMLIMIT was overridden")
	}
}

func TestRotateOversizedLogsCopyTruncatesActiveFile(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "system-2026-07-01.log")
	content := []byte("0123456789")
	if err := os.WriteFile(active, content, 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateOversizedLogs(dir, 5, time.Unix(1000, 0))
	if err != nil || rotated != 1 {
		t.Fatalf("rotateOversizedLogs=(%d,%v), want (1,nil)", rotated, err)
	}
	if info, err := os.Stat(active); err != nil || info.Size() != 0 {
		t.Fatalf("active log not truncated: info=%v err=%v", info, err)
	}
	parts, err := filepath.Glob(filepath.Join(dir, "system-2026-07-01-part-*.log.gz"))
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts=%v err=%v", parts, err)
	}
	f, err := os.Open(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not gzip: %v", err)
	}
	if got, err := io.ReadAll(gz); err != nil || string(got) != string(content) {
		t.Fatalf("archive=%q err=%v", got, err)
	}
	rotated, err = rotateOversizedLogs(dir, 5, time.Unix(1001, 0))
	if err != nil || rotated != 0 {
		t.Fatalf("part file was recursively rotated: (%d,%v)", rotated, err)
	}
}

func TestSweepOldLogs(t *testing.T) {
	now := mustDate(t, "2026-07-01").Add(10 * time.Hour)
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspaces", "grovetools")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(path string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	expired1 := filepath.Join(dir, "system-2026-05-01.log")
	expired2 := filepath.Join(wsDir, "workspace-2026-06-10.log")
	expiredUndated := filepath.Join(dir, "old-undated.log")
	expiredPartGz := filepath.Join(dir, "system-2026-05-01-part-20260501T120000.000000000.log.gz")
	keptToday := filepath.Join(dir, "system-2026-07-01.log")
	keptRecent := filepath.Join(dir, "system-2026-06-25.log")
	keptNotLog := filepath.Join(dir, "system-2026-05-01.txt")

	write(expired1, now)
	write(expired2, now)
	write(expiredUndated, now.AddDate(0, 0, -60))
	write(expiredPartGz, now.AddDate(0, 0, -60)) // undated suffix, expires by mtime
	write(keptToday, now)
	write(keptRecent, now)
	write(keptNotLog, now.AddDate(0, 0, -60))

	deleted, freed, err := sweepOldLogs(dir, 14, now)
	if err != nil {
		t.Fatalf("sweepOldLogs error: %v", err)
	}
	if deleted != 4 {
		t.Errorf("deleted = %d, want 4", deleted)
	}
	if freed != 8 { // 4 files x 2 bytes
		t.Errorf("freed = %d, want 8", freed)
	}
	for _, gone := range []string{expired1, expired2, expiredUndated, expiredPartGz} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been deleted", gone)
		}
	}
	for _, kept := range []string{keptToday, keptRecent, keptNotLog} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}

	// Missing dir is a silent no-op.
	deleted, freed, err = sweepOldLogs(filepath.Join(dir, "nope"), 14, now)
	if err != nil || deleted != 0 || freed != 0 {
		t.Errorf("missing dir: got (%d, %d, %v), want (0, 0, nil)", deleted, freed, err)
	}
}
