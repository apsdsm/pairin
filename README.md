# pairin

A terminal dashboard for running multiple local development services in parallel.

<img width="600" height="362" alt="pairin-screenshot-01" src="https://github.com/user-attachments/assets/6b82c149-1f96-4ad5-b9bb-8afd4ab51c52" />

pairin reads a `.pairinrc.toml` config file from the current directory (or any parent directory), spawns a detached supervisor that runs all defined services, and attaches a split-pane TUI to it. The TUI can be detached and reattached without restarting the services.

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
| `pairin dash`               | Dashboard of every project on this host                                      |
| `pairin` (or `pairin up`)   | Start the supervisor for this project (or attach if one is already running)  |
| `pairin -d` (or `pairin up -d`) | Start the supervisor in the background and exit without attaching a TUI |
| `pairin up <project>`       | Start a **registered** project by name, from anywhere                        |
| `pairin attach [project]`   | Attach a TUI to a supervisor that's already running                          |
| `pairin down [project]`     | Stop all services and the supervisor                                         |
| `pairin register [path]`    | Add a project to the catalog so it can be started by name                    |
| `pairin unregister <name>`  | Remove a project from the catalog                                            |
| `pairin projects`           | List registered projects and whether each is running                         |
| `pairin ls`                 | List every running pairin supervisor on this host                            |
| `pairin status`             | Show per-service status across every running supervisor                      |
| `pairin version`            | Print the version                                                            |

If a previous supervisor exited without cleaning up, `pairin up` detects the orphaned services and prompts you to **adopt** them, **restart** them fresh, or **quit**.

`-d` / `--detach` is idempotent: if a supervisor is already running for this project, it prints the existing PID and exits without doing anything.

`--clear-logs` (on `pairin` / `pairin up`) deletes the existing service logs in `.pairin/logs/` before starting, so the TUI opens with fresh panes instead of preloading history from previous sessions. It refuses to run while a supervisor is already up — stop it with `pairin down` first.

To clear logs *without* stopping anything, press `c` in either TUI (or `C` in the dashboard for a whole project). That empties the log in place rather than deleting it, which is what makes it safe while the service is running: services write with `O_APPEND`, so after truncation they simply resume from the start of the file. Deleting it instead would leave the service writing to a file nobody can read.

## Dashboard Mode

`pairin dash` shows every project on the host at once.

The key sits on the first line where it stays put, and the bottom two lines are the last action's
result and the keys — the result gets its own line rather than replacing them.

Registered projects that aren't running appear greyed out with their service names read from their
config, so you can see a project's shape before starting it — `s` starts it in place.

### Pinned and unpinned projects

Running projects always appear. Stopped ones appear only if they are **pinned**. A project heading
leads with `◆` when pinned and `◇` when not, and both are in the key at the top of the screen:

- `pairin register` pins a project. Registering is a deliberate act, so it stays listed.
- `pairin up` adds an unpinned entry. Starting a project once to check something shouldn't leave a
  permanent entry behind, so it drops off the dashboard as soon as it stops.
- `p` in the dashboard toggles pinning, including for projects that were never registered at all.

A project with no services to list — usually one whose config file has been moved or deleted — still
gets a selectable `(no services)` placeholder, so it can be pinned, started, or unpinned like any
other. `pairin projects` shows the pin state of everything in the catalog.

### Adding a project from the dashboard

`a` opens a project picker in the bottom half of the screen. 

It lists directories and pairin configs, nothing else. The count on the right says how many configs
each directory holds, so it's clear which are worth opening. Inside a project, configs are labelled
with their `[project].name` rather than just a filename — which matters when a project keeps
`.pairinrc.toml` and `.pairinrc.localdev.toml` side by side.

Configs already in the dashboard are marked `already added` and refuse to be added twice. A config
that's in the catalog but **unpinned** is marked `unpinned — enter to pin` instead: it isn't on
screen, so adding it pins it and makes it appear.

`enter` on a directory descends, `←` goes up, `enter` on a config adds it (pinned) and closes the
picker. The directory you were last in is remembered for next time.

### Other Commands

| Key            | Action                                                     |
|----------------|------------------------------------------------------------|
| `←↑↓→` / `hjkl`| Move the selection (scroll, when zoomed)                   |
| `z` / `enter`  | Open the selected service's logs, and back                 |
| `r`            | Restart the selected service                               |
| `x`            | Stop the selected service                                  |
| `s`            | Start the selected service, or the whole project if it's down |
| `S`            | Shut down the selected project                             |
| `a`            | Add a project — opens a picker in the bottom of the screen |
| `p`            | Pin or unpin the selected project (pinned = always listed) |
| `c` / `C`      | Clear the selected service's logs / the whole project's    |
| `b`            | Cycle cell style: plain → boxed → cards                    |
| `/`            | Filter services by name, across every project              |
| `q` / `ctrl+c` | Close the dashboard (everything keeps running)             |

