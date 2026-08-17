# Architecture

This document describes how pairin's code is structured, how the pieces connect, and the concurrency model that holds it all together.

## File Map

```
main.go                           Entry point, calls cmd.Execute()
cmd/
  root.go                         `pairin` (default): start-or-attach, stale-state prompt, supervisor spawn
  up.go                           `pairin up` alias for the default behavior
  attach.go                       `pairin attach`: connect a TUI to an already-running supervisor
  down.go                         `pairin down`: ask the supervisor to stop everything and exit
  ls.go                           `pairin ls`: list every running supervisor on this host
  status.go                       `pairin status`: per-service status across every supervisor
  supervisor.go                   Hidden `pairin supervisor` subcommand: the detached worker
  version.go                      Version constant and `pairin version` subcommand
internal/
  config/
    config.go                     TOML config loading, dir resolution, validation
    config_test.go                Validation tests (deps, cycles, restart policies)
  browse/
    browse.go                     Directory listing for the project picker: dirs, configs, counts
    browse_test.go                Ordering, project names, config counts, skip list
  ports/
    ports.go                      Listening TCP ports per process group, read from /proc
    ports_test.go                 Fake /proc parsing, plus a real socket against the real kernel
  catalog/
    catalog.go                    Registered projects: load/save, name derivation, prefix lookup
    catalog_test.go               Slugs, unique names, idempotent Add, ambiguous lookup
  hub/
    hub.go                        Connections to every supervisor on the host; tagged events
    hub_test.go                   Multi-supervisor connect, stopped-project stubs, disconnect
  launcher/
    launcher.go                   Spawning detached supervisors, shared by the CLI and the dashboard
  crash/
    crash.go                      Panic capture: Guard for goroutines, Report writes to the state dir
    crash_test.go                 Guard recovers, report lands under XDG_STATE_HOME
  process/
    manager.go                    Process lifecycle, log capture, healthchecks, auto-restart, adoption, mirror services
    manager_test.go               Ring buffer, healthcheck, dependency, restart tests
  control/
    protocol.go                   NDJSON wire format: Request / Event / Snapshot types
    server.go                     Supervisor: socket listener, per-client send queues, Sink that broadcasts manager events
    client.go                     TUI: dial socket, mirror services, reconnect, re-emit events as tea.Msg
    server_test.go                Transport tests: snapshot-on-connect, wedged client, overflow resync, reconnect
  state/
    state.go                      .pairin/state.json + supervisor.pid + IsProcessAlive helpers
    registry.go                   Host-wide instance registry under $XDG_STATE_HOME/pairin/instances/
    ui.go                         Remembered interface state (grid cell style)
    logfile.go                    Per-service log paths and 10 MiB rotation threshold
  tui/
    model.go                      Bubble Tea model: keys, layout, split/grid/focus views; uses Backend interface
    grid.go                       Compact status grid, grouped and filterable; shared by both models
    fleet.go                      Host-wide dashboard model over a hub
    grid_test.go                  Column geometry, navigation, filtering, windowing
    model_test.go                 View auto-degrade, zoom, filter input, render fits the terminal
    fleet_test.go                 Multi-project rendering, cross-project selection, zoom log routing
    pane.go                       Single service pane: viewport, title bar, log rendering
    tail.go                       Preload last N lines from on-disk log files when attaching
    styles.go                     Lipgloss styles, color map
    messages.go                   Re-export comment (message types live in process package)
```

## Boot Sequence

`pairin` always splits into two processes once services are up: a long-lived **supervisor** that owns the `process.Manager`, and a **TUI client** that attaches to it over `.pairin/control.sock`. The default command (`pairin` or `pairin up`) is a start-or-attach flow.

```
foreground (pairin / pairin up)         detached supervisor (pairin supervisor)
-------------------------------         ---------------------------------------
config.Load()                           cmd.runSupervisor() (called via re-exec)
  |                                       |
  v                                       v
LockHolder() alive?                     config.LoadFrom(--config)
  |                                       |
  +- yes -> attachTUI(cfg)                v
  |                                     state.EnsureDirs + write supervisor.pid
  +- no, stale state?                     |
       |                                  v
       +- prompt: Adopt / Restart / Quit  state.Register(Instance)        <-- host registry
       |                                  |
       v                                  v
     spawnSupervisor(--adopt?)          process.NewManager(cfg)
       |                                  |
       v                                  v (if --adopt) for each LiveServices:
     waitForSupervisor()                    mgr.AdoptService(i, pid, pgid, log)
       |                                  |
       v                                  v
     attachTUI(cfg)                     control.NewServer(mgr, cfg)
       |                                server.Start(.pairin/control.sock)
       v                                  |    mgr.SetSink(server)        <-- events fan out
     control.Dial(socket)  --NDJSON---->  accept loop (one goroutine per client)
       |                                  |
       v                                  v
     tui.NewDashboardModel(cfg, client) mgr.StartAll() (deps -> Waiting; rest -> startService)
       |                                  |
       v                                  v
     p.Run() event loop                 select { server.Done() | SIGINT/TERM }
                                          |
                                          v
                                        mgr.StopAll(); server.Shutdown(); state.Unregister()
```

The same `tui.DashboardModel` runs in both directions: it talks to a `Backend` interface (`internal/tui/model.go`), and that backend is either a local `*process.Manager` or a remote `*control.Client`. The client mirrors snapshot events into local `*process.Service` structs (built via `process.NewMirrorService`) and re-emits status/log/health events as the same `tea.Msg` types the manager produces, so the model is identical in either mode.

