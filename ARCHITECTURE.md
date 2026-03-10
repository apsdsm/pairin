# Architecture

This document describes how pairin's code is structured, how the pieces connect, and the concurrency model that holds it all together.

## File Map

```
main.go                           Entry point, calls cmd.Execute()
cmd/
  root.go                         Cobra root command, wires config -> manager -> TUI
  version.go                      Version constant and `pairin version` subcommand
internal/
  config/
    config.go                     TOML config loading, dir resolution, validation
    config_test.go                Validation tests (deps, cycles, restart policies)
  process/
    manager.go                    Process lifecycle, log capture, healthchecks, auto-restart
    manager_test.go               Ring buffer, healthcheck, dependency, restart tests
  tui/
    model.go                      Bubble Tea model: Update loop, keyboard handling, layout
    pane.go                       Single service pane: viewport, title bar, log rendering
    styles.go                     Lipgloss styles, color map
    messages.go                   Re-export comment (message types live in process package)
```

## Boot Sequence

```
main.go
  |
  v
cmd.Execute()
  |
  v
config.Load()                     Find .pairinrc.toml (walks cwd -> root)
  |                               Parse TOML, resolve relative dirs, validate
  v
process.NewManager(cfg)           Create Service structs, build name->index map
  |
  v
tui.NewDashboardModel(cfg, mgr)  Create Pane per service
  |
  v
tea.NewProgram(model)             Create Bubble Tea program (alt screen)
  |
  v
mgr.SetProgram(p)                Give manager a handle to send messages to TUI
  |
  v
p.Run()                          Start event loop, calls model.Init()
  |                                |
  |                                v
  |                              mgr.StartAll()   (returned as tea.Cmd, runs in goroutine)
  |                                |
  |                                +--> for each service:
  |                                       no deps? -> startService(i)
  |                                       has deps? -> set StatusWaiting
  |
  v
Event loop runs until tea.QuitMsg
  |
  v
mgr.Error() check, exit
```

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

## Message Flow (Manager -> TUI)

The manager communicates with the TUI exclusively through Bubble Tea messages sent via `m.send()` -> `p.Send()`. The TUI never calls manager methods directly except for `StopAll()` and `RestartService()`.

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
| 1 db  2 api  3 web  4 vk  5 cp  tab cycle  r restart  q quit |  <- footer
+---------------------------------------------------------------+
```

**Split view** (default): All panes stacked vertically, height divided evenly.
**Focus view** (press 1-9): Single pane fills the screen, scrollable with j/k/arrows.

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
| `m.mu` | `m.program`, `m.err` | `send`, `SetProgram`, `Error` |

Critical invariant: `svc.mu` is **never held** while waiting for a process to exit or while calling `m.send()` from `captureOutput`. This prevents deadlocks between pipe draining and process shutdown.
