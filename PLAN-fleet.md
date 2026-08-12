# Plan: fleet mode

A plan to take pairin from "one TUI per project, one project per terminal" to "one dashboard for
every project on the host."

---

## 1. What you asked for

Three pains, and one idea that resolves all three:

| # | Pain | Root cause |
|---|------|------------|
| 1 | Need tmux to watch several pairins at once | The TUI can only dial one supervisor's socket |
| 2 | 20 services won't fit as stacked log panes | Split view gives every service a full log viewport |
| 3 | pairin sometimes dies and takes the tmux pane with it | No panic capture, no disconnect handling, no reconnect |

The idea: **a host-wide dashboard** (`pairin dash`) that shows a grid of services grouped by project,
with `z` to zoom into one service's logs — plus a **project catalog** so you can start any registered
project from anywhere without hunting for its directory.

Pain 2 is the same widget as the dashboard, scoped to one project. So the grid gets built once and
used in both places.

---

## 2. Recommendation in one paragraph

Ship it as **one binary**, not a separate `pairinctl` — the dashboard shares the registry, protocol,
and TUI code with `pairin`, and a second binary means two things to install and keep in version sync.
The entry point is `pairin dash` (with `pairin` outside a project directory falling back to it, and a
`pairinctl` symlink if you want the muscle memory). Build in this order: **stability first** (phase 0),
then **compact view** (phase 1) because it's self-contained and kills pain 2 immediately, then
catalog → hub → fleet dashboard.

---

## 3. What's in the way (findings in the current code)

Four things need fixing before a dashboard holding ~100 services across N sockets will be stable.
These are real, verified against the current tree — not speculation.

### 3a. A wedged client can freeze service startup — `process/manager.go:372-439`

`startService` holds `svc.mu` for its whole body (`defer svc.mu.Unlock()` at line 375) and calls
`m.send(...)` five times inside that critical section. `m.send` → `Server.Send` → `broadcast` →
`serverClient.write` → a **synchronous** `json.Encoder.Encode` straight onto the unix socket, with no
write deadline (`control/server.go:143-159, 285-292`).

So: one TUI client that stops reading — suspended with `ctrl+z`, blocked rendering, or on the far end
of a stalled SSH pipe — fills the socket buffer, blocks the encode, and pins `svc.mu` forever. Every
other goroutine touching that service (log tailer, healthcheck poller, stop path) blocks behind it,
and `broadcast` iterates clients serially so *all other clients* stop receiving events too.

With one local TUI this is nearly invisible. With a dashboard holding a dozen sockets open across the
host, it's a matter of time.

**Fix:** per-client buffered send queue + a writer goroutine, drop-oldest on overflow, write deadline
on the conn, and drop `m.send` calls out of the `svc.mu` critical section (collect them and send
after unlock).

### 3b. The client mirror is written and read from two goroutines — `control/client.go:181-221`

`Client.apply` runs on the readLoop goroutine and writes `svc.Status`, `svc.PID`, `svc.Branch`,
`svc.Healthy`, `svc.RestartCount`, and `svc.Logs.Add(...)` — none of it under `svc.mu`. Meanwhile the
Bubble Tea render goroutine reads exactly those fields in `Pane.titleLine` (`tui/pane.go:93-144`).
`Service.GetLines` takes `svc.mu`, but since `apply` never does, the mutex buys nothing on this path.

`go test -race` doesn't currently exercise it, which is why it hasn't surfaced.

**Fix (preferred):** stop sharing mutable structs across the boundary. Have the client put the data
*in* the tea.Msg and let the model own its copy — that's the Bubble Tea grain, and it's a
precondition for the fleet model anyway, which needs per-instance state the model can key by ID.
Cheaper interim: lock in `apply`.

### 3c. Nothing watches for the supervisor going away — `cmd/root.go:120-136`