## Supervisor / Client Split

The supervisor is a session leader (`Setsid: true` on the spawn) so it survives the parent TUI exiting and SSH disconnects. The TUI is just a connected client; closing it (`q`) doesn't touch the services.

### Lifecycle

| Event                                   | Effect                                                                           |
|-----------------------------------------|----------------------------------------------------------------------------------|
| `pairin` and no supervisor running      | Spawn `pairin supervisor --config <path>` via re-exec; wait for socket; attach.  |
| `pairin` and supervisor already running | Skip spawn; `attachTUI` dials `.pairin/control.sock` directly.                   |
| `pairin` with stale state               | Prompt user; on Adopt, spawn supervisor with `--adopt` so it picks up live PIDs. |
| `pairin -d` / `pairin up -d`            | Same spawn-or-confirm flow, but skip `attachTUI` and exit after the socket is up. Prints the supervisor PID. Idempotent. |
| TUI `q` / `ctrl+c`                      | Close the client connection. Supervisor + services keep running.                 |
| TUI `d`                                 | Send `ReqShutdown`. Supervisor calls `StopAll`, then exits.                      |
| `pairin down`                           | Same as `d` but from outside the TUI — dial socket, send `ReqShutdown`, wait.    |
| `pairin attach`                         | Refuse if no live supervisor; otherwise attach a fresh TUI to the running one.   |

### Control Protocol

Wire format is NDJSON over a unix-domain stream socket. All types live in `internal/control/protocol.go`.

```
Client -> Server (Request)              Server -> Client (Event)
--------------------------              ------------------------
{"kind":"snapshot"}                     {"kind":"snapshot", "snapshot": {...}}
{"kind":"restart","service":"web"}      {"kind":"status",   "status":   {...}}
{"kind":"stop",   "service":"web"}      {"kind":"log",      "log":      {...}}
{"kind":"start",  "service":"web"}      {"kind":"health",   "health":   {...}}
{"kind":"shutdown"}                     {"kind":"shutdown"}
{"kind":"subscribe","log_mode":"none"}  {"kind":"logs_cleared","logs_cleared":{...}}
{"kind":"clear_logs","service":"web"}
```

`clear_logs` with an empty `service` clears every service in the project. The supervisor **truncates**
the log rather than unlinking it: services are started with `O_APPEND`, so after truncation the child
resumes writing from the start of the file, whereas unlinking would leave it writing to an inode
nobody can read. (`pairin --clear-logs` *does* delete the files, which is why it only works before a
supervisor starts.) The tailer already handles a file shrinking below its read offset. Clients drop
their mirrored copy on `logs_cleared`, and panes clear themselves — a pane holds its own slice of
lines, so emptying the ring buffer behind it is not enough.

A new client always receives an `EvtSnapshot` first, then a stream of incremental events. The snapshot contains everything the TUI needs to render before any further events arrive (project name, started-at timestamp, and per-service name/short/color/dir/cmd/status/PID/branch/health/adopted/log_file/restart_count/max_restarts/depends_on).

### Server (`control.Server`)

- Wraps a `*process.Manager`. Once `Start(socketPath)` succeeds, the server installs itself as the manager's `Sink`, so every `tea.Msg` the manager produces is routed through `eventFor` and broadcast as a protocol `Event` to every connected client.
- Maintains a `clients map[*serverClient]struct{}` under a mutex. Broadcast iterates a snapshot copy of that map so writes never hold the lock during I/O.
- **Each client owns a send queue and a writer goroutine.** `broadcast` only enqueues, and enqueuing never blocks. This is what keeps one misbehaving client — suspended with `ctrl+z`, or on the far end of a stalled SSH pipe — from stalling the Manager goroutine that produced the event, and from blocking every other client behind it in the broadcast loop.
- When a client's queue overflows, events are **dropped** and the client is flagged for resync; once it catches up, the writer sends a fresh `EvtSnapshot`. Dropping is safe precisely because of this: whatever was lost, the snapshot makes the client's view authoritative again. Drops are logged to `supervisor.log`.
- Every socket write carries a `clientWriteTimeout` deadline, so a client that never reads is disconnected rather than pinning a writer goroutine forever.
- `dispatch` handles incoming requests by looking up the service by name (clients never see indices), then calling the corresponding manager method on a fresh goroutine — never blocking the read loop.
- `Shutdown` is idempotent. It closes the listener, broadcasts a final `EvtShutdown` event, closes every client conn, and `close()`s the `shutdown` channel so `Done()` waiters return.

### Client (`control.Client`)

- `Dial` opens the socket, starts a read loop, and blocks until either the initial snapshot arrives, the connection drops, or 5 seconds elapse. It is a thin wrapper over `Reconnect`.
- `Reconnect` re-establishes a dropped connection on the same client. The read loop takes its conn, `done` and `ready` channels **as parameters** rather than reading them off the struct, so a reconnect can never leave an old loop operating on a replaced socket. `Done()` returns the current connection's channel, so callers must re-read it after reconnecting rather than caching it.
- The first snapshot allocates `*process.Service` mirrors via `process.NewMirrorService` and builds a `nameToIdx` map. Subsequent snapshots **update fields in place** rather than rebuilding the slice, so pointers held by the TUI survive a supervisor restart.
- Each incoming event mutates the mirror service **through `Service`'s locked mutators** (`ApplyStatus`, `ApplyHealth`, `AppendLog`, `UpdateMirror`) and forwards a translated `tea.Msg` (`StatusMsg`, `LogMsg`, `HealthCheckMsg`) to the TUI's `tea.Program` (installed via `SetProgram` / `SetSink`). The model's `Update` function is identical to the local-manager case.
- `StopAll()` is a deliberate no-op in client mode (`q` is detach, not stop). `Shutdown()` is the explicit "kill everything" path used by the `d` key and `pairin down`.