`b` cycles how much room each service gets.

### Ports

In card style, each cell lists the TCP ports its service is listening on, one per line:

```
┏━━━━━━━━━━━━━┓ ╭─────────────╮ ╭─────────────╮
┃›● single    ┃ │ ● double    │ │ ● quiet     │
┃   :45001    ┃ │   :45010    │ │   pid 365066│
┃             ┃ │   :45011    │ │             │
┗━━━━━━━━━━━━━┛ ╰─────────────╯ ╰─────────────╯
```

These are **discovered, not declared** — pairin asks the kernel which ports the service's process
group has open. Nothing to configure, and it finds the port even when it lives inside a vite config
or a `.env` file that pairin never sees.

A service listening on several ports gets several lines, and every card in that row grows to match so
the grid stays rectangular. Past four ports the last line reads `+N more`. The zoomed log view shows
them in its title bar too.

The line is blank when there are no ports. It means one thing — where to reach this service — so
nothing else goes there; the glyph already carries the status, and the PID is in the zoomed view.

#### `exposes`

Discovery works by process group, so it misses anything bound outside it. A service running
`docker compose up` has its ports bound by the docker daemon, which is in nobody's process group but
its own, and shows nothing. Declare those:

```toml
[[services]]
name = "stack"
cmd = "docker compose up"
exposes = ["db:5432", "redis:6379", "ses:4500", 9000]
```

Renders as:

```
╭─────────────────╮
│ ● stack         │
│   ses :4500     │
│   db :5432      │
│   redis :6379   │
│   :9000         │
╰─────────────────╯
```

A label says what answers on the port, which is the part a bare number can't tell you when one
service fronts several things. It's optional — `9000` above has none. Labels are truncated to eight
characters, because column width is sized to fit the widest line and an unbounded label would widen
every cell in the grid.

Four shapes are accepted, so you can write whichever reads best:

```toml
exposes = [5432, 9000]                       # bare ports
exposes = ["db:5432", "redis:6379"]          # labelled
exposes = [["db", 5432], ["redis", 6379]]
exposes = [{label = "db", port = 5432}]
```

The string form doesn't care about order or separator — `"db:5432"`, `":5432 db"`, `"db=5432"`,
`"5432 db"` and `"db/5432"` all mean the same thing. Whatever's a number is the port; the rest is the
label.

A port pairin can't read is **dropped with a warning, not an error**. It's a label on a dashboard, so
a typo in one has no business stopping your project from starting:

```
$ pairin
pairin: stack: ignoring exposes entry: "redis" has no port number in it
```

The same warning is written to that service's log, so it's still there if you started detached or
came back to it later.

Declared ports are shown **alongside** anything discovered, deduplicated and sorted — they add to
what was found rather than replacing it, since hiding a port a service is genuinely listening on
would be a lie. Labels apply to discovered ports too, so declaring `"api:40200"` names that port
whether or not discovery also finds it — you can label the ones you care about without listing them
all. Like discovered ports they appear only while the service is running, so a port on a card always
means you can reach it there.

`z` on any service opens its logs full-screen. Only that one service streams its output while you're
looking at it; the rest of the host's logs stay off the wire.

`q` closes the dashboard and touches nothing. Stopping is always explicit: `x` for a service, `S` for
a whole project.

The dashboard re-reads the catalog and the registry every couple of seconds, so projects you start in
another terminal appear on their own, and supervisors that go away are dropped.


## Standalone Mode

pairin while running in stand-alone mode has three views:

- **split** — every service gets a log pane, stacked. The default for a handful of services.
- **grid** — a compact status grid, one cell per service. Built for configs too big to give
  everything a readable pane.
- **focus** — one service's logs, full screen.

`v` switches between split and grid; `z` (or `enter`) zooms the selected service to full screen and
back. If there are so many services that each pane would be under 6 lines tall, pairin **starts in
grid view on its own** — twenty two-line viewports show nothing useful. Pressing `v` takes the choice
back, and pairin stops second-guessing it on resize.

In grid view each cell carries a status glyph: `●` up, `◍` running but failing its healthcheck,
`◐` starting, `⋯` waiting on a dependency, `⟳` restarting, `✕` crashed, `○` stopped.

### Keyboard Shortcuts