`attachTUI` dials, runs the program, and never selects on `client.Done()`. If the supervisor dies or
the socket closes, `readLoop` closes the channel and the TUI keeps rendering stale content with no
indication anything is wrong. There is no reconnect path at all.

**Fix:** a goroutine that forwards `Done()` into the program as a `DisconnectedMsg`; the model shows a
banner and enters reconnect-with-backoff instead of freezing.

### 3d. No panic capture anywhere

A panic in the TUI, or in any supervisor goroutine (`tailFile`, `waitForExit`, the healthcheck poller,
`autoRestartService`), takes the whole process down with a stack trace splattered over the alt-screen —
and if it's the supervisor, all services go with it. This is the most likely explanation for pain 3,
but **we don't actually know yet**, which is the point: step one is capturing the evidence, not
guessing at the bug.

**Fix:** `recover()` at the top of every long-lived goroutine and around `p.Run()`, writing stack +
context to `$XDG_STATE_HOME/pairin/crash-<ts>.log`, with the TUI path printing "pairin crashed; your
services are still running — see <path>" on the way out.

---

## 4. Target architecture

```
                    $XDG_CONFIG_HOME/pairin/projects.toml     <- catalog (you curate this)
                    $XDG_STATE_HOME/pairin/instances/*.json   <- registry (supervisors self-report)
                                    |
                          +---------+---------+
                          |    internal/hub   |   dial all live supervisors concurrently,
                          |       Hub         |   reconnect w/ backoff, tag every event
                          +---------+---------+   with an InstanceID
                            |       |       |
                   control.Client   |    control.Client        (one per running supervisor)
                          |         |         |
                     .pairin/control.sock  ...  (unchanged, plus a log-subscription filter)
                          |
                +---------+----------+
                |  supervisor (per project)  |
                +----------------------------+

     tui.FleetModel  ---uses--->  tui.Grid  <---uses---  tui.ProjectModel
     (all projects)               (shared)               (one project: split | grid | focus)
```

Nothing about the supervisor's ownership model changes. One supervisor per project, keyed by absolute
config path, exactly as today. The dashboard is purely a *client* that holds several of them open.

### Log volume: subscribe, don't firehose

Today the server broadcasts every log line to every connected client. A dashboard across 5 projects ×
20 services would pull ~100 live log streams over sockets to render a grid of names — pure waste, and
it makes 3a much easier to trigger.

Add a subscription filter to the protocol:

```go
// control/protocol.go
const ReqSubscribe RequestKind = "subscribe"

type Request struct {
    Kind     RequestKind `json:"kind"`
    Service  string      `json:"service,omitempty"`
    LogMode  string      `json:"log_mode,omitempty"`  // "" (=all, back-compat) | "none" | "only"
    Services []string    `json:"services,omitempty"`  // for log_mode "only"
}
```

`serverClient` grows `logMode` + `logServices` under its existing mutex; `broadcast` filters per
client. Zero-value default is "all", so `pairin attach` behaves exactly as it does now.

The fleet dashboard sends `{kind: subscribe, log_mode: none}` right after connect — status and health
events only — and switches to `{log_mode: only, services: ["api"]}` when you zoom. Zoom fills the
backlog from the on-disk log file via the existing `tui/tail.go`, so it's instantly populated.

---

## 5. Phases

Each phase ships standalone and is useful on its own.

### Phase 0 — Stop vanishing *(fixes pain 3)* — **shipped on `fleet-stability`**

- `recover()` + crash-log writer in: `attachTUI`'s `p.Run()`, `runSupervisor`, and each long-lived
  manager goroutine (`tailFile`, `waitForExit`, healthcheck poller, `autoRestartService`).
- Crash log at `$XDG_STATE_HOME/pairin/crash-<ts>.log`: stack, version, config path, service states.
- Per-client send queue + writer goroutine + write deadline in `control/server.go`; drop-oldest with a
  `[pairin] dropped N log lines (slow client)` marker.
