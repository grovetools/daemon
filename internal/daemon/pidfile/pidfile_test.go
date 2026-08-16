package pidfile

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperEnv makes a test binary re-exec itself as a pidfile contender. The
// cross-process test below is the whole point of this file: flock is arbitrated
// per open file description, so goroutines in one process would exercise the
// same code path but not the same kernel arbitration that two `groved start`
// invocations hit.
const (
	helperEnv     = "GROVE_PIDFILE_TEST_PATH"
	helperHoldEnv = "GROVE_PIDFILE_TEST_HOLD"
)

// TestMain doubles as the contender process: with helperEnv set it acquires the
// pidfile, holds it, and exits 0 on success / 3 on a lost race.
func TestMain(m *testing.M) {
	path := os.Getenv(helperEnv)
	if path == "" {
		os.Exit(m.Run())
	}

	hold, _ := time.ParseDuration(os.Getenv(helperHoldEnv))
	if err := Acquire(path); err != nil {
		fmt.Fprintf(os.Stderr, "acquire: %v\n", err)
		if IsAlreadyRunning(err) {
			os.Exit(3)
		}
		os.Exit(4)
	}
	fmt.Printf("acquired %d\n", os.Getpid())
	time.Sleep(hold)
	_ = Release(path)
	os.Exit(0)
}

// contender starts this test binary as a separate process racing for path.
func contender(t *testing.T, path string, hold time.Duration, wait string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestNothingToRun") //nolint:gosec // G204: re-execs this test binary
	cmd.Env = append(os.Environ(),
		helperEnv+"="+path,
		helperHoldEnv+"="+hold.String(),
		AcquireWaitEnv+"="+wait,
	)
	cmd.Stdout = new(strings.Builder)
	cmd.Stderr = new(strings.Builder)
	return cmd
}

// TestConcurrentStartersElectExactlyOneWinner is the regression test for the
// shadow daemon: two `groved start` invocations landing in the same instant
// with no pidfile present used to BOTH survive Acquire, and the loser then ran
// a complete daemon workload against a socket inode the winner had replaced.
func TestConcurrentStartersElectExactlyOneWinner(t *testing.T) {
	const contenders = 8
	path := filepath.Join(t.TempDir(), "groved.pid")

	cmds := make([]*exec.Cmd, contenders)
	for i := range cmds {
		// Losers must not linger: the wait exists for an upgrade handoff, and
		// there is no handoff here.
		cmds[i] = contender(t, path, 2*time.Second, "200ms")
	}

	// Start them as close together as the scheduler allows, which is what the
	// fleet restart does when every client auto-starts groved at once.
	var wg sync.WaitGroup
	errs := make([]error, contenders)
	for i, cmd := range cmds {
		wg.Add(1)
		go func(i int, cmd *exec.Cmd) {
			defer wg.Done()
			if err := cmd.Start(); err != nil {
				errs[i] = err
				return
			}
			errs[i] = cmd.Wait()
		}(i, cmd)
	}
	wg.Wait()

	var winners, losers int
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case exitCode(err) == 3:
			losers++
		default:
			t.Errorf("contender %d failed unexpectedly: %v\nstderr: %s", i, err, cmds[i].Stderr)
		}
	}

	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d (losers: %d)", winners, losers)
	}
	if losers != contenders-1 {
		t.Errorf("expected %d losers, got %d", contenders-1, losers)
	}
}

// TestAcquireRejectsLiveHolder covers the steady state: a running daemon holds
// the lock, and a second starter is told so instead of proceeding to boot.
func TestAcquireRejectsLiveHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")

	holder := contender(t, path, 3*time.Second, "0")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill() }()
	waitForPID(t, path)

	err := AcquireWait(path, 200*time.Millisecond)
	if !IsAlreadyRunning(err) {
		t.Fatalf("expected AlreadyRunningError, got %v", err)
	}
	var are *AlreadyRunningError
	if !errors.As(err, &are) || are.PID != holder.Process.Pid {
		t.Fatalf("expected the holder's pid %d in the error, got %v", holder.Process.Pid, err)
	}
}

