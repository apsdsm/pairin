# CLAUDE.md

## Overview

pairin is a terminal-based local development process manager. It reads a `.pairinrc.toml` config, spawns a detached **supervisor** that owns all services as subprocesses, and attaches a split-pane TUI **client** to it over a unix-domain socket. The TUI can detach and reattach without restarting services, and a host-wide registry lets `pairin ls` discover supervisors across every project on the machine.

## Commands

```bash
go build -o pairin .        # Build binary
go run main.go              # Run directly
go install .                # Install to GOPATH/bin
go test ./...               # Run tests
```

## Project Structure

```
main.go                        # Entry point
cmd/
  root.go                      # `pairin` (default): start-or-attach flow, stale-state prompt, supervisor spawn
  up.go                        # `pairin up` alias for the default
  attach.go                    # `pairin attach`: connect a TUI to an existing supervisor
  down.go                      # `pairin down`: tell the supervisor to stop everything and exit
  ls.go                        # `pairin ls`: list supervisors across the host
  projects.go                  # `pairin register` / `unregister` / `projects`: the project catalog
  status.go                    # `pairin status`: per-service status across every supervisor
  dash.go                      # `pairin dash`: host-wide dashboard over a hub
  supervisor.go                # Hidden `pairin supervisor`: the detached background worker
  version.go                   # Version constant + `pairin version`
internal/
  catalog/catalog.go           # User's registered projects: $XDG_CONFIG_HOME/pairin/projects.toml
  hub/hub.go                   # Connections to every supervisor on the host; tagged events, per-instance reconnect
  launcher/launcher.go         # Spawning detached supervisors; shared by the CLI and the dashboard
  config/config.go             # TOML config loading, dir resolution, dependency/cycle validation
  crash/crash.go               # Panic capture: Guard for goroutines, reports under $XDG_STATE_HOME/pairin/
  process/manager.go           # Process lifecycle, log capture, healthcheck polling, auto-restart, adoption
  control/
    protocol.go                # NDJSON wire format: Request / Event / Snapshot types
    server.go                  # Supervisor side: socket listener, broadcasts manager events, handles requests
    client.go                  # TUI side: dials the socket, mirrors services, re-emits events as tea.Msg
  state/
    state.go                   # .pairin/state.json + supervisor.pid lock + IsProcessAlive helpers
    registry.go                # Host-wide registry under $XDG_STATE_HOME/pairin/instances/
    logfile.go                 # Per-service log paths and 10 MiB rotation threshold
  tui/
    model.go                   # Bubble Tea model: keys, layout, split/grid/focus views; talks to a Backend interface
    grid.go                    # Compact status grid (groups of cells), shared by the project and fleet models
    fleet.go                   # `pairin dash` model: every project on the host, over a hub
    pane.go                    # Single service pane: viewport, title bar, log rendering
    tail.go                    # Preload last N lines from on-disk log files when attaching
    styles.go                  # Lipgloss styles and color mapping
    messages.go                # Re-exports (message types live in process package)
```

## Architecture

`pairin` always splits into two processes once services are running: a long-lived **supervisor** that owns the `process.Manager`, and a **TUI client** that attaches to it. The TUI talks to its backend through the `tui.Backend` interface — locally that backend is a `process.Manager`-like wrapper, but in normal operation it's a `control.Client` driving an NDJSON socket.

```
pairin (foreground)                  pairin supervisor (detached, Setsid)
-------------------                  -------------------------------------
config.Load()                        config.LoadFrom(--config)
  |                                    |
spawnSupervisor() (re-execs self) -->  state.Register(Instance)
  |                                    process.NewManager(cfg)
waitForSupervisor()                    control.NewServer(mgr, cfg)
  |                                    server.Start(.pairin/control.sock)
control.Dial(socket) ---------------> accept loop (one goroutine per client)
  |                                    mgr.SetSink(server)  // events fan out
tui.NewDashboardModel(cfg, client)     mgr.StartAll()
p.Run()  <-- LogMsg/StatusMsg/...      ^
                                       |
                                       sigCh / server.Done() -> StopAll + Shutdown
```

