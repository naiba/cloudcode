# AGENTS.md — CloudCode

- Reply in English only.
- Prefer `bun` over `npm`.

## Project Overview

CloudCode is a Docker-based management platform for [OpenCode](https://opencode.ai) instances. Go backend + Docker orchestration + Next.js frontend. Each instance is a Docker container running `opencode web`, exposed through the platform's reverse proxy.

### Architecture

```
main.go                          Entry point, starts HTTP server
internal/
  config/config.go               Config file management (host-side read/write, bind mount generation)
  config/plugins/                Embedded built-in plugins (written to plugins/ on every startup)
  docker/manager.go              Docker container lifecycle (create/start/stop/delete)
  handler/handler.go             All HTTP handlers (JSON REST API)
  handler/memory_*.go            Platform-specific host RAM detection
  proxy/proxy.go                 Dynamic reverse proxy to each instance's OpenCode web UI
  store/store.go                 SQLite persistence (instance CRUD)
docker/
  Dockerfile                     Base image (Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode)
  entrypoint.sh                  Container startup script (updates deps + starts opencode web)
Dockerfile.platform              Multi-stage platform image build
frontend/                        Next.js 16 App Router (TypeScript + Tailwind CSS v4)
  app/                           Pages: dashboard, instance detail, terminal, settings, new instance
  components/AnsiLog.tsx         ANSI log renderer (safe DOM API)
  lib/api.ts                     Typed API client + WebSocket helpers
templates/                       Go html/template templates (legacy HTMX UI, still served)
static/                          CSS + JS for legacy UI
```

## Build & Run

```bash
# Build
go build -o bin/cloudcode .

# Run in dev mode (no Docker)
go run . --no-docker --addr :8080 --access-token dev-token

# Run with Docker
go run . --addr :8080 --access-token dev-token --image cloudcode-base:latest

# Run backend with CORS for frontend dev server
./bin/cloudcode --addr :9090 --access-token dev-token --cors-origin http://localhost:3000

# Frontend dev server (in frontend/)
bun install && bun run dev

# Build base image
docker build -t cloudcode-base:latest -f docker/Dockerfile docker/

# Build platform image
docker build -t cloudcode:latest -f Dockerfile.platform .
```

## Checks & Verification

```bash
go vet ./...
go build ./...
bun run build   # in frontend/
```

## Code Style

- **Go 1.25**, `net/http` stdlib router (`mux.HandleFunc("GET /path", handler)`), no web framework
- **modernc.org/sqlite** (pure Go, no CGO), JSON fields stored as `TEXT`
- **Next.js 16** App Router, TypeScript, Tailwind CSS v4, no additional UI frameworks
- Terminal uses xterm.js (local npm import, not CDN)
- CSS custom property theming, dark/light dual theme (`[data-theme="light"]`)

### Docker Integration

- Uses `github.com/moby/moby/client` official SDK
- Container naming: `cloudcode-{instanceID}`
- Network: custom bridge network `cloudcode-net`
- Global config injected via bind mounts; each instance uses a named volume (`cloudcode-home-{id}`) at `/root`
- Bind mount sub-paths take priority over parent volume paths
- Restart = delete container + recreate (volume preserved), triggering entrypoint to update deps
- Delete cleans up both container and named volume via `RemoveContainerAndVolume`
- Container port (`4096/tcp`) is published to `127.0.0.1:0` (random loopback-only host port); the proxy reads the assigned port via `GetContainerIPAndPort` and routes through `127.0.0.1:<hostPort>`
- Each container is started with `OPENCODE_SERVER_PASSWORD` set to its unique per-instance access token

### Per-Instance Access Tokens

Each instance has a unique 32-byte hex `access_token` stored in the DB. It is enforced at two layers:

1. **CloudCode proxy** (`proxy.go`) — validates token via `_cc_inst_token_{id}` cookie, `?token=` query param, or `Authorization: Bearer` header before forwarding any request
2. **OpenCode's own Basic Auth** — `OPENCODE_SERVER_PASSWORD` env var set at container creation provides defense-in-depth

**Web UI entry flow:**
- Navigate to `/instance/{id}/?token={access_token}`
- Proxy validates token, sets `_cc_inst_token_{id}` cookie, issues `303` redirect (strips token from URL with `Referrer-Policy: no-referrer`)
- Subsequent SPA/asset/WebSocket requests use the cookie

**SDK / CLI:**
```bash
opencode attach http://your-cloudcode-host/instance/{id}/ --password {access_token}
```

### WebSocket

- Uses `github.com/gorilla/websocket`
- `r.Context()` manages connection lifecycle (cancels when client disconnects)
- Server must send a close frame (`websocket.CloseMessage`) before closing to avoid client `onerror`
- `http.Server.WriteTimeout` must be `0` — a non-zero value tears down idle WebSocket sessions before the hijack completes
- Log stream: `GET /instances/{id}/logs/ws` — Docker logs follow, decoded via `stdcopy.StdCopy`
- Terminal: `GET /instances/{id}/terminal/ws` — Docker exec TTY, bidirectional bridge
- Terminal resize via JSON message `{"type":"resize","cols":N,"rows":N}`, server calls `ExecResize`

## Config Layout

| Storage | Container Path | Scope | Contents |
|---|---|---|---|
| `{dataDir}/config/opencode/` (bind mount) | `/root/.config/opencode/` | Global | opencode.jsonc, AGENTS.md, package.json, commands/, agents/, skills/, plugins/ |
| `{dataDir}/config/opencode-data/auth.json` (bind mount) | `/root/.local/share/opencode/auth.json` | Global | Auth tokens (shared across all instances) |
| `{dataDir}/config/dot-opencode/` (bind mount) | `/root/.opencode/` | Global | package.json |
| `{dataDir}/config/agents-skills/` (bind mount) | `/root/.agents/` | Global | skills.sh-installed skills and lock file |
| `cloudcode-home-{id}` (named volume) | `/root` | Per-instance | Workspace, cloned repos, session data, databases |

The `commands/`, `agents/`, `skills/`, and `plugins/` subdirectories are managed via the Settings page.

### Built-in Plugin

`_cloudcode-instructions.md` is embedded via `//go:embed` and written to the `plugins/` directory on every startup, ensuring it is always up to date.

## Reverse Proxy Architecture

Referer-based routing — response content is **not rewritten** (no HTML/CSS/JS path rewriting).

1. **Entry proxy** `/instance/{id}/` — validates instance token (cookie, `?token=`, or Bearer), strips prefix, forwards request, sets `_cc_inst` cookie
2. **Catch-all fallback** `"/"` — registered after all platform routes
   - Extracts instance ID from `Referer` header (`/instance/{id}/`)
   - Falls back to `_cc_inst` cookie (handles SPA pushState where Referer is lost)
   - Validates instance token for the resolved instance
   - Forwards original path unmodified
3. **No Referer and no cookie** → 404

The `_cc_inst` cookie is global (`Path=/`), so only one instance's Web UI is active at a time; opening a new instance overwrites the cookie. The per-instance token cookie (`_cc_inst_token_{id}`) is also global.

## Key Constraints

- All instances share global config; writes inside a container affect all instances (bind mount is read-write)
- No port pool — containers publish to `127.0.0.1:0` (Docker assigns a random loopback port)
- Resource limits: memory (MB) and CPU (cores) configurable at creation; 0 = unlimited
- Base image: Ubuntu 24.04 + Go 1.23 + Node 22 + Bun
- `oh-my-opencode` installed globally via `bun install -g`
- `cloudflared` pre-installed in each container
- Playwright Chromium pre-installed, symlinked to `/usr/bin/chromium-browser` and `/usr/bin/chrome`

## When Modifying Code

- After changing Go: run `go vet ./...` and `go build ./...`
- After changing Dockerfile: verify with `docker build -t cloudcode-base:latest -f docker/Dockerfile docker/`
- After changing frontend: run `bun run build` in `frontend/`
- New routes: add to `RegisterRoutes` in `handler.go` following existing format
- New config files: update the relevant slices and `EditableFiles()` in `config.go`