- Move `m.send` calls out of `svc.mu` in `startService`.
- Fix the client-mirror race (3b).
- Watch `client.Done()`; `DisconnectedMsg` → banner → reconnect with backoff (250ms → 5s cap).
- `go test -race ./...` green; add a two-client integration test over a temp-dir config.

**Done when:** killing the supervisor mid-session leaves the TUI up with a reconnect banner, and it
reattaches by itself when you `pairin up` again. `SIGSTOP`ing one client doesn't stall another.

**Outcome.** All of the above landed. Two notes worth carrying forward:

- The most likely explanation for the vanishing turned out to be the TUI panic path. Bubble Tea's
  own handler prints a stack to a screen it's tearing down and lets `Run` return a **nil** error, so
  pairin exited zero with nothing recorded and tmux closed the pane. `tea.WithoutCatchPanics()` plus
  our own handler now writes a report and returns a real error. If it recurs, there will be a file in
  `~/.local/state/pairin/` naming the goroutine and the line.
- The stall invariants in §3a were already *documented* in ARCHITECTURE.md ("`svc.mu` is never held
  while calling `m.send()`", "a stuck/slow client can't block the manager's event path") — the code
  had simply drifted away from them. Both are now enforced, and
  `TestWedgedClientDoesNotBlockOthers` fails against the old broadcast, so the drift can't recur
  silently.

Unrelated find, not fixed: a project nested deeply enough that `.pairin/control.sock` exceeds the
~107-byte unix socket path limit fails with a bare `bind: invalid argument`. Worth a friendlier
error, but it isn't a stability bug.

### Phase 1 — Compact view *(fixes pain 2)*

New `internal/tui/grid.go`, a pure component: given width, a list of groups, and a selection, render a
wrapping grid of status cells. Layout is a pure function, so it's unit-testable without a terminal.

`tui/model.go` (rename → `project.go`) gains a third mode alongside split/focus:

```
 acme-api                                              20 services · 18 up

 ● postgres      ● redis         ● api           ◍ worker        ● migrator
 ● mailhog       ● minio         ● stripe-mock   ● web           ● bff
 ● cms           ⟳ scheduler 3/5 ✕ indexer       ⋯ reporter      ● gateway
 ● docs          ● storybook     ○ e2e           ● proxy         ● tunnel

 ● healthy   ◍ running/unhealthy   ⟳ restarting   ⋯ waiting   ✕ crashed   ○ stopped

 ↑↓←→ move   z zoom   r restart   x stop   s start   v view   / filter   q detach
```

- `v` cycles split → grid → split; `z`/`enter` zooms the selection to full-screen logs; `esc` back.
- **Auto-degrade:** if `availableHeight / len(services) < 5`, open in grid mode instead of split.
- Cell shows name + glyph; selected cell reverses. Restart counts and health inline.
- `/` filters by name substring.

**Done when:** a 20-service config opens in a readable grid and `z` gets you a full-screen log tail.

### Phase 2 — Project catalog *(fixes "hunting for config files")*

New `internal/catalog/catalog.go`, backed by `$XDG_CONFIG_HOME/pairin/projects.toml` (fallback
`~/.config/pairin/projects.toml`) — config, not state, so it survives cleanup and can live in dotfiles:

```toml
[[project]]
name   = "acme-api"
config = "/home/nick/Code/acme-api/.pairinrc.toml"
group  = "work"
```

New commands:

- `pairin register [path]` — defaults to the config found from cwd; `--name`, `--group`
- `pairin unregister <name|path>`
- `pairin projects` — catalog + live/stopped state
- `pairin up <name>` / `pairin down <name>` — resolve through the catalog, so no `cd` needed

`pairin up` auto-registers the project it just started (opt out with `--no-register`), so the catalog
fills itself in as you work. Entries whose config file has disappeared are flagged in `projects`
output but never silently deleted.

**Done when:** `pairin up acme-api` works from `~`.

### Phase 3 — The hub

New `internal/hub/hub.go`:

```go
type InstanceID string   // reuse the existing sha256-of-abs-config-path key

type ConnState int       // Stopped | Connecting | Connected | Unreachable

type Instance struct {
    ID         InstanceID
    Name       string
    ConfigPath string
    Group      string
    State      ConnState
    Services   []ServiceView   // from the socket when up, from config.LoadFrom when down
    Err        error
}

type Msg struct {           // every client event, tagged with its origin
    ID    InstanceID
    Inner tea.Msg
}
```

- Unions catalog ∪ `state.ListInstances()`; dials every live supervisor **concurrently and
  non-blocking** (today's `control.Dial` blocks up to 5s for the snapshot — the hub must not serialize
  that across a dozen projects).
- Installs a per-instance `process.Sink` adapter that re-tags events as `hub.Msg` — so
  `control.Client` needs no changes at all; `SetSink` already takes any `Sink`.
- Sends `{log_mode: none}` on connect; `SubscribeLogs(id, service)` on zoom.
- Polls the registry every ~2s to pick up projects started elsewhere, and drops instances whose
  supervisor died.
- `StartProject(id)` re-execs `pairin supervisor --config <path>` (the existing `spawnSupervisor`,
  lifted out of `cmd/root.go` so the hub can call it); `StopProject(id)` sends `ReqShutdown`.

**Done when:** a headless test can hold three temp-dir supervisors and observe tagged status events
from all of them.

### Phase 4 — The fleet dashboard *(fixes pain 1)*

`internal/tui/fleet.go` — `FleetModel` over a `Hub`, rendering `Grid` once per project group:

```
 pairin · 4 projects · 37 services · 34 up                          14:22:08

 ▾ acme-api          ~/Code/acme-api                     sup 48213  2h13m
   ● postgres      ● redis         ● api          ◍ worker       ● migrator
   ● mailhog       ✕ scheduler     ● web

 ▾ storefront        ~/Code/storefront                   sup 48910  45m
   ● web           ● bff           ● cms          ● search

 ▸ analytics         ~/Code/analytics                    stopped
   5 services — press s to start

 ▸ infra             ~/Code/infra                        unreachable (socket)

 ↑↓←→ move  z zoom  r restart  x stop  s start  S shutdown project  / filter  ? help  q quit
```

- Registered-but-stopped projects render greyed with their service names read from
  `config.LoadFrom` — you can see a project's shape before starting it, and `s` starts it in place.
- `z` on a service → full-screen log view: on-disk backlog + live subscription, `esc` returns.
- `q` quits the dashboard only. It never touches supervisors — same detach semantics as today.
- Groups collapse with `space`; `/` filters across all projects at once.

**Done when:** you can close tmux.

### Phase 5 — Polish

- `pairin logs <project> <service> [-f]` — plain tail, no TUI, for pasting into tickets.
- `pairin status --json` / `pairin ls --json` for scripting.
- Log search within zoom (`/` in focus mode), jump-to-crash.
- `?` help overlay; `pairinctl` symlink; shell completion for catalog names.
- Consider collapsing `ProjectModel` into `FleetModel` filtered to one instance — one model, one set
  of keybindings, split view kept as a mode for small projects.

---

## 6. Decisions (settled)

1. **One binary with a `dash` mode.** Not a separate `pairinctl`.
2. **Bare `pairin` outside a project keeps its current error.** No dashboard fallback — "no config
   found" stays the answer, so the command's behavior doesn't depend on where you happen to be.
3. **Auto-register on `up`, with `--no-register`.** The catalog fills itself in as you work.
4. **Phase order as written**, starting with phase 0.

## 7. Non-goals

Keeping these out so the scope doesn't drift:

- No config hot-reload. The supervisor still holds the config it started with.
- No cross-host / network transport. Unix sockets on one host, same as today.
- No input forwarding to services (no stdin to a REPL from the grid).
- No changes to the supervisor's ownership model, adoption flow, or restart policy semantics.
- No web UI.
