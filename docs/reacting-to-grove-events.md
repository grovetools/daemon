# Reacting to grove events

The daemon publishes a lifecycle event whenever something it tracks changes —
a job finishes, a note is written, a build completes, a satellite reconnects.
There are two ways to react:

- **run a command** — `[[daemon.hooks.on_event]]` in `grove.toml`. No process
  to keep alive, no reconnect logic. Best for notifications, indexers, and
  anything you would otherwise wire into a cron.
- **subscribe to the stream** — `GET /api/stream` over the daemon's unix
  socket, or `daemon.Client.StreamStateWithOptions` from Go. Best for a
  long-lived consumer that maintains derived state.

Both read the same bus, so the event vocabulary below applies to both.

---

## Running a command on an event

```toml
[[daemon.hooks.on_event]]
  name    = "desktop-notify"
  events  = ["job_completed", "job_failed"]
  filter  = "workspace=grove*"
  command = 'notify-send "grove" "$GROVE_JOB_ID $GROVE_EVENT_TYPE"'
  timeout = 30
```

`events` accepts globs, so `job_*` catches the whole job lifecycle. A hook
with no `events` never fires — the daemon logs that at startup rather than
defaulting to the firehose.

### What the hook receives

The event arrives twice, so you can pick whichever is less work.

**On stdin, as JSON:**

```json
{
  "type": "job_completed",
  "seq": 4711,
  "source": "jobrunner",
  "time": "2026-07-29T13:24:02-04:00",
  "job_id": "job-9",
  "plan": "grove-extensiblity",
  "status": "completed",
  "data": { "id": "job-9", "plan_name": "grove-extensiblity", "…": "…" }
}
```

The top-level fields are a stable projection. `data` is the daemon's raw
internal payload — useful, but its shape follows whatever subsystem owns the
event, so treat it as a debugging aid rather than a contract.

**In the environment:**

| Variable | Meaning |
| :--- | :--- |
| `GROVE_EVENT_TYPE` | the event type (`job_completed`) |
| `GROVE_EVENT_SEQ` | the bus sequence number |
| `GROVE_EVENT_SOURCE` | which collector produced it |
| `GROVE_EVENT_ORIGIN` | the satellite the event came from; empty for local |
| `GROVE_JOB_ID` | job id, when the event has one |
| `GROVE_WORKSPACE` | workspace path or name |
| `GROVE_PLAN` | plan name |
| `GROVE_JOB_STATUS` | status/state/kind, depending on the event |

Fields the event does not carry are omitted rather than exported empty, so
`${GROVE_PLAN:-none}` works the way you expect.

### Narrowing with `filter`

`filter` is a conjunction of `field=glob` terms, separated by spaces or
commas. Known fields: `type`, `job_id`, `workspace`, `plan`, `status`,
`source`, `origin`.

```toml
filter = "plan=grove-* status=failed"    # both must hold
filter = "workspace=grovetools"          # matches the path's last segment too
filter = "grovetools"                    # bare term: substring over workspace/plan/job id
```

`*` spans any characters, `/` included — unlike shell globbing — because the
fields you most want to filter on are paths. Path-valued fields also match on
their last segment, so `workspace=grovetools` matches
`/Users/me/src/grovetools` without a leading `*`.

This is deliberately not an expression language. If you need one, that is the
CEL follow-up, not something to approximate with more punctuation here.

### Lifecycle

`[[daemon.hooks.on_event]]` carries the full `HookCommand` lifecycle:

| Key | Behavior |
| :--- | :--- |
| `timeout` | Seconds before the hook is killed. Defaults to 30. The hook runs in its own process group, so the timeout kills the whole tree rather than leaving orphaned grandchildren. |
| `cancel_previous` | On a new matching event, SIGTERM the in-flight run of the same hook instead of running both. Use it for anything that rebuilds or re-indexes. |
| `disable_env` | Skip the hook while the named variable is non-empty in the DAEMON's environment. A headless satellite can mute desktop notifications this way. |
| `enable_env` | Skip the hook unless the named variable is non-empty. Opt-in gating for experiments. |

