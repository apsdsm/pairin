# pairin

A terminal dashboard for running multiple local development services in parallel. Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

pairin reads a `.pairinrc.toml` config file from the current directory (or any parent directory), starts all defined services, and displays their logs in a split-pane TUI.

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

| Key          | Action                          |
|--------------|---------------------------------|
| `1`-`9`      | Focus a service pane full-screen |
| `a`          | Return to split view            |
| `tab`        | Cycle active pane forward       |
| `shift+tab`  | Cycle active pane backward      |
| `r`          | Restart the active service      |
| `up` / `k`   | Scroll up (focused view)        |
| `down` / `j` | Scroll down (focused view)      |
| `q`          | Quit (stops all services)       |

## How It Works

- Each service runs as a subprocess in its own process group
- stdout and stderr are merged and captured into a ring buffer (1000 lines)
- On restart, the process receives SIGINT with a 5-second grace period before SIGKILL
- Git branch is detected automatically for each service directory
