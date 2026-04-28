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
  process/
    manager.go                    Process lifecycle, log capture, healthchecks, auto-restart, adoption, mirror services
    manager_test.go               Ring buffer, healthcheck, dependency, restart tests
  control/
    protocol.go                   NDJSON wire format: Request / Event / Snapshot types
    server.go                     Supervisor: socket listener, Sink that broadcasts manager events
    client.go                     TUI: dial socket, mirror services, re-emit events as tea.Msg
  state/
    state.go                      .pairin/state.json + supervisor.pid + IsProcessAlive helpers
    registry.go                   Host-wide instance registry under $XDG_STATE_HOME/pairin/instances/
    logfile.go                    Per-service log paths and 10 MiB rotation threshold
  tui/
    model.go                      Bubble Tea model: keys, layout, split/focus views; uses Backend interface
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
```

A new client always receives an `EvtSnapshot` first, then a stream of incremental events. The snapshot contains everything the TUI needs to render before any further events arrive (project name, started-at timestamp, and per-service name/short/color/dir/cmd/status/PID/branch/health/adopted/log_file/restart_count/max_restarts/depends_on).

### Server (`control.Server`)

- Wraps a `*process.Manager`. Once `Start(socketPath)` succeeds, the server installs itself as the manager's `Sink`, so every `tea.Msg` the manager produces is routed through `eventFor` and broadcast as a protocol `Event` to every connected client.
- Maintains a `clients map[*serverClient]struct{}` under a mutex. Broadcast iterates a snapshot copy of that map so writes never hold the lock during I/O.
- `dispatch` handles incoming requests by looking up the service by name (clients never see indices), then calling the corresponding manager method on a fresh goroutine — never blocking the read loop.
- `Shutdown` is idempotent. It closes the listener, broadcasts a final `EvtShutdown` event, closes every client conn, and `close()`s the `shutdown` channel so `Done()` waiters return.

### Client (`control.Client`)

- `Dial` opens the socket, starts a read loop, and blocks until either the initial snapshot arrives, the connection drops, or 5 seconds elapse.
- The first snapshot allocates `*process.Service` mirrors via `process.NewMirrorService` and builds a `nameToIdx` map. Subsequent snapshots **update fields in place** rather than rebuilding the slice, so pointers held by the TUI stay valid.
- Each incoming event mutates the mirror service and forwards a translated `tea.Msg` (`StatusMsg`, `LogMsg`, `HealthCheckMsg`) to the TUI's `tea.Program` (installed via `SetProgram` / `SetSink`). The model's `Update` function is identical to the local-manager case.
- `StopAll()` is a deliberate no-op in client mode (`q` is detach, not stop). `Shutdown()` is the explicit "kill everything" path used by the `d` key and `pairin down`.

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

**Split view** (default): All panes stacked vertically, height divided evenly.
**Focus view** (press 1-9, or `z` to toggle): Single pane fills the screen, scrollable with j/k/arrows.

### Keys

| Key            | Action                                                   |
|----------------|----------------------------------------------------------|
| `1`-`9`        | Focus pane N                                             |
| `z`            | Toggle zoom (split ↔ focus on the active pane)           |
| `tab` / `S-tab`| Cycle active pane forward / backward                     |
| `r`            | Restart the active service                               |
| `↑`/`k`, `↓`/`j` | Scroll the active pane                                 |
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
| `svc.mu` | `Status`, `PID`, `cmd`, `Branch`, `Logs`, `Healthy`, `generation`, `RestartCount`, `healthCancel` | `startService`, `stopService`, `captureOutput`, `waitForExit`, `healthcheckPoller`, `autoRestartService`, `GetLines` |
| `m.mu` (manager) | `m.sink`, `m.err` | `send`, `SetSink`/`SetProgram`, `Error` |
| `s.mu` (control.Server) | `s.clients`, `s.closed`, `s.ln` | `acceptLoop`, `handle`, `broadcast`, `Shutdown` |
| `c.mu` (control.Client) | `c.conn`, `c.enc`, `c.sink`, `c.err` | `Dial`, `send`, `readLoop`, `forward`, `SetSink` |

Critical invariant: `svc.mu` is **never held** while waiting for a process to exit or while calling `m.send()` from `captureOutput`. This prevents deadlocks between pipe draining and process shutdown. Similarly, `control.Server.broadcast` releases `s.mu` before writing to any client, so a stuck/slow client can't block the manager's event path.
