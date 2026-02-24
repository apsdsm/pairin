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

| Field   | Description                                                        |
|---------|--------------------------------------------------------------------|
| `name`  | Full service name (shown in pane title)                            |
| `short` | Short label (shown in header indicators and footer)                |
| `dir`   | Working directory for the command (relative to config file or absolute) |
| `cmd`   | Shell command to run                                               |
| `color` | Pane title color: `blue`, `green`, `yellow`, `red`, `cyan`, `magenta`, `white` |

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