| Key            | Action                                                     |
|----------------|------------------------------------------------------------|
| `1`-`9`        | Focus a service pane full-screen                           |
| `v`            | Switch between split and grid view                         |
| `z` / `enter`  | Zoom the selected service full-screen, and back            |
| `esc`          | Leave focus view, or clear the grid filter                 |
| `tab`          | Cycle selection forward                                    |
| `shift+tab`    | Cycle selection backward                                   |
| `←↑↓→` / `hjkl`| Move the selection (grid) or scroll logs (split / focus)   |
| `/`            | Filter services by name (grid view)                        |
| `b`            | Cycle cell style: plain → boxed → cards (grid view)        |
| `r`            | Restart the selected service                               |
| `c`            | Clear the selected service's logs                          |
| `q` / `ctrl+c` | Detach the TUI (services and supervisor keep running)      |
| `d`            | Shut down: stop every service and exit the supervisor      |


## Register Projects

Rather than cd-ing around to find configs, register your projects once and start them by name from
anywhere:

```bash
pairin register                  # register the project in the current directory
pairin register ~/Code/acme-api  # or one somewhere else
pairin projects                  # see what's registered, and what's running

pairin up acme-api               # start it, from any directory
pairin up acme                   # unique prefixes work too
pairin down acme-api
```

Running `pairin up` in a project registers it automatically, so the catalog fills itself in as you
work. Pass `--no-register` to opt out of that for a particular project. Entries added this way are
**unpinned** — they show in `pairin dash` while running and drop off when stopped, so a project
started once to check something doesn't clutter the dashboard forever. `pairin register` pins.

Names are slugs derived from the `[project].name` in the config — "Acme API (localdev)" becomes
`acme-api-localdev` — because display names with spaces and parentheses make poor things to type.
Override with `pairin register --name <slug>`; a name you chose is never overwritten. Use `--group`
to label entries for the listing.

A prefix that matches more than one project is refused rather than guessed:

```
$ pairin up acme
Error: "acme" matches 2 projects (acme-api-localdev, acme-worker) — be more specific
```

The catalog lives at `$XDG_CONFIG_HOME/pairin/projects.toml` (default `~/.config/pairin/projects.toml`).
It's config, not state: it survives a state cleanup, it's safe to hand-edit, and it's reasonable to
keep in a dotfiles repo. `pairin projects` flags entries whose config file has moved or been deleted
rather than quietly reporting them as stopped.

## Alternative Configs

`pairin`, `pairin up`, `pairin attach`, and `pairin down` accept `-c <path>` to point at a specific `.pairinrc.toml` instead of searching from the current directory. Relative paths are resolved against the current working directory. Each config's supervisor, socket, state, and logs live under `<config-dir>/.pairin/`, so configs in different directories don't interfere.

```bash
pairin -c ../other-project/.pairinrc.toml
pairin attach -c /path/to/.pairinrc.toml
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
| `exposes`       | Ports pairin can't discover for itself, e.g. `["db:5432", 9000]` — see Ports |
| `depends_on`    | List of service names that must be healthy before this service starts |
| `restart`       | Restart policy: `"no"` (default), `"always"`, `"on-failure"`, `"on-success"` |
| `restart_delay` | Cooldown before restarting, Go duration string (default: `"3s"`)   |
| `max_restarts`  | Max consecutive auto-restarts before giving up; `0` = unlimited (default: `0`) |

### Service Dependencies & Healthchecks

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

### Auto-Restart

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

## Crash Logs

If pairin itself crashes, it writes a report to `$XDG_STATE_HOME/pairin/crash-<timestamp>-<pid>.log`
(default `~/.local/state/pairin/`) naming the goroutine and the stack. A TUI crash leaves your
services running — reattach with `pairin attach`.

If the supervisor goes away while a TUI is attached, the TUI stays up, shows a reconnect banner, and
reattaches by itself once the supervisor is back.

## How It Works

- Running `pairin` spawns a detached supervisor process (its own session leader) and attaches a TUI client to it over a unix socket at `.pairin/control.sock`. Closing the TUI with `q` leaves the supervisor and its services running.
 reattach later with `pairin` or `pairin attach`. Use `d` (or `pairin down`) to stop everything.
- Each service runs as a subprocess in its own process group, so the supervisor can clean up child processes on stop.
- Per-service stdout and stderr are merged into both an in-memory ring buffer (1000 lines, used by the TUI) and a rotating log file at `.pairin/logs/<service>.log` (rotated to `.log.1` once it exceeds 10 MiB).
- The supervisor announces itself in a host-wide registry (under `$XDG_STATE_HOME/pairin/instances/`, defaulting to `~/.local/state/pairin/instances/`), which is what `pairin ls` and `pairin status` read.
- On restart, the process receives SIGINT with a 5-second grace period before SIGKILL.
- Git branch is detected automatically for each service directory.

## Uses

- [Bubble Tea](https://github.com/charmbracelet/bubbletea).

<p align="center">
  <img src="pairin.jpeg" alt="pairin" width="400">
</p>