## The Fleet Dashboard

`pairin dash` runs `tui.FleetModel` over an `internal/hub.Hub`. The hub is a *client* — nothing about
the supervisor's ownership model changes, there is still exactly one supervisor per project.

```
catalog (registered)  +  registry (running)
            |
       Hub.Refresh()  -- reconciles the instance SET only, never blocks
            |
   one supervise() goroutine per project
     dial -> hold -> redial with backoff
            |
     control.Client per live supervisor
            |
   instanceSink tags each event with its InstanceID
            |
      hub.Msg{ID, Inner} -> FleetModel.Update
            |
      FleetModel renders from Hub.Snapshot() values
```

Design points that matter:

- **Refresh never blocks.** `control.Dial` waits up to five seconds for a snapshot; a dozen projects
  dialed in sequence would be a minute of dead air before the first frame. Refresh only adds and
  removes instances, and each one's supervise goroutine does the dialing.
- **Rendering is from value snapshots.** `Hub.Snapshot()` returns `[]InstanceView` holding
  `[]process.ServiceView`. Service views are read *after* releasing the hub lock, because
  `Service.View` takes its own and holding two is how deadlocks start.
- **Cells are keyed, not named.** Two projects each having a service called `web` is normal, so
  `GridCell.Key` carries `instanceID + NUL + service` and selection is tracked by that. Within a
  single project `Key` is left empty and `Name` serves.
- **Logs are subscribed, not firehosed.** The hub connects with `LogsNone`. Zooming narrows that one
  instance to `LogsOnly` for the one service; leaving zoom restores `LogsNone`. The zoomed pane fills
  its backlog from the on-disk log file, so it isn't empty on arrival.
- **Quitting is not stopping.** `q` closes the dashboard and touches no supervisor. Stopping is
  always an explicit key: `x` for a service, `S` for a project.
- **Starting goes through `internal/launcher`**, the same path the CLI uses. Orphan adoption is
  automatic there — the CLI can prompt, a dashboard can't, and adopting doesn't destroy work.

## Catalog vs. Registry

Two different lists, deliberately kept apart:

| | Catalog | Instance registry |
|---|---|---|
| Question it answers | which projects do I *know about* | which supervisors are *running now* |
| Location | `$XDG_CONFIG_HOME/pairin/projects.toml` | `$XDG_STATE_HOME/pairin/instances/<hash>.json` |
| Written by | `pairin register`, and `up` (auto) | the supervisor, on start |
| Lifetime | curated; survives cleanup; hand-editable | derived; self-cleans when a PID dies |

A third file, `$XDG_STATE_HOME/pairin/ui.json`, remembers interface choices the user makes by
*using* the TUI rather than by configuring it — currently just the grid's cell style. It sits with
the state rather than the catalog on that distinction: losing it costs a keystroke, not a setting,
so every read tolerates a missing or corrupt file by falling back to the default.

The catalog is *config*, which is why it isn't under the state dir: a user should be able to wipe
`~/.local/state/pairin` without losing their project list, and should be able to keep the file in a
dotfiles repo.

`resolveConfig` (cmd/root.go) is the single entry point that decides which config a command acts on:
an explicit `--config` wins, then a catalog lookup on a positional argument, then a search upward
from the cwd. `pairin`, `up`, `attach` and `down` all route through it.

Lookup order inside `Catalog.Find` is exact name → exact config path → unique name prefix. An
ambiguous prefix returns `ErrAmbiguous` rather than picking one: starting the wrong project's
services isn't a mistake the user can undo by pressing ctrl-C.

### The project picker (`a`)

`internal/browse` lists a directory as directories plus `.pairinrc*.toml` files and nothing else —
it is not a general file browser, because the only reason to be looking is to find a config. Two
things make browsing bearable rather than tedious, and both cost a `readdir` or a small file read:
each directory carries a count of the configs inside it, so it's clear which are worth opening; and
each config carries its `[project].name`, so the choice is made by project rather than by filename.
Config counts stop after `maxProbes` directories rather than making the user wait on a huge tree.

`ProjectName` deliberately avoids `config.Load`: a config with an invalid service definition should
still be *listed* with its name, or it becomes impossible to find and fix.

The panel takes the bottom of the screen rather than all of it, so the dashboard being added to stays
in view. `browserHeight()` sizes it to its contents, capped at half the content area, and `resize()`
subtracts it from the grid's height — the same fixed-chrome discipline as everywhere else, so nothing
reflows. While it's open, keys route to `handleBrowseKey`, where `q` closes the picker rather than
quitting: a mode opened by accident should not be able to quit out from under you.

`Hub.AddProject` loads the config before writing a catalog entry, so an invalid one can't be added
and then never start. Entries added this way are pinned, since going looking for a project is the
same deliberate signal `pairin register` carries.

