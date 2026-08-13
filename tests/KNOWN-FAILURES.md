# Known test failures — the baseline a phase gate diffs against

`go test ./...` in this repo is not green as of Phase 3. The failures below are
declared, with provenance, so a per-phase gate can **diff against this list**
instead of re-deriving each one by hand. Anything not listed here is a
regression.

## Why this file exists

A phase that adds code to a package inherits that package's gate. When the gate
is already red for reasons the phase did not cause, every reviewer after the
first re-derives the same provenance — `git log` the test, find the commit that
changed the behavior, confirm it predates the phase base — and the one who does
not re-derive it waves the failure through instead. Both outcomes are worse than
writing the answer down once.

This is recommendation #4 of the satellite-PoC retrospective: capture the
known-fail set at plan start, so per-phase gates diff against a declaration
rather than re-proving pre-existence.

## The contract

- A failing test may be listed here **only** with: its full name, the commit or
  condition that makes it fail, the date, and why it is not the current phase's.
- An entry is a debt, not a waiver. Nothing here is expected to stay.
- **An unlisted failure fails the gate.** This file cannot hide a new failure —
  it can only pre-declare an old one, in writing, with its cause.
- A listed test that starts PASSING is also a signal: move it to *Resolved* with
  the fixing commit rather than deleting the entry. The record of what was once
  broken is the part a future reviewer needs.

## Open

### `internal/daemon/store` — `TestAllUpdateTypesCoversEveryConstant`

```
types_registry_test.go:63: allUpdateTypes is missing 1 constant(s): [UpdateForgeState]
```

| | |
|---|---|
| **Failing since** | `984c2c8` (2026-08-02) — *feat(forgepoll): read-only forge poller* |
| **Cause** | the commit added the `UpdateForgeState` constant without adding it to `allUpdateTypes`, which is precisely what this registry test exists to catch |
| **Not P3** | predates the P3 base; P3 touches no store update type |
| **Owner** | forgepoll. One line in `allUpdateTypes` clears it. |

### `internal/daemon/server` — `TestBootEndpointAdvancesInBindMode`, `TestBootPhaseBroadcastReachesStream`

```
Listen: failed to listen on socket: listen unix /var/folders/.../TestBootEndpointAdvancesInBindMode…/001/groved.sock: bind: invalid argument
```

| | |
|---|---|
| **Failing when** | `t.TempDir()` resolves long enough that `<dir>/groved.sock` exceeds the platform's `sun_path` limit (104 bytes on macOS, 108 on Linux) |
| **Cause** | environment, not code: the path is built from the test NAME, and these two names are long. The same tests pass where `TMPDIR` is short. |
| **Not P3** | no P3 change touches the boot handler or the socket path |
| **Owner** | the daemon test harness. Binding under a short `os.MkdirTemp("", "gd")` rather than `t.TempDir()` fixes it for every platform. |

## Resolved

### `internal/daemon/server` — `TestHandleGetWorkflows`

| | |
|---|---|
| **Failed from** | `d2e1dbb` (2026-06-17) — *store: drop phantom subagent_start registration events* |
| **Assertion dated** | `6b75288` (2026-06-10); `a9727a1` (2026-07-08) touched the file without revisiting it |
| **Symptom** | `workflow_handlers_test.go:212: snapshot missing ad-hoc agent: map[]` |
| **Not P3** | the P3 diff over this package touches only `server.go` (route wiring) and the new `sync_contested.go` — no workflow, models or store code |
| **Resolved by** | this phase — the test, not the product |

The test seeded a run-less `WorkflowAgentStarted` with `agent_id: "x1"` and
expected it in the ad-hoc bucket. Since `d2e1dbb` the store deliberately
discards a run-less started event whose agent id is not a genuine spawn id
(`^a[0-9a-f]{16}$`): the harness fires one `SubagentStart` per registered agent
definition at session init, and those phantoms would otherwise populate the
bucket with agents nobody spawned. `"x1"` is a phantom shape, so the store was
right and the assertion was stale.

Fixed by seeding a real spawn id, and the same test now also pins the negative —
a phantom-shaped id must reach the snapshot from nowhere — so both halves of the
guard are asserted at the surface the TUI actually reads.
