# Development Command Center

A local dashboard for the projects on your machine: what is running, what is
stopped, what is broken, and which one is sitting on port 3000.

It is two pieces:

- **`devcc`** — a Go agent that runs on your machine and does all the host work:
  scanning directories, driving `docker compose`, supervising dev servers,
  inspecting ports, running health checks, opening URLs.
- **A React dashboard**, embedded in that binary and served from the same
  origin.

The browser never touches your filesystem, processes or Docker daemon. It only
talks to the agent's localhost API — which is also what a future CLI, menu-bar
app or editor extension would talk to.

```
Browser ──HTTP──▶ Go agent ──▶ docker compose CLI
                     │      ──▶ supervised process groups
                     │      ──▶ /proc, lsof, TCP probes
                     └──▶ HTTP health checks
```

## Quick start

```bash
make build          # builds the dashboard and embeds it in the agent binary
./agent/bin/devcc   # prints a URL containing the API token
```

Open the printed URL. That is it — there is no config file to write. The agent
scans `~/Projects`, `~/dev`, `~/src` and similar for projects, and everything it
finds shows up on the dashboard.

```bash
devcc --open        # start and open the browser automatically
devcc url           # print the dashboard URL again (with the token)
devcc config        # show which config file is in use, and the scan roots
devcc init          # write a commented config file you can edit
```

## Choosing which directories are scanned

Edit `scan.roots` in [`devcc.yaml`](devcc.yaml) at the root of this repository:

```yaml
scan:
  roots:
    - ~/Projects
    - ~/work
    - ~/src/github.com/acme
  depth: 3
```

Run `devcc` from this directory and that file is used — no flags needed. Confirm
with `devcc config`, which prints the file in use and the roots it resolved to.

Config is resolved in this order:

| # | Source | Use it for |
| --- | --- | --- |
| 1 | `$DEVCC_CONFIG` | scripts, CI, temporarily pointing somewhere else |
| 2 | `./devcc.yaml` or `./devcc.yml` | a project-local config, checked in next to your repos |
| 3 | `~/.config/devcc/config.yaml` (macOS: `~/Library/Application Support/devcc/config.yaml`) | your everyday personal setup |

`--config <path>` overrides all three. `devcc init` writes the per-user file;
`devcc init devcc.yaml` writes a local one.

Relative paths inside a config file resolve against **that file's directory**,
not your shell's working directory, so `./projects` means the same thing however
you launch the agent.

Because a `devcc.yaml` can specify `start:` commands the agent will run, running
`devcc` inside a directory you did not write means trusting its config — the same
way running `make` there means trusting its Makefile. The startup banner always
prints which file was loaded.

## What it detects

| Marker | Technology | Start mechanism |
| --- | --- | --- |
| `compose.yaml`, `docker-compose.yml` | Docker Compose | `docker compose up -d` in the project directory |
| `package.json` with a `dev`/`start` script | Node | `npm`/`pnpm`/`yarn`/`bun run <script>`, chosen by lockfile |
| `go.mod` with `main.go` or a single `cmd/*` | Go | `go run .` / `go run ./cmd/<name>` |
| `Cargo.toml` with `src/main.rs` | Rust | `cargo run` |
| anything else | — | listed and monitored, but not controllable until you configure a `start` command |

### How the scan works

Yes, it recurses — but with two rules that keep the result useful:

**It descends `scan.depth` levels below each root** (default 3). Depth 1 means
only the immediate children of a root; depth 3 reaches `~/Projects/a/b/c`.

**It stops as soon as a directory is recognised as a project.** Everything below
a detected project is skipped, so a compose project that also contains a
`package.json`, or a repo with vendored sub-modules, produces exactly one card
instead of a dozen. This is also why raising `depth` is cheap: it lets the scan
find projects nested further down without pulling in their contents.

```
~/Projects/                     ← root, depth 0
├── shop/                       ← depth 1  ✓ one project (compose)
│   ├── package.json                       ✗ not a second card
│   └── node_modules/…                     ✗ ignored outright
├── clients/                    ← depth 1  · no marker, keep walking
│   └── acme/                   ← depth 2  ✓ project
│       └── api/                ← depth 3  ✗ inside a project, skipped
└── monorepo/                   ← depth 1  ✓ project (workspace root)
    └── apps/web/               ← depth 3  ✓ project — workspaces are the exception
```

Monorepo roots (`pnpm-workspace.yaml`, or a `package.json` with `workspaces`)
are that exception: the agent lists the root *and* keeps descending, because the
real projects live one level down.

