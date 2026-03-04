# CloudCode

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-Required-2496ED?logo=docker)](https://www.docker.com)

A self-hosted management platform for [OpenCode](https://opencode.ai) instances. Spin up multiple isolated OpenCode environments as Docker containers and manage them through a single web dashboard.

## Features

- **Multi-instance management** — Create, start, stop, restart, and delete OpenCode instances
- **Configurable resource limits** — Set memory and CPU limits per instance at creation time, or leave unlimited
- **Session isolation** — Each instance has its own workspace; auth tokens are shared globally
- **Shared global config** — Manage `opencode.jsonc`, `AGENTS.md`, auth tokens, custom commands, agents, skills, and plugins from a unified Settings UI
- **skills.sh integration** — Install [skills.sh](https://skills.sh) skills inside any container, shared across all instances
- **Reverse proxy** — Access each instance's Web UI through a single entry point (`/instance/{id}/`)
- **Auto-updating containers** — OpenCode + Oh My OpenCode updated on each container start
- **Cloudflare Tunnel built-in** — Each container ships with `cloudflared` pre-installed; expose any local service to the public internet with a single command, no port forwarding needed
- **Playwright Chromium** — Pre-installed in each container with symlinks at `/usr/bin/chromium-browser` and `/usr/bin/chrome`

## Quick Start

### Docker Compose (Recommended)

```bash
mkdir cloudcode && cd cloudcode
# Create the shared Docker network (required, one-time setup)
docker network create cloudcode-net
curl -O https://raw.githubusercontent.com/naiba/cloudcode/main/docker-compose.yml
docker compose up -d
```

Open http://localhost:8080 in your browser.

Images are pulled from `ghcr.io/naiba/cloudcode` and `ghcr.io/naiba/cloudcode-base` automatically.

## Architecture

```
Browser → CloudCode Platform (Go JSON API + Next.js frontend)
               ├── Dashboard        — List / manage instances
               ├── Settings         — Global config editor
               └── /instance/{id}/  — Reverse proxy → container:port
                                          │
                              ┌────────────┼────────────┐
                              ▼            ▼            ▼
                         Container 1  Container 2  Container N
                         (opencode    (opencode    (opencode
                          web :10000)  web :10001)  web :10002)
```

```
main.go                          Entry point, starts HTTP server
internal/
  config/config.go               Config file management (read/write host config, generate bind mounts)
  config/plugins/                Embedded built-in plugins (written to plugins/ on startup)
  docker/manager.go              Docker container lifecycle (create/start/stop/delete)
  handler/handler.go             All HTTP handlers (JSON REST API)
  handler/memory_*.go            Platform-specific host memory detection
  proxy/proxy.go                 Dynamic reverse proxy to each instance's OpenCode web UI
  store/store.go                 SQLite persistence (instance CRUD)
docker/
  Dockerfile                     Base image (Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode)
  entrypoint.sh                  Container startup script (updates deps + starts opencode web)
Dockerfile.platform              Multi-stage build for the platform itself
frontend/                        Next.js 16 App Router frontend (TypeScript + Tailwind)
  app/                           Pages: dashboard, instance detail, terminal, settings, new instance
  components/AnsiLog.tsx         ANSI log renderer (safe DOM API, no dangerouslySetInnerHTML)
  lib/api.ts                     Typed API client + WebSocket helpers
```

Each container runs `opencode web` and is accessible through the platform's reverse proxy.

## Configuration

Global config is managed through the Settings page and bind-mounted into all containers:

| Storage | Container Path | Scope | Contents |
|---|---|---|---|
| `data/config/opencode/` | `/root/.config/opencode/` | Global | `opencode.jsonc`, `AGENTS.md`, `package.json`, commands/, agents/, skills/, plugins/ |
| `data/config/opencode-data/auth.json` | `/root/.local/share/opencode/auth.json` | Global | Auth tokens (shared across all instances) |
| `data/config/dot-opencode/` | `/root/.opencode/` | Global | `package.json` |
| `data/config/agents-skills/` | `/root/.agents/` | Global | Skills installed via [skills.sh](https://skills.sh) |
| `cloudcode-home-{id}` (volume) | `/root` | Per-instance | Workspace, cloned repos, session data |

Environment variables (e.g. `ANTHROPIC_API_KEY`, `GH_TOKEN`) are configured in Settings and injected into all containers.

### Cloudflare Tunnel (Exposing Local Services)

Each container has `cloudflared` pre-installed. To expose a local service running inside a container to the public internet:

```bash
# Inside the container terminal, expose a local service (e.g. port 3000)
cloudflared tunnel --url http://localhost:3000
```

This creates a free temporary tunnel via [TryCloudflare](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/) and outputs a public `*.trycloudflare.com` URL. No account or configuration required.

For persistent tunnels with custom domains, see the [Cloudflare Tunnel documentation](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/).

## Tech Stack

- **Backend**: Go 1.25, `net/http` stdlib router, SQLite (via `modernc.org/sqlite`, pure Go no CGO)
- **Frontend**: Next.js 16 App Router, TypeScript, Tailwind CSS v4, xterm.js for terminal
- **Containers**: Docker SDK (`github.com/moby/moby/client`)
- **Base Image**: Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode + Oh My OpenCode

## Development

```bash
# Build the Go binary
go build -o bin/cloudcode .

# Run backend in dev mode (no Docker required)
go run . --no-docker --addr :9090

# Run frontend dev server (in frontend/)
bun install
bun run dev

# Backend started with CORS for frontend dev:
./bin/cloudcode --addr :9090 --cors-origin http://localhost:3000

# Static analysis
go vet ./...

# Build checks
go build ./...
bun run build   # in frontend/

# Build base image
docker build -t cloudcode-base:latest -f docker/Dockerfile docker/

# Build platform image
docker build -t cloudcode:latest -f Dockerfile.platform .
```

## Port Allocation

The platform assigns one port per instance from the range `10000–10100` (101 ports max). Ports are tracked in SQLite and released when an instance is deleted.

## Security Notes

- Path traversal protection on all config file operations (`containedPath` validation)
- WebSocket connections validated against `Host` header (CSRF protection)
- Request bodies limited to 1–10 MB via `http.MaxBytesReader`
- `_cc_inst` cookie uses `HttpOnly` + `Secure` (when served over TLS) + `SameSite=Lax`
- Instance IDs in injected proxy scripts are JSON-encoded to prevent XSS
