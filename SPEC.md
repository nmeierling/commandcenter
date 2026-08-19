# Local Development Command Center

## Goal

Build a local developer command center that discovers and manages multiple projects on a developer's machine.

The application provides a browser-based dashboard for seeing which projects are running, starting/stopping them, opening their local URLs, viewing logs, and detecting port conflicts.

The system consists of:

1. **Web UI** — React + TypeScript
2. **Local Agent** — Go, running directly on the host machine
3. **Projects** — Existing local projects, commonly using Docker Compose

The Go agent is responsible for all host-level operations. The web application should not need direct access to the host filesystem, processes, or Docker daemon.

## Core Features

### Project Discovery

The agent scans configured project directories and detects projects based on common files:

* `compose.yaml` / `docker-compose.yml`
* `package.json`
* `pnpm-workspace.yaml`
* `go.mod`
* `Cargo.toml`
* Other configurable project markers

Each discovered project should expose:

* Name
* Local path
* Detected technology
* Start/stop mechanism
* Expected ports
* Current status

### Project Lifecycle

For every project, the dashboard should support:

* Start
* Stop
* Restart
* Open in browser
* View logs
* Refresh status

Docker Compose projects should initially be managed through the Docker CLI:

```text
docker compose up -d
docker compose down
docker compose restart
docker compose ps
docker compose logs
```

The agent should execute these commands from the project's directory.

### Status & Health

The agent determines whether a project is running using multiple signals where appropriate:

* Process status
* Docker Compose container status
* Port availability
* HTTP health check

A project can therefore display states such as:

```text
Running
Stopped
Starting
Unhealthy
Error
Unknown
```

### Port Management

Ports should be first-class project metadata.

The agent should detect:

* Which ports a project expects
* Which ports are currently occupied
* Which process/container owns an occupied port

If a configured port is unavailable, the UI should clearly report the conflict.

Automatic port reassignment can be added later; **v1 should primarily detect and explain conflicts rather than silently changing ports.**

### Browser Access

Each project can have one or more URLs, for example:

```text
http://localhost:3000
http://localhost:3001/admin
```

The dashboard provides an **Open** action that launches the URL in the user's default browser.

## Go Local Agent

The Go agent runs directly on the developer's machine.

Responsibilities:

* Filesystem access
* Project discovery
* Process management
* Docker CLI interaction
* Port inspection
* Health checks
* Log retrieval
* Opening URLs
* Exposing a local HTTP API

Example API:

```text
GET  /api/projects
GET  /api/projects/:id
GET  /api/projects/:id/status

POST /api/projects/:id/start
POST /api/projects/:id/stop
POST /api/projects/:id/restart

GET  /api/projects/:id/logs
GET  /api/projects/:id/health
POST /api/projects/:id/open
```

The agent should listen only on localhost by default.

Example:

```text
http://127.0.0.1:47123
```

No external network access should be required for normal operation.

## Project Configuration

Auto-discovery should be supplemented by an optional configuration file.

Example:

```yaml
projects:
  - name: Ecommerce
    path: ~/Projects/ecommerce
    health:
      url: http://localhost:3000/health

  - name: Dashboard
    path: ~/Projects/dashboard
    health:
      url: http://localhost:3001
```

The configuration should allow overriding automatically detected values.

## Web UI

The initial dashboard should be intentionally simple.

Example:

```text
┌─────────────────────────────────────────────────┐
│ Development Command Center                      │
├─────────────────────────────────────────────────┤
│                                                 │
│ 🟢 Ecommerce                         :3000       │
│ Docker Compose · 4 containers                   │
│ [ Open ] [ Restart ] [ Logs ]                   │
│                                                 │
│ 🟢 Dashboard                         :3001       │
│ Docker Compose · 3 containers                   │
│ [ Open ] [ Restart ] [ Logs ]                   │
│                                                 │
│ ⚪ CRM                               :3002       │
│ Stopped                                         │
│ [ Start ]                                        │
│                                                 │
│ 🔴 Website                           :3003       │
│ Port 3003 is occupied by another process       │
│ [ Resolve ] [ Logs ]                            │
│                                                 │
└─────────────────────────────────────────────────┘
```

## Architecture

```text
                    Browser
                       │
                       │ HTTP
                       ▼
              ┌─────────────────┐
              │ React / Vite UI │
              └────────┬────────┘
                       │
                 localhost API
                       │
                       ▼
              ┌─────────────────┐
              │    Go Agent     │
              │                 │
              │  Filesystem     │
              │  Processes      │
              │  Docker         │
              │  Ports          │
              │  Health checks  │
              └────────┬────────┘
                       │
              ┌────────┴────────┐
              ▼                 ▼
           Docker           Local processes
```

## Non-Goals for v1

Do not initially build:

* App Store distribution
* iOS application
* Automatic port reassignment
* Remote project management
* Cloud synchronization
* User accounts
* Kubernetes support
* Complex Docker API integration
* IDE integrations

The goal of v1 is to make **local project management dramatically easier**.

## Future Extensions

Once the core agent is stable, the same local API can support:

* CLI: `cc start my-project`
* macOS menu-bar application
* VS Code extension
* Automatic project discovery
* Automatic port allocation
* Git branch/worktree awareness
* Project-specific environment variables
* Docker container metrics
* Startup dependencies
* "Start my development environment" profiles

The central architectural decision is to keep the **Go agent independent from the UI**. This allows the command center to evolve from a simple browser dashboard into a complete local development environment manager without rewriting the underlying orchestration layer.