`run_if` is a skill-sync concept and is ignored here: an event already asserts
that something changed.

Hooks run concurrently with each other and with the daemon. Editing
`grove.toml` re-arms them without a daemon restart.

### Delivery guarantees

**At-most-once, and only while the daemon is running.** Hooks miss events that
happen while the daemon is down, and events dropped when a subscriber's buffer
overflows. Terminal job events are deduplicated per hook by job id — the store
synthesizes a terminal event from every federated snapshot that shows the
transition, so without dedupe a satellite job would notify repeatedly — but
that is the only guarantee on offer.

If your hook must not miss anything, treat the event as a trigger and
reconcile against the REST API (`GET /api/jobs`, `GET /api/notes/index`, …)
rather than trusting that you saw every transition.

### Security

`[[daemon.hooks.on_event]]` is exec-bearing config with **implicit** risk: it
runs when the daemon merely notices something, with no session, no verb and no
user action. It is therefore quarantined when it arrives from a repository —
the ecosystem, project, or project-local layers — unless you have explicitly
trusted that file:

```bash
grove config trust          # show what trusting would enable
grove config trust --yes
```

Hooks in your own `~/.config/grove/` are always honored: you own those files.
See [the exec-config trust gate](../../core/docs/03-configuration.md#security-the-exec-config-trust-gate).

---

## Subscribing to the stream

`GET /api/stream` is a Server-Sent Events endpoint on the daemon's per-scope
unix socket. Every frame is a JSON object with an `update_type` and a
monotonic `seq`.

```
GET /api/stream                          # everything, from now
GET /api/stream?types=job_*,note_event   # server-side filter
GET /api/stream?since=4711               # resume after a sequence number
```

The response advertises what the daemon supports:

```
X-Grove-Stream-Features: seq,since,types
X-Grove-Stream-Ring: 1024
```

**Check that header.** `/api/stream` is not a new endpoint, so a daemon older
than this feature answers `200` and simply ignores `since` and `types`. Absent
features mean you got an unfiltered, unsequenced firehose — filter locally and
do not build a cursor out of the zeroes in `seq`.

### Resuming

Pass the last `seq` you processed as `?since=`. Two outcomes:

- **Exact resume** — the daemon replays everything after your cursor from its
  in-memory ring, then continues live. The `initial` snapshot frame is
  skipped, because you already have that state.
- **Gap** — the daemon emits a `stream_gap` control frame and re-sends the
  `initial` snapshot instead of replaying:

  ```json
  {"update_type":"stream_gap","seq":5200,
   "payload":{"reason":"too_old","since":11,"oldest":4177,"current":5200,"ring_size":1024}}
  ```

  `reason` is `too_old` when the ring already evicted what you asked for (you
  were away too long, or fell behind), and `reset` when your cursor is *ahead*
  of the daemon — sequences restart at 1 with each daemon process, so that is
  what a restart looks like from outside. Both mean **reconcile**: take the
  snapshot, re-read whatever you derive from individual events, and resume.
  Only `reset` invalidates the cursor itself.

`stream_gap` is a control frame and is never suppressed by `?types=` — a
consumer that filtered itself into silence would otherwise never learn it
needs to reconcile. The `initial` snapshot is *not* a control frame: it is
ordinary state, so a subscriber filtering on `job_*` does not receive it,
before or after a gap. Such a consumer reconciles through the REST API, which
is what it was going to do anyway.

The ring holds the last **1024** updates. That is roughly a minute of a busy
daemon, so reconnect promptly if you care about gap-free resumption; it is a
recovery buffer, not a durable log. A daemon restart empties it.

### From Go

```go
ch, caps, err := client.StreamStateWithOptions(ctx, daemon.StreamOptions{
    Resume: true,
    Since:  lastSeq,
    Types:  []string{"job_*"},
})
if err != nil {
    return err
}
if !caps.TypeFilter {
    // Old daemon: it sent everything. Filter locally.
}
for update := range ch {
    if gap, ok := daemon.ParseStreamGap(update); ok {
        reconcile(gap)
        continue
    }
    lastSeq = update.Seq
    handle(update)
}
```

A bubbletea TUI can use `daemonstream.StartStreamCmdWithOptions`, which
surfaces gaps as a `StreamGapMsg` and the daemon's capabilities on
`StreamReadyMsg`.

---

## The event vocabulary

Event names are `store.UpdateType` values. The families:

| Family | Events |
| :--- | :--- |
| Jobs | `job_submitted`, `job_started`, `job_completed`, `job_failed`, `job_cancelled`, `job_pending_user`, `job_orphaned` |
| Sessions | `session_intent`, `session_confirmation`, `session_status`, `session_end`, `session_tokens` |
| Workflows / subagents | `workflow_run_discovered`, `workflow_agent_started`, `workflow_agent_completed`, `workflow_run_stale`, `workflow_run_completed`, `workflow_children_snapshot`, `workflow_bash_started` |
| Subjobs | `subjob_report_ready`, `subjob_joined` |
| Builds | `build_queued`, `build_started`, `build_finished` |
| Workspaces / git | `workspaces`, `workspaces_delta`, `task_result` |
| Notes | `note_event`, `note_index` |
| Plans | `plan_index` (revisioned deltas), `plan_index_snapshot`, `plans` |
| Memory | `memory_index`, `memory_reindex` |
| Sync | `sync_conflict` |
| Satellites | `satellite_status`, `satellite_snapshot` |
| Config / theme / skills | `config_reload`, `theme_changed`, `skill_sync`, `watcher_status` |
| Daemon | `boot_phase`, `focus`, `nav_bindings` |
| Agent panes, relayed | `spawn_agent_pane`, `attach_agent_pane`, `agent_input`, `capture_request` |
| Bulk / in-place writes | `jobs_discovered`, `sessions`, `session_channels`, `session_autonomous`, `session_ping`, `session_tmux_target`, `session_last_sender`, `test_report` |

### Two warts worth knowing

**Hooks see more than the stream does.** A hook subscribes to the store
directly, so it can name any type in the table above. The stream carries a
subset: bulk scans, in-place session field writes and full-list replacements
are deliberately not broadcast (they are declared, with reasons, in
`apiUpdateSkipList`). So `events = ["test_report"]` works for a hook while
`?types=test_report` matches nothing.

**Two families are renamed on the wire.** The five `session_*` lifecycle types
all collapse to `session`, and `task_result` arrives as `workspaces_delta`. A
hook uses the store name; a stream subscriber uses the wire name and reads the
payload to tell which transition it got. Everything else keeps one name on
both sides.

`job_orphaned` is deliberately **not** terminal: "the daemon cannot see this
job" is a claim about the daemon, not a verdict on the agent's work. It is not
deduplicated as a terminal event, and a hook that treats it as failure will be
wrong across daemon restarts.

Some internal update types never reach the wire — bulk discovery scans,
in-place session field writes, full plan-list replacements. Those omissions are
declared with reasons in `apiUpdateSkipList`
(`internal/daemon/server/stream_bus.go`), and a type in neither the converter
nor the skip list fails a test. If you configure a hook for an event name the
daemon does not know, it says so at startup instead of silently never firing.

`job_orphaned` is deliberately **not** terminal: "the daemon cannot see this
job" is a claim about the daemon, not a verdict on the agent's work. It is not
deduplicated as a terminal event, and a hook that treats it as failure will be
wrong across daemon restarts.

Some internal update types never reach the wire — bulk discovery scans,
in-place session field writes, full plan-list replacements. Those omissions are
declared with reasons in `apiUpdateSkipList`
(`internal/daemon/server/stream_bus.go`), and a type in neither the converter
nor the skip list fails a test. If you configure a hook for an event name the
daemon does not know, it says so at startup instead of silently never firing.
