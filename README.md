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

### The agent writes to this file

Resolving a port conflict writes `portOverrides` or `substitutes` into the
project's entry, creating the entry if it does not exist yet. Those choices
belong somewhere you can read, review and commit — not in hidden agent state.

Editing is surgical: the file is round-tripped through the YAML node tree rather
than re-marshalled from a struct, so **your comments, key order, flow style and
2-space indentation all survive**, and keys the agent does not understand are
left untouched. Writes go to a temporary file and are renamed into place, so an
interrupted write cannot truncate your config.

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

## Every project is a list of rows

Each card lists one row per **unit** — a compose service, or the single
supervised app of a non-Docker project — regardless of state:

```
ecommerce        docker compose                        4/4 containers running
  ● admin    Up 2 minutes                    3010→80    [Open] [Kill]
  ● db       Up 2 minutes · healthy        5432→5432           [Kill]
  ● web      Up 2 minutes · healthy          3000→80    [Open] [Kill]
  ● worker   Up 2 minutes                    no ports          [Kill]
  [ Restart all ] [ Stop all ]  Logs

dashboard        docker compose                        0/2 containers running
  ○ ui       not created yet                 3001→80          [Start]
  ○ worker   not created yet                 no ports         [Start]
  [ Start all ]  Logs
```

`dashboard` has never been started, and still shows what it is made of and which
ports it is configured to use. Services come from the compose file, so they are
listed before any container exists; containers not in the file (left over from
an older revision) are listed too rather than hidden.

The port chip shows the port devcc will use and, for containers, the container
port behind it (`3000→80`). Three numbers are tracked, because after a move they
genuinely differ:

| | meaning |
| --- | --- |
| **declared** | what the compose file or config says — `8080` |
| **configured** | what devcc will actually use — `8081` after an override |
| **actual** | what is listening right now — nothing, until it starts |

The chip leads with **configured**, so a moved port reads `8081→8080  WAS 8080`
the moment you choose it, rather than waiting until something is running — and
turns amber, which is the whole signal. A substituted row is tagged
`SUBSTITUTED` and names its provider, since that is information the chip cannot
carry.

### One action per row

The button is whatever that row can actually do right now:

| Row state | Button |
| --- | --- |
| running | **Kill** — stops just this container/app, nothing else |
| stopped, port free | **Start** |
| stopped, port taken | **Resolve…** — opens the conflict dialog |
| provided by something else | **Unlink** |
| no start command / docker down | disabled, with the reason on hover |

Rows also carry their own inline note: `provided by shop/db on :5432`, or
`moved to :3100 — configured as :3000`. A row whose behaviour has been
customised gets a **change…** button that reopens the resolve dialog, so a
decision can be revisited without first breaking something.

**Clicking a port chip** opens a popover with that port's full story — configured
vs actual, the container port behind it, and who holds it:

```
Port 5432                         IN USE BY SOMETHING ELSE
  configured  5432
  actual      not listening
  container   5432

  Port 5432 is occupied by container shop-db-1 (compose project "shop").
  postgres:16
                                              [ Resolve… ]
```

That sentence used to sit in the card's summary line, where it crowded out the
scannable information and never said which row it referred to. Anchored to the
chip it has an obvious subject, and the card line is just `Port conflict`.

The card header shows the checked-out **git branch**, read straight from
`.git/HEAD` (worktrees and detached HEADs included) rather than by shelling out
to git once per project per refresh.

Offering all three at once would mean two of them are wrong. **Open** appears on
hover for running rows that publish a port; it goes through the agent so the URL
lands in your default browser, and degrades to a plain link on a machine with no
opener.

Card-level **Start all** / **Stop all** / **Restart all** stay for compose
projects, where doing four services one at a time is tedious. A supervised app
is a single row, so its card has no duplicate buttons.

Per-service compose commands are `up --detach --no-deps <service>` and
`stop --timeout 10 <service>`. `--no-deps` matters: without it, starting one
service would also start its dependencies, which is a decision you did not
make.

Every compose command is echoed into the project's log before it runs, in full:

```
$ docker compose --file /w/pmd/docker-compose.yml \
    --file /w/pmd/docker-compose.override.yml --ansi never \
    up --detach --no-deps backend   (in /w/pmd)
```

Which files Compose was handed is the answer to most compose failures, and it
is invisible otherwise.

### Compose override files

The agent passes `--file` explicitly — it has to, to layer a port override on.
That switches off Compose's *automatic* loading of `compose.override.yaml` /
`docker-compose.override.yml`, so the agent finds that sibling and passes it too.
Without it, a project that keeps its build context in the override file builds
something different from what `docker compose up` builds, and fails with
`failed to read dockerfile`. The order is always: base file, your override
sibling, then anything the agent generates.

Naming a file explicitly with `composeFile:` in the config is the equivalent of
`docker compose -f x`, which does not pull in a sibling either — so the agent
does not add one in that case.

## Port conflicts

Starting something whose port is taken does not fail and does not guess. The
agent refuses, returns the conflict, and **Resolve…** asks:

```
Resolve the port conflict
db in blog cannot use its configured port.

  :5432 is held by container shop-db-1 (compose project "shop")

  (•) Use something already running
      This project will not start its own copy.
      [ shop / db — :5432  (serves this port) ▾ ]

  ( ) Start on a different port
      You get a second, separate instance.
      :5432 → [ 5433 ]

  [ Free the port instead ]              [ Cancel ]  [ Use it ]
```

