# pairin

A terminal dashboard for running multiple local development services in parallel. Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

pairin reads a `.pairinrc.toml` config file from the current directory (or any parent directory), spawns a detached supervisor that runs all defined services, and attaches a split-pane TUI to it. The TUI can be detached and reattached without restarting the services.

<p align="center">
  <img src="pairin.jpeg" alt="pairin" width="400">
</p>


## Install

```bash
go install github.com/apsdsm/pairin@latest
```

Requires Go 1.25+.

## Usage

Create a `.pairinrc.toml` in your project root:

```toml
[project]
name = "My Project"

[[services]]
name = "api"
short = "api"
dir = "backend"
cmd = "go run main.go"
color = "blue"

[[services]]
name = "web"
short = "web"
dir = "frontend"
cmd = "npm run dev"
color = "green"
```

Then run:

```bash
pairin
```

## Subcommands

| Command                     | Action                                                                       |
|-----------------------------|------------------------------------------------------------------------------|
| `pairin` (or `pairin up`)   | Start the supervisor for this project (or attach if one is already running)  |
| `pairin -d` (or `pairin up -d`) | Start the supervisor in the background and exit without attaching a TUI |
| `pairin attach`             | Attach a TUI to a supervisor that's already running for this project         |
| `pairin down`               | Stop all services and the supervisor for this project                        |
| `pairin ls`                 | List every running pairin supervisor on this host                            |
| `pairin status`             | Show per-service status across every running supervisor                      |
| `pairin version`            | Print the version                                                            |

If a previous supervisor exited without cleaning up, `pairin up` detects the orphaned services and prompts you to **adopt** them, **restart** them fresh, or **quit**.

`-d` / `--detach` is idempotent: if a supervisor is already running for this project, it prints the existing PID and exits without doing anything.

## Configuration

### `[project]`

| Field  | Description          |
|--------|----------------------|
| `name` | Project display name |

### `[[services]]`

| Field           | Description                                                        |
|-----------------|--------------------------------------------------------------------|
| `name`          | Full service name (shown in pane title)                            |
| `short`         | Short label (shown in header indicators and footer)                |
| `dir`           | Working directory for the command (relative to config file or absolute) |
| `cmd`           | Shell command to run                                               |
| `color`         | Pane title color: `blue`, `green`, `yellow`, `red`, `cyan`, `magenta`, `white` |
| `healthcheck`   | Health endpoint: `tcp://host:port` or `http(s)://url`              |
| `depends_on`    | List of service names that must be healthy before this service starts |
| `restart`       | Restart policy: `"no"` (default), `"always"`, `"on-failure"`, `"on-success"` |
| `restart_delay` | Cooldown before restarting, Go duration string (default: `"3s"`)   |
| `max_restarts`  | Max consecutive auto-restarts before giving up; `0` = unlimited (default: `0`) |

## Service Dependencies & Healthchecks

Services can declare healthchecks and depend on other services:

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

- `healthcheck` — `tcp://host:port` (1s dial timeout) or `http(s)://url` (2s GET, expects 2xx)
- `depends_on` — listed services must be healthy before this service starts
- Services with unmet dependencies show as **waiting** (magenta) and auto-start when dependencies become healthy
- Healthcheck is orthogonal to status: a service can be running but not yet healthy

## Auto-Restart

Services can automatically restart when they exit, using systemd-style policies:

```toml
[[services]]
name = "web"
cmd = "bun run dev"
restart = "on-failure"
restart_delay = "5s"
max_restarts = 5
```

- `restart` — `"no"` (default), `"always"`, `"on-failure"`, `"on-success"`
- `restart_delay` — cooldown before retrying (default: `"3s"`)
- `max_restarts` — max consecutive auto-restarts; `0` = unlimited (default: `0`)
- The title bar shows restart count (e.g. `restarting 3/5` or `restarting #3`)
- Manual restart (`r` key) resets the restart counter

## Keyboard Shortcuts

| Key          | Action                                                       |
|--------------|--------------------------------------------------------------|
| `1`-`9`      | Focus a service pane full-screen                             |
| `z`          | Toggle zoom (split view ↔ focus on the active pane)          |
| `tab`        | Cycle active pane forward                                    |
| `shift+tab`  | Cycle active pane backward                                   |
| `r`          | Restart the active service                                   |
| `up` / `k`   | Scroll up                                                    |
| `down` / `j` | Scroll down                                                  |
| `q` / `ctrl+c` | Detach the TUI (services and supervisor keep running)      |
| `d`          | Shut down: stop every service and exit the supervisor        |

## How It Works

- Running `pairin` spawns a detached supervisor process (its own session leader) and attaches a TUI client to it over a unix socket at `.pairin/control.sock`. Closing the TUI with `q` leaves the supervisor and its services running; reattach later with `pairin` or `pairin attach`. Use `d` (or `pairin down`) to stop everything.
- Each service runs as a subprocess in its own process group, so the supervisor can clean up child processes on stop.
- Per-service stdout and stderr are merged into both an in-memory ring buffer (1000 lines, used by the TUI) and a rotating log file at `.pairin/logs/<service>.log` (rotated to `.log.1` once it exceeds 10 MiB).
- The supervisor announces itself in a host-wide registry (under `$XDG_STATE_HOME/pairin/instances/`, defaulting to `~/.local/state/pairin/instances/`), which is what `pairin ls` and `pairin status` read.
- On restart, the process receives SIGINT with a 5-second grace period before SIGKILL.
- Git branch is detected automatically for each service directory.