**Catalogue membership and dashboard visibility are different questions**, and the picker has to
answer the second one. An unpinned, stopped project has a catalog record but is not shown, so the
picker offers it (marked `unpinned — enter to pin`) rather than refusing it as already added — which
would tell the user it is in a list they can plainly see it isn't in. Only a *pinned* config is
refused. `browse.Entry` therefore carries both `Added` and `Pinned`, and `AddProject` pins an
existing entry rather than treating it as a no-op.

### Port discovery

`internal/ports` answers "what is this service listening on" by reading the kernel rather than the
config. A declared port says what a service is *supposed* to expose; this says what it does, which is
what catches a dev server whose port lives in a framework config pairin never sees.

The lookup is by **process group**. Services are started with `Setpgid`, so every descendant shares
the service's PGID however many shells and wrappers the command goes through — and `svc.PGID` is
already recorded for adopted services too. The scan reads `/proc/net/tcp{,6}` for sockets in state
`0A` (LISTEN), builds inode → port, then walks `/proc/<pid>/stat` for processes whose PGID is wanted
and maps their `socket:[N]` file descriptors back. Ports are deduplicated (a service bound on both
IPv4 and IPv6 holds two sockets on one port) and sorted.

Two implementation notes. The PGID parse starts after the *last* `)` in the stat file, because the
second field is the executable name and may itself contain spaces and parentheses. And the whole
thing is best-effort: a permission error reading one process's descriptors is not worth failing a
dashboard over, so errors yield nothing rather than propagating.

`Manager.watchPorts` runs **one** goroutine for all services, not one each: the expensive part is
walking `/proc`, and a per-service poller would multiply it by the service count. It publishes a
`PortsMsg` only when a service's ports actually change — they are identical on almost every poll, and
an event per service per tick would be noise on every connected client.

**Known blind spot:** a `docker compose up` service has its ports bound by the docker daemon, which
is in nobody's process group but its own. Those services discover nothing — the ports exist, but not
in any process the service owns. The `exposes` config field covers exactly that gap.

`mergePorts` unions declared with discovered rather than letting either win: hiding a port a service
is genuinely listening on would be a lie, and the point of declaring is to cover what discovery can't
see. Labels come only from the config and are applied to discovered ports too, so declaring
`"api:40200"` names that port whether or not discovery also finds it. Both are gated on the service
having a live PGID, so a port on a card always means the service is reachable there — a declared port
on a stopped service would be an invitation to connect to nothing.

`config.ExposeList` has a custom `UnmarshalTOML` accepting bare ports, `"label:port"` strings,
`[label, port]` pairs and `{label, port}` tables. Several shapes for one concept is usually a smell,
but the bare form shipped first and a config that worked yesterday has to keep working; the rest are
what people actually reach for. Labels are truncated to `maxPortLabel` when rendered, for the reason
the whole detail line is bounded — column width is sized to fit it.

On the wire, `control.Port` has a custom `UnmarshalJSON` that also accepts a bare number. Ports went
out as plain integers before labels existed, and a supervisor started with that build keeps sending
them for as long as it runs; without this a newer dashboard would fail to decode a snapshot from a
supervisor that is working perfectly well.

The card's detail slot carries **ports and nothing else**, blank when there are none. It briefly
carried a status fallback (`pid 1234`, `waiting`), which put two unrelated kinds of value in one
place and read as noise. The glyph already carries status, the restart counter is folded into the
name line, and the PID lives in the zoomed view's title bar.

### Pinning

Catalog entries carry an `Auto` flag, and the dashboard shows a stopped project only when it is
*pinned* (`!Auto`). `pairin register` writes pinned entries; `pairin up`'s auto-registration writes
unpinned ones. The distinction is between a commitment and a convenience: a project started once to
check something should not occupy the dashboard forever afterwards.

The field is stored **inverted** — `auto = true` in the file, absence meaning pinned — so entries
written before pinning existed keep appearing, which is what someone who ran `pairin register`
deliberately would expect. That's the only reason it isn't called `Pinned`.

The hub filters in `Refresh`: an entry that is neither running nor pinned is dropped from `found`
before instances are reconciled, so its supervise goroutine is torn down too. `Hub.SetPinned`
updates the catalog and will *add* an entry for a project that was started by path and never
registered.

A project with no cells — its config moved or deleted — would otherwise be visible but unselectable,
and so impossible to unpin. `FleetModel.refresh` gives such a group a single placeholder cell whose
service name is empty; `selection()` returns it with an empty service, which the service-level
actions treat as "this is about the project".

Catalog names are slugs (`Slugify`) rather than display names, because real project names look like
`JJC2 (localdev)` and make poor command-line arguments. Collisions are resolved by qualifying with
the parent directory before falling back to a counter — several checkouts of one project legitimately
share a display name. An explicitly chosen name is never overwritten by a later auto-registration.

## State and Registry

```
.pairin/                                $XDG_STATE_HOME/pairin/instances/
  state.json                              <hash16>.json
  supervisor.pid                          <hash16>.json
  control.sock                            ...
  supervisor.log
  logs/
    <service>.log
    <service>.log.1
```

