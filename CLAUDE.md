# CLAUDE.md

## Overview

pairin is a terminal-based local development process manager. It reads a `.pairinrc.toml` config, launches multiple services as subprocesses, and displays their output in a split-pane TUI built with Bubble Tea.

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
cmd/root.go                    # Cobra root command, wires config -> manager -> TUI
internal/
  config/config.go             # TOML config loading (.pairinrc.toml), directory resolution, dependency validation
  process/manager.go           # Process lifecycle (start/stop/restart), log capture, healthcheck polling, dependency-aware startup
  tui/
    model.go                   # Bubble Tea model: keyboard handling, layout, split/focus views
    pane.go                    # Single service pane: viewport, title bar, log rendering
    styles.go                  # Lipgloss styles and color mapping
    messages.go                # Re-exports (message types live in process package)
```

## Architecture

```
main.go -> cmd.Execute() -> config.Load() -> process.NewManager() -> tui.NewDashboardModel()
                                                  |
                                          Starts subprocesses
                                          Captures stdout/stderr
                                          Sends LogMsg/StatusMsg to TUI via tea.Program
```

- **config** - Loads `.pairinrc.toml`, resolves relative service directories against config file location. Searches from cwd up to filesystem root. Validates dependency references, ensures depended-on services have healthchecks, and detects circular dependencies (Kahn's algorithm).
- **process.Manager** - Owns all `Service` structs. Each service runs in its own process group (`Setpgid`). Logs are stored in a fixed-size ring buffer (1000 lines). Sends Bubble Tea messages (`LogMsg`, `StatusMsg`, `AllStartedMsg`, `ServiceRestartedMsg`, `HealthCheckMsg`) to drive the TUI. Services with `depends_on` enter `StatusWaiting` until their dependencies pass healthchecks. Healthcheck polling (TCP dial or HTTP GET) runs every 2 seconds per service.
- **tui.DashboardModel** - Bubble Tea model with two view modes: split (all panes) and focus (single pane full-screen). Handles keyboard input for navigation, restart, and scrolling.
- **tui.Pane** - Wraps a `bubbles/viewport` for scrollable log display with a title bar showing service name, git branch, status, health indicator, and PID.

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

## Key Design Decisions

- Process groups (`Setpgid`) ensure child processes of services are also cleaned up on stop
- SIGINT with 5-second timeout before SIGKILL for graceful shutdown
- Generation counter on Service prevents stale goroutines from updating state after a restart
- Ring buffer avoids unbounded memory growth from long-running services
- Healthcheck poller uses the same generation guard to prevent stale goroutines after restart

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