The two answers are genuinely different, and the wrong one is costly. Moving to
another port gives you a **second instance** — right for a web server, wrong for
the database three projects share, because an app talking to an empty second
database looks healthy while being useless. **Substituting** says "the one
already running is the one I want".

### Substitutes

A substituted unit is deliberately not started:

- Its dot is green with a blue ring: it is genuinely being served, just not by
  this project.
- Its row reads `provided elsewhere` with `provided by shop/db on :5432`.
- Its port stops counting as a conflict, so the project is no longer red.
- **Start all** skips it — `up --detach web` rather than `up --detach` — so a
  project-wide start cannot quietly undo the choice.
- If the provider stops, the row says so (`substitute shop/db unavailable — not
  running`) instead of silently pretending to be fine.
- **Unlink** hands the unit back to this project, and **change…** reopens the
  dialog — which then offers a third option, *Run it here, as configured*, that
  drops both the substitute and any port override in one go.

Candidates come from two places:

| Source | Target | Example |
| --- | --- | --- |
| a unit devcc manages | `shop/db` | another project's compose service |
| any running container | `docker/my-local-pg` | a hand-started `docker run -p 5432:5432 postgres`, or a stack outside your scan roots |

The second is why the picker calls `docker ps`: the postgres you started by hand
months ago is invisible to project discovery, and is exactly the thing a shared
dependency should point at. Containers that a known project already accounts for
are not offered twice, and anything publishing no port at all is left out — it
cannot stand in for a dependency another project needs to reach.

Candidates are ranked by whether they already serve the blocked port, so the
likely answer is preselected, with managed units above loose containers.

A `docker/` target is matched by **container name**, so recreating the container
under a generated name breaks the link — the row then reads `no running
container called "my-local-pg"` rather than pretending to be satisfied. The
`docker/` prefix is checked before project names, so a project called `docker`
cannot shadow it.

The suggested port is one that is genuinely free *and* not claimed by any other
project the agent knows about, so accepting it does not just defer the same
collision. A moved project keeps its choice across agent restarts, shows
`Moved to :4003 — declared as :4001`, and has a **Reset ports** button.

### Can it really move a Docker Compose port?

Sometimes, and the dashboard is explicit about which case you are in. There is
no CLI flag that remaps a published port, so:

| Compose file says | Mechanism | Works? |
| --- | --- | --- |
| `"${WEB_PORT:-3000}:80"` | set `WEB_PORT` in the environment | yes — this is what compose variables are for |
| `"3000:80"` | generated override file using `!override` | yes, on Compose **v2.24+** |
| `"3000:80"` on older Compose | — | no; the dialog says so and offers to free the port instead |

The second row is the subtle one. Layering an extra compose file with a
different port does **not** replace the original — compose concatenates port
lists, so you end up publishing both 3000 *and* 3100 and the conflict survives.
Only the `!override` tag replaces. The agent verifies your Compose version
before offering the option, writes the override into its own state directory
(never into your repository), and refuses rather than silently producing a
double-published service.

For supervised processes it sets `PORT` and rewrites an explicit `--port` flag
if the command has one. Neither is universal — Vite ignores `PORT`, and a
hand-written server may hard-code its address — so after starting, the agent
checks which port the process group actually opened and reports
`ignored the requested port 4003 and is listening on 4001 instead` when they
disagree.

## Nothing stops without being asked

The agent never shuts down a service you did not point at:

- A start blocked by a port returns the conflict. Nothing is signalled until you
  press **Free the port instead** in the dialog.
- **Kill** acts on exactly one row: one container, or one supervised app.
- `docker compose up`/`down` run **without** `--remove-orphans`. That flag stops
  and deletes containers belonging to the project but absent from the current
  compose file — a shutdown nobody requested.
- Starting an already-running supervised project reports
  `already running as pid N — use Restart to apply new settings` instead of
  quietly doing nothing, so a start that carries a new port cannot appear to
  have been applied while the old process keeps running.
- Supervised processes are left running when the agent exits, unless you pass
  `--stop-on-exit`.

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
| `POST` | `/api/projects/{id}/units/{unit}/start` \| `/kill` | act on one container or app. `{unit}` is the compose service name, or `app`. Same body and 409 semantics as below. |
| `GET` | `/api/projects/{id}/units/{unit}/candidates` | running units elsewhere that could provide this one |
| `GET` | `/api/projects/{id}/units/{unit}/options` | how this unit could run — the same payload a refused start returns, with `blocked: false` when nothing is actually in the way |
| `POST` | `/api/projects/{id}/units/{unit}/substitute` | `{"target": "shop/db"}`, or `""` to unlink. Persisted to the config file. |
| `POST` | `/api/projects/{id}/start` \| `/stop` \| `/restart` | runs to completion; returns the updated project. Optional body: `{"stopOwners": true}`, `{"ports": {"3000": 3100}}`, `{"clearOverrides": true}`. With an empty body nothing is stopped and no port moves — a blocked start returns **409** with a `conflict` object describing the owners and a suggested free port. |
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

**Open `http://localhost:5173`, not the agent's own URL.** The agent serves the
bundle embedded at the last `make build`, which has no Vite client and therefore
no hot reload — editing files against it looks like nothing happening. `make dev`
and the agent's startup banner both point at the Vite URL for this reason.

In dev, Vite proxies `/api` to the agent and injects the token in Node, so the
browser never holds it and the agent's auth is not weakened for convenience. The
token is read per request rather than once at startup, so it does not matter
whether Vite or the agent wins the race on a first run.

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