- **`state.json`** — per-project on-disk record of services (PID, PGID, log file, started-at). Atomically written via tmp+rename. `LiveServices()` filters to PIDs that are still alive (signal 0). Used by the adopt path.
- **`supervisor.pid`** — lockfile holding the supervisor's PID. `LockHolder` reads it; `IsProcessAlive` answers whether the lock is real or stale. Removed on clean exit.
- **`control.sock`** — the TUI ↔ supervisor socket.
- **`supervisor.log`** — captures the supervisor's own stdout/stderr (it's a detached process, so this is where to look for boot-time errors).
- **`logs/<service>.log`** — every service's stdout+stderr is appended here in addition to the in-memory ring buffer. `state.RotateIfLarge` rotates to `<service>.log.1` when the file exceeds 10 MiB at supervisor start. The TUI's `tail.go` reads the last N lines on attach so reattaching doesn't show a blank pane.
- **Host registry** (`internal/state/registry.go`) — each running supervisor writes one JSON file to `$XDG_STATE_HOME/pairin/instances/<sha256(absConfigPath)[:16]>.json` (default base `~/.local/state`). `Register` is idempotent (same config path replaces its entry); `Unregister` is called on clean exit. `ListInstances` self-cleans entries whose PID is dead, so `pairin ls` and `pairin status` always reflect reality.

## Adoption

When `pairin up` finds `LockHolder == 0 || !IsProcessAlive(holder)` but `state.json` lists services whose PIDs are still alive, the user is prompted:

- **Adopt** → `spawnSupervisor(adopt=true)`. The supervisor passes `--adopt`, loads `state.json`, and for each `LiveServices` entry calls `mgr.AdoptService(idx, pid, pgid, logfile)`. Adoption sets `svc.Adopted = true`, fills in `PID`/`PGID`, opens the existing log file for continued append, and skips `cmd.Start` — but starts the same `captureOutput` / `waitForExit` / `healthcheckPoller` goroutines as a fresh start. The orphan is now a normal managed service.
- **Restart** → SIGINT (3s grace) then SIGKILL the orphans' process groups, clear `state.json`, release the lock, then spawn a fresh non-adopting supervisor.
- **Quit** → exit without touching anything.

## Goroutine Model

Each running service spawns up to 3 background goroutines. The manager coordinates them through a per-service mutex (`svc.mu`) and a generation counter (`svc.generation`).

```
                           startService(idx)
                                 |
                  +--------------+--------------+
                  |              |              |
                  v              v              v
           captureOutput   waitForExit   healthcheckPoller
           (reads stdout)  (cmd.Wait)    (polls every 2s)
                  |              |              |
                  |              |              |
                  +------+-------+-------+------+
                         |               |
                    svc.mu.Lock()   m.send(msg)
                   (write to logs,  (push to TUI
                    read/set state)  event loop)
```

### Goroutine details

| Goroutine | Started by | Lifetime | What it does |
|-----------|-----------|----------|--------------|
| `captureOutput` | `startService` | Until stdout EOF | Reads lines from process stdout pipe, writes to ring buffer under `svc.mu`, sends `LogMsg` to TUI |
| `waitForExit` | `startService` | Until `cmd.Wait()` returns | Waits for process exit, updates status under `svc.mu`, decides whether to auto-restart |
| `healthcheckPoller` | `startService` (if `healthcheck` set) | Until context cancelled or generation mismatch | Polls TCP/HTTP every 2s, updates `svc.Healthy`, triggers `tryStartWaiting` on health transitions |
| `autoRestartService` | `waitForExit` (if policy matches) | One-shot: sleep then restart | Sleeps for `restart_delay`, then calls `startService` (which spawns new goroutines) |

## Message Flow (Manager -> Sink -> TUI)

The manager communicates outward exclusively through `m.send(msg)`, which forwards to whatever `Sink` is installed. Two sinks exist:

- **Local mode**: the sink is a `tea.Program`, so messages land directly in `model.Update`.
- **Supervisor mode**: the sink is `control.Server`, which translates each `tea.Msg` into a protocol `Event` and broadcasts it. On the other end, `control.Client.apply` translates incoming events back into the same `tea.Msg` types and sends them to the TUI's `tea.Program`.

In both cases the model sees the same `tea.Msg` shapes, so the diagram below applies regardless of mode. The TUI never calls manager methods directly — only `Backend.RestartService`, `Backend.StopAll`, and `Backend.Shutdown` (the latter is a no-op locally and a `ReqShutdown` request remotely).

```
Manager goroutines                    TUI event loop (model.Update)
-------------------                   ----------------------------

captureOutput:
  svc.Logs.Add(line)
  m.send(LogMsg{idx, line})    ---->  panes[idx].AppendLine(line)

startService:
  m.send(StatusMsg{idx, s})    ---->  (triggers re-render, status
                                       is already on svc struct)

waitForExit:
  svc.Status = StatusCrashed
  m.send(StatusMsg{idx, s})    ---->  (triggers re-render)

healthcheckPoller:
  svc.Healthy = true/false
  m.send(HealthCheckMsg{})     ---->  (triggers re-render)
  if healthy:
    tryStartWaiting()                 (may start waiting services)

RestartService (tea.Cmd):
  stopService -> startService
  return ServiceRestartedMsg   ---->  panes[idx].SyncFromBuffer()

StopAll (tea.Cmd):
  stop all services in parallel
  return tea.QuitMsg           ---->  Bubble Tea exits
```

### Message types

| Message | Sent by | Purpose |
|---------|---------|---------|
| `LogMsg{Index, Line}` | `captureOutput` | Append a log line to a pane |
| `StatusMsg{Index, Status, PID}` | `startService`, `waitForExit` | Status changed, trigger re-render |
| `AllStartedMsg{}` | `StartAll` | All services have been started or set to waiting |
| `ServiceRestartedMsg{Index}` | `RestartService` | Manual restart complete, sync pane from buffer |
| `HealthCheckMsg{Index, Healthy}` | `healthcheckPoller` | Health state changed, trigger re-render |