// TestAcquireWaitsOutADrainingPredecessor is the graceful-upgrade half of the
// contract: `groved upgrade` starts the successor while the predecessor is
// still tearing down, and the successor must take the pidfile the moment it
// lets go rather than refuse to start.
func TestAcquireWaitsOutADrainingPredecessor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")

	predecessor := contender(t, path, 300*time.Millisecond, "0")
	if err := predecessor.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPID(t, path)

	start := time.Now()
	if err := AcquireWait(path, 5*time.Second); err != nil {
		t.Fatalf("successor should have taken the pidfile after the predecessor left: %v", err)
	}
	defer func() { _ = Release(path) }()

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("handoff took %s; expected it to complete as soon as the predecessor exited", elapsed)
	}
	if pid, _ := Read(path); pid != os.Getpid() {
		t.Errorf("pidfile names %d, want this process (%d)", pid, os.Getpid())
	}
	_ = predecessor.Wait()
}

// TestAcquireTakesOverStalePidfile keeps the pre-existing behaviour that a
// crashed daemon's leftovers never block a restart. With flock this needs no
// stale detection at all — the dead process's lock died with it — but the file
// content check must not resurrect the old rule either.
func TestAcquireTakesOverStalePidfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")

	// A PID that is certainly not alive: spawn true(1) and reap it.
	dead := exec.Command("sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(dead.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AcquireWait(path, 0); err != nil {
		t.Fatalf("stale pidfile should not block acquisition: %v", err)
	}
	defer func() { _ = Release(path) }()

	if pid, _ := Read(path); pid != os.Getpid() {
		t.Errorf("pidfile names %d, want this process (%d)", pid, os.Getpid())
	}
}

// TestReleaseLeavesAnotherProcessesPidfileAlone protects the upgrade overlap:
// the predecessor's teardown must not delete the successor's pidfile, which
// would leave a live daemon that no census, no `groved stop`, and no auto-start
// client can see.
func TestReleaseLeavesAnotherProcessesPidfileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")

	if err := AcquireWait(path, 0); err != nil {
		t.Fatal(err)
	}
	// Simulate the successor taking over while we are still tearing down.
	if err := os.WriteFile(path, []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Release(path); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Release removed a pidfile owned by another process: %v", err)
	}
}

// TestReleaseRemovesOurOwnPidfile is the ordinary path.
func TestReleaseRemovesOurOwnPidfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")
	if err := AcquireWait(path, 0); err != nil {
		t.Fatal(err)
	}
	if err := Release(path); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the pidfile to be gone, got %v", err)
	}
	// And the lock must be gone with it.
	if err := AcquireWait(path, 0); err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = Release(path)
}

func TestOwnsReportsTakeover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")
	if err := AcquireWait(path, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Release(path) }()

	if ok, why := Owns(path); !ok {
		t.Fatalf("expected to own our own pidfile, got %q", why)
	}

	if err := os.WriteFile(path, []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, why := Owns(path)
	if ok {
		t.Fatal("expected Owns to report the takeover")
	}
	if !strings.Contains(why, "999999") {
		t.Errorf("reason should name the new owner, got %q", why)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ok, why := Owns(path); ok || !strings.Contains(why, "no longer exists") {
		t.Errorf("expected a missing-pidfile reason, got ok=%v why=%q", ok, why)
	}
}

func TestIsRunningToleratesAnEmptyPidfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groved.pid")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	running, pid, err := IsRunning(path)
	if err != nil || running || pid != 0 {
		t.Fatalf("empty pidfile: got running=%v pid=%d err=%v", running, pid, err)
	}
}

func TestAcquireWaitFromEnv(t *testing.T) {
	t.Setenv(AcquireWaitEnv, "")
	if got := acquireWaitFromEnv(); got != DefaultAcquireWait {
		t.Errorf("unset: got %s, want %s", got, DefaultAcquireWait)
	}
	t.Setenv(AcquireWaitEnv, "250ms")
	if got := acquireWaitFromEnv(); got != 250*time.Millisecond {
		t.Errorf("set: got %s", got)
	}
	t.Setenv(AcquireWaitEnv, "not-a-duration")
	if got := acquireWaitFromEnv(); got != DefaultAcquireWait {
		t.Errorf("garbage: got %s, want the default", got)
	}
}

// waitForPID blocks until the pidfile names a PID, so a race test starts from a
// known state instead of a guessed sleep.
func waitForPID(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := Read(path); err == nil && pid > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to be written", path)
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