- **config** — Loads `.pairinrc.toml`, resolves relative service directories against the config file. `Load()` searches cwd up to root; `LoadFrom(path)` is used by the supervisor to load a specific file. Validates dependency references, ensures depended-on services have healthchecks, and detects circular dependencies (Kahn's algorithm).
- **process.Manager** — Owns all `Service` structs. Each service runs in its own process group (`Setpgid`). Logs are stored in a fixed-size ring buffer (1000 lines) **and** appended to `.pairin/logs/<service>.log` (rotated past 10 MiB). The Manager publishes events via a `Sink` interface — locally that's a `tea.Program`, in supervisor mode it's the `control.Server`. Services with `depends_on` enter `StatusWaiting` until their dependencies pass healthchecks. Healthcheck polling (TCP dial or HTTP GET) runs every 2 seconds. `AdoptService` re-attaches the manager to a PID/PGID inherited from a previous supervisor (no new process is spawned).
- **control.Server** — Listens on `.pairin/control.sock`, sends every connecting client a `Snapshot`, then broadcasts `EvtStatus` / `EvtLog` / `EvtHealth` events translated from `tea.Msg`s the manager produces. Handles `ReqRestart`, `ReqStop`, `ReqStart`, `ReqShutdown` from clients.
- **control.Client** — Dials the socket, blocks until the initial snapshot arrives, mirrors the services into local `*process.Service` structs (via `process.NewMirrorService`), and re-emits incoming events as the same `tea.Msg` types the Manager produces — so the TUI's `Update` loop is identical whether services run in-process or remote.
- **state** — `.pairin/state.json` records each adopted/running service (PID, PGID, log file). `.pairin/supervisor.pid` is the lockfile that `pairin up` checks to decide between "attach" and "spawn". The host-wide instance registry lives outside the project — under `$XDG_STATE_HOME/pairin/instances/<hash>.json` (default `~/.local/state/pairin/instances/`) — which is what `pairin ls` reads. `ListInstances()` self-cleans entries whose supervisor PID is dead.
- **tui.DashboardModel** — Bubble Tea model with two view modes: split (all panes) and focus (single pane full-screen, toggled by `z` or by pressing a number key). Uses the `Backend` interface so it doesn't know whether it's local or remote. On attach, panes call `PreloadHistory` to fill from the on-disk log file (`tui/tail.go`) so reattaching to a long-running supervisor doesn't show a blank screen.
- **tui.Pane** — Wraps a `bubbles/viewport` for scrollable log display with a title bar showing service name, git branch, status, health indicator, and PID.

## Service Dependencies & Healthchecks

Services can declare dependencies and healthchecks in `.pairinrc.toml`:

```toml
[[services]]
name = "database"
cmd = "docker compose up postgres"
healthcheck = "tcp://localhost:5432"

[[services]]
name = "web"
cmd = "bun run dev"
depends_on = ["database"]
```

- **`healthcheck`** - `tcp://host:port` (1s dial timeout) or `http(s)://url` (2s GET, expects 2xx)
- **`depends_on`** - list of service names that must be healthy before this service starts
- Services with unmet deps enter `StatusWaiting` (magenta) and auto-start when deps become healthy
- Healthcheck is orthogonal to status: a service can be `Running` but not yet `Healthy`
- No cascade restarts — restarting a dependency doesn't auto-restart dependents

## Auto-Restart (systemd-style)

Services can be configured to automatically restart when they exit:

```toml
[[services]]
name = "web"
cmd = "bun run dev"
restart = "on-failure"
restart_delay = "5s"
max_restarts = 5
```

- **`restart`** - restart policy: `"no"` (default), `"always"`, `"on-failure"`, `"on-success"` — mirrors systemd's `Restart=` directive
- **`restart_delay`** - Go duration string (e.g. `"5s"`, `"500ms"`), cooldown before retrying (default: `"3s"`)
- **`max_restarts`** - maximum number of consecutive auto-restarts before giving up; `0` = unlimited (default: `0`)
- During the cooldown, the service enters `StatusRestarting` (yellow) in the TUI
- The title bar shows restart count (e.g. `restarting 3/5` or `restarting #3`)
- Manual restart (`r` key) resets the restart counter
- Intentional stops (via `stopService`) do not trigger auto-restart

## Detach / Attach / Adopt

- The supervisor is spawned with `Setsid: true` (new session leader), so it survives the parent TUI exiting and SSH disconnects.
- `pairin -d` / `pairin up -d` spawns (or confirms) the supervisor and exits immediately without attaching a TUI — same end state as starting normally and pressing `q`. Idempotent: if a supervisor is already up, it prints the existing PID and returns.
- `pairin --clear-logs` / `pairin up --clear-logs` deletes everything in `.pairin/logs/` (including rotated `.log.1` files) before spawning the supervisor, so the TUI doesn't preload old history. Errors out if a supervisor is already running, since its open fds would keep writing to the unlinked files.
- `q` / `ctrl+c` in the TUI is **detach**: it tears down the client, leaves the supervisor and all services running. `d` is **shut down**: sends `ReqShutdown`, supervisor calls `StopAll` and exits. `pairin down` is the out-of-TUI equivalent of `d`.
- If `pairin up` finds a stale `supervisor.pid` whose process is gone but services in `state.json` are still alive, it prompts: **A**dopt (spawn a new supervisor with `--adopt`, which calls `mgr.AdoptService` per surviving PID/PGID), **R**estart (SIGINT then SIGKILL the orphans, clear state), or **Q**uit.
- Adopted services have `svc.Adopted = true`. The manager skips `exec.Command.Start` for them and instead resumes log capture and healthchecking against the existing PID.

## Key Design Decisions

- **`svc.mu` is never held across `m.send()`** — the sink may be a socket, and a client that stops reading would otherwise pin the mutex and stall the tailer, healthchecks and the stop path. `startServiceLocked` returns `[]tea.Msg` for the caller to publish after unlocking; do the same for any new send inside a locked region.
- **`Grid.layout()` is the only source of grid geometry** — rendering and navigation both read it. They once computed it separately and drifted, so vertical movement skipped whole groups
- **Nothing computed in `View()` may be stored** — Bubble Tea renders from a copy of the model, so grid layout, column counts and scroll offsets are derived on each render; cell data is rebuilt in `Update` via `refreshGrid()`
- **Render from `Service.View()`, never from live `Service` fields** — those are mutated concurrently by the manager's goroutines and the control client's read loop.
- Each control-socket client has its own send queue and writer goroutine; `broadcast` only enqueues. On overflow, events are dropped and the client is resynced with a fresh snapshot.
- Panics are captured (`internal/crash`) rather than allowed to vanish: the TUI runs with `tea.WithoutCatchPanics()` because Bubble Tea's own handler exits zero with no record
- Process groups (`Setpgid`) ensure child processes of services are also cleaned up on stop
- SIGINT with 5-second timeout before SIGKILL for graceful shutdown
- Generation counter on Service prevents stale goroutines from updating state after a restart
- Ring buffer avoids unbounded memory growth from long-running services
- On-disk per-service log files (`.pairin/logs/<name>.log`, rotated past 10 MiB) let a reattached TUI preload history via `tui/tail.go`
- Healthcheck poller uses the same generation guard to prevent stale goroutines after restart
- Auto-restart uses the same generation guard — if a manual restart or stop happens during the cooldown sleep, the stale auto-restart goroutine exits without acting
- The TUI's `Backend` interface decouples it from the manager: a local `*process.Manager` and a remote `*control.Client` are interchangeable, which is why the same Bubble Tea model handles both modes

## Versioning

The version is defined as a `const` in `cmd/version.go`. When bumping the version:
1. Update the `Version` constant in `cmd/version.go`
2. Create a git tag matching the version (e.g. `git tag v0.1.0`)
3. Push the tag (e.g. `git push origin v0.1.0`)

## Dependencies

- `charmbracelet/bubbletea` - TUI framework
- `charmbracelet/bubbles` - Viewport component for scrollable log panes
- `charmbracelet/lipgloss` - Terminal styling
- `spf13/cobra` - CLI framework
- `BurntSushi/toml` - Config parsing