## Service Lifecycle

A service moves through these states:

```
                             startService()
                                  |
   +-------+    +----------+     v      +-----------+
   |Stopped| -->| Starting | -------->  |  Running  |
   +-------+    +----------+            +-----------+
       ^                                  |       |
       |                         exit ok  |       | exit err
       |                                  v       v
       |                            +---------+  +---------+
       |                            | Stopped |  | Crashed |
       |                            +---------+  +---------+
       |                                  |            |
       |      (policy matches?)           +-----+------+
       |                                        |
       +-------------- no ------<-----  shouldAutoRestart?
                                                |
                                               yes
                                                |
                                                v
                                        +--------------+
                                        |  Restarting  |  (sleep restart_delay)
                                        +--------------+
                                                |
                                                v
                                          startService()
                                          (back to Starting)
```

Services with `depends_on` have an additional entry state:

```
    StartAll()
        |
        v
   deps healthy? --yes--> startService()
        |
        no
        |
        v
   +---------+    dep becomes healthy    +----------+
   | Waiting | -----------------------> | Starting |
   +---------+   (via tryStartWaiting)   +----------+
   (magenta)
```

## Generation Counter

The generation counter (`svc.generation`) prevents stale goroutines from corrupting state after a restart. It is incremented at the start of `startService()`.

```
Timeline for service "web":

  gen=1: startService()
           -> captureOutput (gen=1)
           -> waitForExit (gen=1)
           -> healthcheckPoller (gen=1)

  User presses 'r' (manual restart):
    stopService()  -> sends SIGINT, waits for exit
    startService() -> gen becomes 2

  gen=2: new goroutines start
           -> captureOutput (gen=2)
           -> waitForExit (gen=2)
           -> healthcheckPoller (gen=2)

  If a gen=1 goroutine is still running:
    it checks svc.generation != gen -> exits without modifying state
```

Places that check the generation guard:
- `waitForExit`: after `cmd.Wait()` returns, before updating status
- `healthcheckPoller`: each poll cycle, before updating `svc.Healthy`
- `autoRestartService`: before and after the cooldown sleep

## Shutdown Flow

When the user presses `q` or `ctrl+c`:

```
handleKey("q")
    |
    v
return tea.Cmd (async):          <-- runs in goroutine, does NOT block Update loop
    |
    v
mgr.StopAll()
    |
    +---> goroutine per service (parallel via WaitGroup)
    |       |
    |       v
    |     stopService(idx):
    |       1. Lock svc.mu
    |       2. Cancel healthcheck poller
    |       3. Set StatusStopped, save cmd ref
    |       4. Unlock svc.mu          <-- critical: unlock BEFORE waiting
    |       5. Send SIGINT to process group (-pgid)
    |       6. Wait up to 5s for exit
    |       7. If timeout: send SIGKILL
    |       8. Lock svc.mu, clear PID/cmd, unlock
    |       |
    |       v
    |     (done)
    |
    v
  return tea.QuitMsg{}   --> Bubble Tea exits p.Run()
```

Key design points:
- **Async shutdown**: `StopAll` runs in a `tea.Cmd` goroutine so the Bubble Tea event loop keeps processing messages. This prevents `p.Send()` calls from background goroutines from blocking on a full channel.
- **Mutex released before wait**: `stopService` releases `svc.mu` before waiting for the process to exit. This lets `captureOutput` continue draining the stdout pipe, preventing a deadlock where the process blocks on write because the pipe buffer is full.
- **Parallel stop**: All services are stopped concurrently via goroutines + WaitGroup, so total shutdown time is bounded by the slowest service (max 5s), not the sum.
- **Process groups**: Each service runs with `Setpgid: true`. SIGINT/SIGKILL are sent to `-pgid` (negative) to kill the entire process group, including child processes.

## Config Validation

`config.Validate()` runs at load time and checks:

```
For each service:
  1. depends_on references exist?        (name lookup)
  2. depended-on service has healthcheck? (required for dependency tracking)
  3. restart policy is valid?             (no, always, on-failure, on-success)
  4. restart_delay is parseable?          (Go duration string)
  5. max_restarts is non-negative?

Then:
  6. Circular dependency detection        (Kahn's algorithm / topological sort)
     - Build adjacency list + in-degree map
     - BFS from nodes with in-degree 0
     - If visited < total services -> cycle exists
```

## TUI Layout

```
+---------------------------------------------------------------+
| Project Name                    * db  * api  * web  * vk  * cp |  <- header
+---------------------------------------------------------------+
| +-----------------------------------------------------------+ |
| | service-name  branch  status  [healthy]  PID 1234         | |  <- title bar
| |                                                           | |
| | log line 1                                                | |  <- viewport
| | log line 2                                                | |     (scrollable)
| | log line 3                                                | |
| +-----------------------------------------------------------+ |  <- pane border
| +-----------------------------------------------------------+ |     (blue if active,
| | ...next service...                                        | |      gray if inactive)
| +-----------------------------------------------------------+ |
+---------------------------------------------------------------+
| tab cycle  r restart  z zoom  q detach  d down                |  <- footer
+---------------------------------------------------------------+
```

**Split view**: All panes stacked vertically, height divided evenly.
**Grid view**: A compact status cell per service — see below.
**Focus view** (press 1-9, or `z` to toggle): Single pane fills the screen, scrollable with j/k/arrows.

