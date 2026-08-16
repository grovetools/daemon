package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/tend/pkg/command"
	"github.com/grovetools/tend/pkg/fs"
	"github.com/grovetools/tend/pkg/harness"
	"github.com/grovetools/tend/pkg/verify"
	"github.com/grovetools/tend/pkg/wait"
)

// raceContenders is how many `groved start` invocations race for the pidfile.
// A fleet restart's real number is "one per client that asked at that instant";
// eight is enough to catch a lock that only serializes pairs.
const raceContenders = 8

// DaemonPidfileRaceScenario is the 2026-08-15 shadow-daemon finding as a test.
//
// `groved start` does not fork — the invoked process IS the daemon — so every
// process that survives pidfile.Acquire runs a complete daemon workload:
// collectors, fsnotify watchers, a signal-cli subprocess, and a socket bind
// that unlinks whatever is already at the path. Acquire used to be a
// read-check-write, so several clients auto-starting groved in the same instant
// (the ordinary shape of a fleet restart) all passed it, and the machine ran
// two full daemons for 2h22m with the second one invisible to `groved status`,
// `groved health` and `groved stats` alike.
//
// This scenario is the END-TO-END guard on that contract, exercised against the
// real binary: exactly one daemon, every loser dead, and each loser's exit
// diagnosable. It is not the reproducer — eight groveds cold-starting spread
// their arrival at Acquire across their own process-startup time, so the old
// read-check-write usually happens to elect one winner here anyway. The
// deterministic reproduction lives in the pidfile package's own race test,
// where the contenders reach Acquire within microseconds of each other (the old
// implementation elects two or more there; the flock elects one).
//
// The assertions are deliberately about the LOSERS, not just the winner: a fix
// that merely made the winner correct would still leave duplicate daemons
// burning CPU. Each loser must exit nonzero, say why, and do it before it can
// have bound a socket or spawned anything.
func DaemonPidfileRaceScenario() *harness.Scenario {
	return harness.NewScenario(
		"daemon-pidfile-race",
		"Concurrent groved starts elect exactly one daemon; the losers exit without booting",
		[]string{"daemon", "lifecycle", "race"},
		[]harness.Step{
			harness.NewStep("race several groved starts for one pidfile", func(ctx *harness.Context) error {
				binary, err := FindBinary()
				if err != nil {
					return err
				}

				// A scratch namespace: pidfile and socket both land inside the
				// sandbox, so this never touches the developer's live daemon.
				groveStateDir := filepath.Join(ctx.StateDir(), "grove")
				if err := fs.EnsureDir(groveStateDir); err != nil {
					return err
				}
				groveRunDir := filepath.Join(ctx.RuntimeDir(), "grove")
				if err := fs.EnsureDir(groveRunDir); err != nil {
					return err
				}
				pidPath := filepath.Join(groveStateDir, "groved.pid")
				ctx.Set("pid_path", pidPath)
				ctx.Set("socket_path", filepath.Join(groveRunDir, "groved.sock"))

				if fs.Exists(pidPath) {
					return fmt.Errorf("scratch pidfile %s already exists; the race must start from nothing", pidPath)
				}

				env := []string{
					"HOME=" + ctx.HomeDir(),
					"XDG_CONFIG_HOME=" + ctx.ConfigDir(),
					"XDG_DATA_HOME=" + ctx.DataDir(),
					"XDG_STATE_HOME=" + ctx.StateDir(),
					"XDG_CACHE_HOME=" + ctx.CacheDir(),
					"XDG_RUNTIME_DIR=" + ctx.RuntimeDir(),
					// The acquire wait exists for a `groved upgrade` handoff,
					// where the pidfile's holder is on its way out. Nobody is
					// leaving here, so compress it: a loser should fail fast.
					"GROVE_PIDFILE_WAIT=500ms",
				}

				procs := make([]*command.Process, raceContenders)
				starts := make([]error, raceContenders)
				var wg sync.WaitGroup
				for i := 0; i < raceContenders; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						p, err := command.New(binary, "start", "--collectors=workspace").
							Dir(ctx.RootDir).
							Env(env...).
							Start()
						procs[i], starts[i] = p, err
					}(i)
				}
				wg.Wait()

				for i, err := range starts {
					if err != nil {
						return fmt.Errorf("contender %d failed to launch: %w", i, err)
					}
				}
				ctx.Set("contenders", procs)
				return nil
			}),
			harness.NewStep("exactly one contender survives", func(ctx *harness.Context) error {
				procs, _ := ctx.Get("contenders").([]*command.Process)
				if len(procs) != raceContenders {
					return fmt.Errorf("expected %d contenders, got %d", raceContenders, len(procs))
				}

				// Liveness is read from the process table rather than by
				// waiting on each contender: Wait() kills whatever is still
				// running when it times out, which would collect the very
				// daemon this scenario is trying to observe.
				var survivors, losers []int
				deadline := time.Now().Add(20 * time.Second)
				for {
					survivors, losers = survivors[:0], losers[:0]
					for i, p := range procs {
						if processAlive(p.PID) {
							survivors = append(survivors, i)
						} else {
							losers = append(losers, i)
						}
					}
					if len(losers) >= raceContenders-1 || !time.Now().Before(deadline) {
						break
					}
					time.Sleep(250 * time.Millisecond)
				}

				// Only the already-exited contenders are waited on, to harvest
				// their exit codes and diagnostics.
				exitCodes := map[int]int{}
				stderrs := map[int]string{}
				for _, i := range losers {
					res := procs[i].Wait(5 * time.Second)
					exitCodes[i], stderrs[i] = res.ExitCode, res.Stderr
				}
				ctx.Set("survivor_pids", survivorPIDs(procs, survivors))

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal("exactly one daemon survived", 1, len(survivors))
					v.Equal("every other contender exited", raceContenders-1, len(losers))
					for _, i := range losers {
						v.NotEqual(fmt.Sprintf("loser %d exited nonzero", i), 0, exitCodes[i])
						v.True(
							fmt.Sprintf("loser %d explained itself (stderr: %q)", i, strings.TrimSpace(stderrs[i])),
							strings.Contains(stderrs[i], "already running") ||
								strings.Contains(stderrs[i], "failed to start"),
						)
						// A loser that got as far as binding or booting would
						// be the very shadow this scenario exists to prevent.
						v.True(
							fmt.Sprintf("loser %d never reached boot", i),
							!strings.Contains(stderrs[i], "Daemon listening"),
						)
					}
				})
			}),
			harness.NewStep("the pidfile names the survivor", func(ctx *harness.Context) error {
				pidPath := ctx.GetString("pid_path")
				socketPath := ctx.GetString("socket_path")

				opts := wait.Options{
					Timeout:      15 * time.Second,
					PollInterval: 200 * time.Millisecond,
					Immediate:    true,
				}
				if err := wait.For(func() (bool, error) {
					return fs.Exists(pidPath) && fs.Exists(socketPath), nil
				}, opts); err != nil {
					return err
				}

				content, err := fs.ReadString(pidPath)
				if err != nil {
					return err
				}
				pid, err := strconv.Atoi(strings.TrimSpace(content))
				if err != nil {
					return fmt.Errorf("pidfile %s holds %q: %w", pidPath, content, err)
				}

				survivorPIDs, _ := ctx.Get("survivor_pids").([]int)

				return ctx.Verify(func(v *verify.Collector) {
					v.True("pidfile names a live pid", pid > 0)
					v.True(
						fmt.Sprintf("pidfile names the surviving daemon (pid %d, survivors %v)", pid, survivorPIDs),
						containsInt(survivorPIDs, pid),
					)
				})
			}),
			harness.NewStep("no shadow contender is still alive", func(ctx *harness.Context) error {
				// The detection gap that let the original pair hide for 2h22m:
				// every daemon-side surface reported one healthy daemon, and
				// only the process table disagreed. So the process table is
				// what this asserts against, contender by contender.
				procs, _ := ctx.Get("contenders").([]*command.Process)

				var alive []int
				for _, p := range procs {
					if p != nil && processAlive(p.PID) {
						alive = append(alive, p.PID)
					}
				}

				return ctx.Verify(func(v *verify.Collector) {
					v.Equal(fmt.Sprintf("exactly one contender process is alive (alive: %v)", alive), 1, len(alive))
				})
			}),
		},
	).WithTeardown(
		harness.NewStep("cleanup contenders", func(ctx *harness.Context) error {
			procs, _ := ctx.Get("contenders").([]*command.Process)
			for _, p := range procs {
				if p != nil {
					_ = p.Kill()
				}
			}
			return nil
		}),
	)
}

// processAlive reports whether a PID is still in the process table. The
// contenders are children of this harness and are reaped by tend's own Wait
// goroutine, so a live PID here is a live process, never a zombie.
func processAlive(pid int) bool {
	out, err := command.RunSimple("ps", "-o", "pid=", "-p", strconv.Itoa(pid))
	return err == nil && strings.TrimSpace(out) != ""
}

func survivorPIDs(procs []*command.Process, idxs []int) []int {
	pids := make([]int, 0, len(idxs))
	for _, i := range idxs {
		pids = append(pids, procs[i].PID)
	}
	return pids
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
