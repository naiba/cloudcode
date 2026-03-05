# CloudCode

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-Required-2496ED?logo=docker)](https://www.docker.com)

A self-hosted management platform for [OpenCode](https://opencode.ai) instances. Spin up multiple isolated OpenCode environments as Docker containers and manage them through a single web dashboard.

## Features

- **Token-based access control** — Single admin token gates all API, WebSocket, and proxy routes; session cookie issued on login
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

Open http://localhost:8080 in your browser and log in with the token configured via `--access-token`.

Images are pulled from `ghcr.io/naiba/cloudcode` and `ghcr.io/naiba/cloudcode-base` automatically.

## Authentication

All API, WebSocket, and proxy routes require authentication. The platform uses a single admin token set at startup.

### Login

`POST /api/auth/login` with `{"token": "<your-token>"}`. On success a session cookie (`_cc_session`) is set — HttpOnly, SameSite=Lax, 30-day MaxAge.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--access-token` | *(required)* | Admin token. Server refuses to start without it. |
| `--cors-origin` | `""` | Allowed CORS origins for the platform API (dev only). Comma-separated. |

### Rate limiting

Login attempts are limited to **10 per IP per 60 seconds**. Exceeded attempts return `429 Too Many Requests`.

### WebSocket authentication

Browsers cannot send cookies on cross-origin WebSocket upgrades. For dev setups where the frontend runs on a different origin, fetch a one-time token first:

```js
// 1. Fetch a one-time token (session cookie sent automatically)
const { token } = await fetch('/api/auth/ws-token').then(r => r.json())
// 2. Append it to the WebSocket URL
const ws = new WebSocket(`ws://localhost:8080/instances/${id}/logs/ws?token=${token}`)
```

The `buildWsUrl(path)` helper in `frontend/lib/api.ts` handles this automatically.

## Architecture

```
Browser → CloudCode Platform (Go JSON API + Next.js frontend)
               ├── /login           — Login page (public)
               ├── Dashboard        — List / manage instances
               ├── Settings         — Global config editor
               └── /instance/{id}/  — Reverse proxy (token-validated)
                                          │  cloudcode-net (Docker bridge)
                              ┌────────────┼────────────┐
                              ▼            ▼            ▼
                         Container 1  Container 2  Container N
                         (opencode    (opencode    (opencode
                          web :4096)   web :4096)   web :4096)
                         [Basic Auth] [Basic Auth] [Basic Auth]
```

Container ports are **not published to the host**. All traffic routes through the Go proxy via the internal `cloudcode-net` Docker bridge network. Each container runs OpenCode with `OPENCODE_SERVER_PASSWORD` set to its unique access token, providing defense-in-depth.

```
main.go                          Entry point, starts HTTP server, embeds frontend/dist
internal/
  config/config.go               Config file management (read/write host config, generate bind mounts)
  config/plugins/                Embedded built-in plugins (written to plugins/ on startup)
  docker/manager.go              Docker container lifecycle (create/start/stop/delete)
  handler/handler.go             All HTTP handlers (JSON REST API, auth, session management)
  handler/memory_*.go            Platform-specific host memory detection
  proxy/proxy.go                 Dynamic reverse proxy to each instance's OpenCode web UI
  store/store.go                 SQLite persistence (instance CRUD)
docker/
  Dockerfile                     Base image (Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode)
  entrypoint.sh                  Container startup script (updates deps + starts opencode web)
Dockerfile.platform              Multi-stage build for the platform itself
frontend/                        Next.js 16 App Router frontend (TypeScript + Tailwind)
  app/                           Pages: dashboard, instance detail, terminal, settings, new, login
  components/AnsiLog.tsx         ANSI log renderer (DOMPurify sanitized)
  components/NavBar.tsx          Top navigation with logout button
  lib/api.ts                     Typed API client + WebSocket helpers (buildWsUrl, auth)
  lib/utils.ts                   Shared UI utilities (statusColor, statusLabel)
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
- **Frontend**: Next.js 16 App Router, TypeScript, Tailwind CSS v4, xterm.js for terminal, DOMPurify for log sanitization
- **Containers**: Docker SDK (`github.com/moby/moby/client`)
- **Base Image**: Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode + Oh My OpenCode

## Development

```bash
# Build the frontend first (required — binary embeds frontend/dist via go:embed)
cd frontend && bun install && bun run build && cd ..

# Build the Go binary
go build -o bin/cloudcode .

# Run backend (access token is required)
./bin/cloudcode --addr :8080 --access-token dev-token-changeme

# Run backend with CORS for a separate frontend dev server
./bin/cloudcode --addr :8080 --access-token dev-token-changeme --cors-origin http://localhost:3000

# Run frontend dev server (in frontend/)
# Set env vars so requests go directly to Go (not through Next.js proxy)
NEXT_PUBLIC_API_BASE=http://localhost:8080 \
NEXT_PUBLIC_WS_BASE=ws://localhost:8080 \
NEXT_PUBLIC_BACKEND_URL=http://localhost:8080 \
bun run dev

# Or set them permanently in frontend/.env.local:
# NEXT_PUBLIC_API_BASE=http://localhost:8080
# NEXT_PUBLIC_WS_BASE=ws://localhost:8080
# NEXT_PUBLIC_BACKEND_URL=http://localhost:8080

# Run backend in no-docker mode (UI preview only, no containers)
go run . --no-docker --addr :8080 --access-token dev-token-changeme

# Static analysis and build checks
go vet ./...
go build ./...
bun run build   # in frontend/

# Build base image
docker build -t cloudcode-base:latest -f docker/Dockerfile docker/

# Build platform image
docker build -t cloudcode:latest -f Dockerfile.platform .
```

> **Important:** `go build` embeds `frontend/dist/` at compile time via `//go:embed`. Always run `bun run build` in `frontend/` before `go build` when frontend changes are made.

## Security

- **Platform token auth** — All routes protected by session cookie; login rate-limited to 10 attempts / IP / 60s
- **Per-instance access tokens** — Each instance has a unique 32-byte hex token. Required to access the web UI (`?token=` or `_cc_inst_token_{id}` cookie) and for SDK/CLI access (`Authorization: Bearer` or `--password`). Tokens are enforced at two layers: the CloudCode proxy and OpenCode's native `OPENCODE_SERVER_PASSWORD` Basic Auth inside the container.
- **No host port exposure** — Container ports are not published to the host. Traffic routes exclusively through the CloudCode proxy via `cloudcode-net`.
- **Session management** — Existing session invalidated on re-login; sessions stored in memory (cleared on restart)
- **WS tokens** — One-time tokens for cross-origin WebSocket auth; 60s TTL, pruned by background goroutine
- **Path traversal protection** — All config file operations validated by `containedPath`; `dirName` restricted to an allowlist
- **No secret leakage** — `env_vars` excluded from all instance API responses; `auth.json` content excluded from the default settings response (load-on-demand via `GET /api/settings/file`)
- **XSS prevention** — Log output sanitized with DOMPurify before DOM insertion
- **Request limits** — Bodies limited to 1–10 MB via `http.MaxBytesReader`; logs WebSocket read limit 512 bytes
- **Security headers** — SPA responses include `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, and `Content-Security-Policy`
- **TLS** — `_cc_session` and `_cc_inst` cookies set `Secure` flag when served over HTTPS