The initial view is **chosen, not fixed**: if `availableHeight / len(panes)` is below
`minSplitPaneHeight` (6), the model opens in grid view, because twenty two-line viewports show
nothing. Pressing `v` sets `viewChosen`, after which resizes leave the choice alone.

Chrome is a **fixed** number of lines — glyph key, header, status, key hints — and the content area
is padded to fill what remains. Two consequences, both deliberate: the glyph key sits at the top
where it stays put (below the grid it slid down the screen every time a project gained a row), and a
status message gets its own line *above* the key hints rather than replacing them, without the rest
of the screen reflowing to make room.

```
+---------------------------------------------------------------+
| ● up  ◍ unhealthy  ◐ starting  ⋯ waiting  ⟳ restarting  ...    |  <- glyph key, fixed
| bigproject   20 services · 18 up            filter: api        |  <- header + tally
|  ● postgres     ● redis       ● api         ◍ worker           |
| ›● migrations   ○ mailhog     ✕ scheduler   ⟳ indexer 3/5      |  <- › marks selection
|  ⋯ reporter     ● gateway                                      |
|                                                                |  <- padded to fixed height
| restart scheduler                                              |  <- status (blank when idle)
| ↑↓←→ move  z zoom  r restart  c clear logs  b cells  q detach  |  <- key hints
+---------------------------------------------------------------+
```

### Grid (`tui.Grid`)

The grid is shared with the fleet dashboard, which is why it takes *groups* of cells rather than a
flat list — one group per project there, exactly one here.

Its only stored state is `groups`, `filter`, `width`, `height`, `cellStyle` and a flat `selected`
index. **Everything else is derived on each render.** Bubble Tea passes `View()` a *copy* of the
model, so a column count or scroll offset computed during a render would be discarded before the
next keystroke. `DashboardModel.Update` calls `refreshGrid()` (not `View`) to rebuild cells from
live service state, for the same reason.

**`Grid.layout()` is the single source of geometry**, and both rendering and navigation read it.
That is not incidental tidiness — it's a bug fix. The two used to compute geometry independently:
rendering broke rows at every group boundary, while `Move` did flat arithmetic over a uniform column
count (`selected + dy*cols`). In the fleet view, pressing down out of a project with fewer services
than there were columns jumped a full row's worth of cells and skipped whole projects. `layout` now
produces the visual rows explicitly, including the breaks between groups, so vertical movement steps
between adjacent *rendered* rows and clamps into short ones. `TestGridMoveDownCrossesGroupBoundary`
fails against the old arithmetic.

Cell styles (`CellPlain`, `CellBoxed`, `CellCard`, cycled with `b`) change a row's height from one
screen line to three or four. Navigation is unaffected — a row is one row however tall it draws —
because `Move` walks `layout.rows` while `window` scrolls by `layout.lineOfCell`, which points at
the *last* screen line of a row so scrolling brings the whole block into view. Card cells measure
their detail text as well as their names when sizing columns, since `pid 2995280` is wider than
`api`.

Selection is tracked by **name**, not index, so it survives filtering — `syncGridSelection` and
`syncActiveFromGrid` keep `m.active` and the grid pointing at the same service in both directions.

### Keys

| Key            | Action                                                   |
|----------------|----------------------------------------------------------|
| `1`-`9`        | Focus pane N                                             |
| `v`            | Switch between split and grid                            |
| `z` / `enter`  | Toggle zoom (split or grid ↔ focus on the selection)     |
| `esc`          | Leave focus view, or clear the grid filter               |
| `tab` / `S-tab`| Cycle selection forward / backward                       |
| `←↑↓→`/`hjkl`  | Move the selection (grid) or scroll (split / focus)      |
| `/`            | Filter by name (grid only); swallows keys while typing   |
| `r`            | Restart the selected service                             |
| `q` / `ctrl+c` | Detach the TUI; supervisor + services keep running       |
| `d`            | Shut down: stop every service and exit the supervisor    |

### View rendering

```
model.View()
    |
    +-> renderHeader()     Project name + status dots per service
    |                      Dot shape/color reflects: running, crashed, starting,
    |                      restarting, waiting, unhealthy (half-circle)
    |
    +-> for each pane:
    |     pane.RenderSplit(active)   or   pane.RenderFocus()
    |       |
    |       +-> titleLine()          Name + branch + status + health + PID
    |       +-> viewport.View()      Scrollable log content
    |
    +-> renderFooter()     Number shortcuts + key hints
```

## Ring Buffer

Logs are stored in a fixed-size circular buffer (1000 lines) per service:

```
capacity = 5, after adding a,b,c,d,e,f,g:

  internal:  [f] [g] [c] [d] [e]
              ^head
  Lines() -> [c] [d] [e] [f] [g]   (oldest to newest)
```

- Bounded memory: no unbounded growth from long-running services
- `Lines()` returns a copy in chronological order
- All access is guarded by `svc.mu`
- On manual restart, the pane calls `SyncFromBuffer()` to reload from the buffer (which was cleared and started fresh)
- A persistent on-disk copy is also written to `.pairin/logs/<service>.log` (rotated to `.log.1` past 10 MiB at supervisor start), so a freshly-attached TUI can call `tui/tail.go`'s `tailLines` to preload the last 500 lines without the supervisor having to replay them

## Healthcheck System