Directories in `scan.ignore` are never entered — the built-in list covers
`node_modules`, `.git`, `vendor`, `dist`, `build`, `target`, `.next`, `.venv`,
`coverage` and friends, and anything you add is appended to it rather than
replacing it. Dotfile directories are skipped too.

If a scan is picking up too much or too little, `devcc config` shows the roots
and depth actually in effect.

### About ports

Ports are first-class, and the agent only reports ones it actually knows:

- **Compose projects** — parsed from the compose file, covering the short form
  (`"3000:80"`, `"127.0.0.1:3010:80"`, ranges), the long form
  (`published:`/`target:`), and `${VAR:-default}` interpolation from `.env`.
  Once containers are running, the ports reported by `docker compose ps` take
  over as the authoritative list.
- **Everything else** — an explicit `--port`/`-p` flag in the dev script, or
  `PORT=` in `.env`, or whatever you put in the config file.

It deliberately does **not** assume framework defaults. A card that says
`:3000` when the app is really on `:5173` is worse than a card that says
nothing, and the agent learns the real port from the running process anyway.

For each port the dashboard shows whether it is free, **owned by this project**,
or **held by something else** — and in the last case, what: a PID and command
name, or a container and its compose project.

## Port conflicts

When a project is stopped and one of its ports is occupied, the card turns red
and names the culprit:

```
Port 4001 is occupied by python3 (pid 5312).          [ Resolve ]
```

**Resolve** stops that specific process or container, after a confirmation. The
agent never reassigns ports and never kills anything on its own — v1's job is to
explain the conflict, not to silently work around it. Starting a project into a
known conflict is refused up front, with the same message, rather than letting
it fail halfway.

## Security

The agent executes shell commands on your machine and listens on localhost.
Localhost is **not** a security boundary: every page you visit in your browser
can send requests to `http://127.0.0.1:47123`. Three independent checks apply to
every API request:

1. **Token.** Generated at first run, stored `0600` under the state directory.
   The dashboard receives it via the `?token=…` URL the agent prints, stashes
   it, and strips it from the address bar. A cross-origin attacker cannot read
   it, so it cannot forge an authorised request.
2. **Origin.** Any browser-supplied `Origin` must be one the agent knows.
   This blocks simple cross-origin `POST`s that never trigger a CORS preflight.
3. **Host.** The `Host` header must name a loopback address. This is what stops
   DNS rebinding, where an attacker points their own hostname at `127.0.0.1` so
   the browser treats them as same-origin with the agent.

Related decisions:

- The agent refuses to bind a non-loopback address.
- `POST /open` will only open URLs the project actually declares — it is not a
  general-purpose "open anything" primitive.
- Repo-local `.devcc.yaml` files are **off by default** (`allowRepoConfig`).
  Such a file specifies commands the agent would run, so honouring it would make
  cloning an untrusted repository enough to get code execution.
- `--no-token` exists for people who want it. It prints a warning, and origin
  and host checks still apply.

## Configuration reference

Everything is optional. See [`devcc.yaml`](devcc.yaml) for a commented copy, or
run `devcc init` to write one to your user config directory.

```yaml
listen: 127.0.0.1:47123

scan:
  roots: [~/Projects]
  depth: 3
  ignore: [.archive]      # added to the built-in ignore list, not replacing it

poll:
  interval: 5s            # how often every project's status is refreshed
  healthTimeout: 2s
  discoverEvery: 2m       # how often roots are re-scanned for new projects
  startupSettleFor: 90s   # health failures inside this window mean "starting"

projects:
  - name: Ecommerce
    path: ~/Projects/ecommerce
    health:
      url: http://localhost:3000/health

  - name: API
    path: ~/Projects/api
    start: npm run dev    # setting `start` puts a project under supervision
    ports: [4000]
    env:
      NODE_ENV: development

  - name: Legacy
    path: ~/Projects/legacy
    disabled: true        # hide it without deleting the directory
```

Configured entries are **patches** over what discovery found: set only a health
URL and the detected ports, name and start command all survive. A configured
`path` that discovery never reached is added to the list on its own.

## Supervised processes

Non-Docker projects are run as detached child process groups, which matters more
than it sounds:

- **Process groups.** `npm run dev` spawns children; the thing that actually
  binds your port is usually a grandchild. The agent puts each project in its
  own group and signals the group, so Stop really stops it instead of orphaning
  a server that keeps the port.
- **Logs go to a file, not a pipe.** If the agent exits while holding the read
  end of a pipe, the child gets `EPIPE` and often dies with it. Writing to a
  file means your dev servers outlive the agent.