```
healthcheck config             dispatch              protocol
-----------------             --------              --------
"tcp://host:port"    ---->    checkTCP(addr)    -->  net.DialTimeout (1s)
"http://url"         ---->    checkHTTP(url)    -->  http.Get (2s), expect 2xx
"https://url"        ---->    checkHTTP(url)    -->  http.Get (2s), expect 2xx
```

The healthcheck poller runs as a goroutine per service with a healthcheck configured:

```
every 2 seconds:
    |
    v
  runHealthcheck(hc)
    |
    v
  result changed?
    |
   yes --> update svc.Healthy
    |        |
    |        +--> send HealthCheckMsg to TUI
    |        |
    |        +--> if became healthy: tryStartWaiting()
    |                                   |
    |                                   v
    |                              for each service in StatusWaiting:
    |                                if allDepsHealthy(i) -> startService(i)
    |
    no --> do nothing
```

## Auto-Restart System

```
Process exits
    |
    v
waitForExit:
    cmd.Wait() returns
    |
    v
  update svc.Status (Crashed or Stopped)
    |
    v
  shouldAutoRestart(svc, exitedWithFailure)?
    |
    +-- check policy: "no" -> false
    |   "always" -> true
    |   "on-failure" -> exitedWithFailure
    |   "on-success" -> !exitedWithFailure
    |
    +-- check status: only Crashed/Stopped allowed
    |                 (prevents restart if manually stopped mid-flow)
    |
    +-- check max_restarts: RestartCount >= MaxRestarts -> false
    |                       MaxRestarts == 0 -> unlimited
    |
    v
  autoRestartService(idx, gen):
    1. Check generation (bail if stale)
    2. Set StatusRestarting, increment RestartCount
    3. Log "restarting in Xs..."
    4. Sleep(restart_delay)                     <-- cooldown period
    5. Check generation again (bail if stale)   <-- manual restart during sleep?
    6. Reset svc.Healthy = false
    7. Call startService(idx)                   <-- new generation, new goroutines
```

Manual restart (`r` key) resets `RestartCount` to 0, giving the service a fresh set of attempts.

## Thread Safety Summary

| Lock | Protects | Held by |
|------|----------|---------|
| `svc.mu` | `Status`, `PID`, `cmd`, `Branch`, `Logs`, `Healthy`, `generation`, `RestartCount`, `healthCancel` | `startService`, `stopService`, `captureOutput`, `waitForExit`, `healthcheckPoller`, `autoRestartService`, `GetLines`, `View`, the `Apply*` mirror mutators |
| `m.mu` (manager) | `m.sink`, `m.err` | `send`, `SetSink`/`SetProgram`, `Error` |
| `s.mu` (control.Server) | `s.clients`, `s.closed`, `s.ln` | `acceptLoop`, `handle`, `broadcast`, `Shutdown` |
| `c.mu` (serverClient) | `dropped`, `resync` | `enqueue`, `writeLoop` |
| `c.wmu` (serverClient) | the JSON encoder | `write` (writer goroutine and `Shutdown`) |
| `c.mu` (control.Client) | `c.conn`, `c.enc`, `c.done`, `c.sink`, `c.err`, `ProjectName` | `Reconnect`, `send`, `readLoop`, `forward`, `SetSink`, `Done` |

**Critical invariant: `svc.mu` is never held across `m.send()`.** The sink on the other end may be a socket broadcaster, and a client that has stopped reading can block that write. Holding the service lock across it would pin the mutex for as long as the client stays wedged, stalling the log tailer, the healthcheck poller and the stop path with it. `startService` and `waitForExit` therefore *collect* their status messages while locked and publish them after unlocking — see `startServiceLocked`, which returns `[]tea.Msg` for exactly this reason. Adding a new `m.send()` call inside a locked region reintroduces the stall.

**Rendering reads value snapshots, never live structs.** `Service.View()` returns a `ServiceView` copy taken under `svc.mu`. The TUI renders from that, because the manager's goroutines (and, on a mirror, the control client's read loop) mutate those fields while a render is in flight. Reading `svc.Status` or `svc.Branch` directly from render code is a data race.

`control.Server.broadcast` releases `s.mu` before touching any client, and only ever enqueues, so a stuck or slow client cannot block the manager's event path.

## Crash Reporting

A panic used to make pairin disappear. In the TUI it was worse than a hard crash: Bubble Tea's built-in handler prints a stack trace to a screen it is simultaneously tearing down, then lets `Run` return a **nil** error — so pairin exited zero and the terminal (or tmux pane) simply closed with nothing recorded.

`internal/crash` fixes that:

- `crash.Guard(context)` is deferred at the top of every long-lived goroutine — the log tailer, `waitForExit`, the healthcheck poller, `autoRestartService`, `watchAdopted`, `persistState`, `StopAll`'s workers, and both sides of the control socket. The goroutine dies; the process survives.
- Reports go to `$XDG_STATE_HOME/pairin/crash-<timestamp>-<pid>.log` with the version, PID, cwd, context and full stack.
- The TUI runs with `tea.WithoutCatchPanics()` so pairin's own handler runs instead: it calls `p.Kill()` to restore the terminal, writes a report, and returns an error naming the report path — and says plainly that services are still running, since the alarming reading is the wrong one.
- `runSupervisor` has the same net at its top level, because a panic there would take every managed service down with it.
- TUI commands are wrapped in `guarded(...)`, since Bubble Tea runs each command on its own goroutine where an unrecovered panic is fatal to the process.