- **Re-adoption.** On startup the agent reattaches to processes recorded by its
  previous run — status, logs and Stop all keep working across an agent restart.
  Identity is checked with the kernel's process start time, so a recycled PID is
  never mistaken for a project you own.

By default the agent leaves supervised processes running when it exits; pass
`--stop-on-exit` if you would rather it cleaned up.

## HTTP API

Everything under `/api` requires the token.

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/status` | agent capabilities: docker, browser opener, port lookup, scan roots |
| `GET` | `/api/projects` | every project with its current state |
| `POST` | `/api/projects/refresh` | re-scan the roots now |
| `GET` | `/api/projects/{id}` | one project |
| `GET` | `/api/projects/{id}/status` | force a status refresh, return the state |
| `POST` | `/api/projects/{id}/start` \| `/stop` \| `/restart` | runs to completion; returns the updated project |
| `GET` | `/api/projects/{id}/logs?tail=200` | snapshot; containers for compose projects, process output otherwise |
| `GET` | `/api/projects/{id}/logs/stream` | SSE, one `line` event per log line |
| `GET` | `/api/projects/{id}/health` | the configured probe and its last result |
| `POST` | `/api/projects/{id}/open` | `{"url": "…"}`, optional; defaults to the project's first URL |
| `POST` | `/api/projects/{id}/ports/{port}/resolve` | `{"force": false}` — stop whatever holds a conflicting port |
| `GET` | `/api/events` | SSE, `snapshot` then `project` / `projects` events |

Status updates are **pushed**, not polled. The agent refreshes every project on
one timer and fans the result out over SSE, so ten open tabs cost the same as
one — `docker compose ps` and port scans do not multiply by the number of
viewers.

Project IDs are derived from the absolute path, so they are stable across
restarts and rescans.

## Development

```bash
make dev      # agent + Vite dev server with hot reload on :5173
make test     # go vet, go test, tsc --noEmit
make smoke    # drive the compose path against a stub docker CLI
make build    # production single binary
make install  # copy it to ~/.local/bin
```

`make smoke` runs `scripts/compose-smoke-test.sh`, which puts a stub `docker` on
`PATH` and drives a real agent through discovery → `up` → `ps` parsing → port
attribution → logs → `down`. It covers the compose integration on machines
without a Docker daemon, which is most CI.

`fixtures/projects/` holds sample projects — a compose stack, Node dev servers,
a Go service, and one deliberately uncontrollable package — to point the agent
at while developing:

```bash
devcc --config fixtures/devcc.yaml
```

In dev, Vite proxies `/api` to the agent and injects the token in Node, so the
browser never holds it and the agent's auth is not weakened for convenience.

If you build the UI from more than one platform against the same checkout (a
macOS host and a Linux container, say), `web/node_modules` ends up holding one
platform's native binaries and the other build fails with `Cannot start
service: Host version … does not match binary version …` or a missing
`@rollup/rollup-<platform>` module. Run `npm install` in `web/` to repair it for
the current platform. In this sandbox, `.sandbox-node-modules` lists `web` so it
gets its own isolated copy and neither side clobbers the other.

```
agent/
  cmd/devcc/          entry point, flags, subcommands
  internal/
    model/            domain types shared everywhere
    config/           config file loading and merging
    discovery/        directory scanning and project detection
    composefile/      compose file parsing (ports, project name)
    dockercli/        docker/compose CLI adapter
    procmgr/          process-group supervision, adoption, log tailing
    ports/            listening ports and their owners (linux + darwin)
    health/           HTTP probes
    logbuf/           bounded log ring buffer with live fan-out
    core/             registry, status reconciler, lifecycle actions
    api/              HTTP routes, auth, SSE
    webui/            embedded dashboard
web/                  React + TypeScript dashboard
fixtures/projects/    sample projects to develop against
```

## Platform support

macOS and Linux. Port ownership uses `lsof` on macOS and `/proc/net/tcp` on
Linux; browser opening uses `open` and `xdg-open`. Windows is not supported —
the process-supervision and port-inspection layers have platform stubs, but no
implementation.

## Known limitations

- Compose is driven through the CLI, so it inherits the CLI's speed. Status
  refresh for a compose project costs one `docker compose ps` (~100–300ms);
  the reconciler bounds concurrency and skips overlapping refreshes.
- Port ownership only sees processes owned by your user. Another user's process
  holding your port is reported as occupied with an unknown owner.
- Compose logs are fetched on demand rather than followed continuously, so the
  agent does not keep a `docker compose logs -f` process alive for every project
  all day. Opening the log panel attaches a follower for as long as it is open.
- Automatic port reassignment, remote projects, Kubernetes and container metrics
  are out of scope for v1.
