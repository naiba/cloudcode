# CloudCode

A self-hosted management platform for [OpenCode](https://opencode.ai) instances. Spin up multiple isolated OpenCode environments as Docker containers and manage them through a single web dashboard.

[中文说明](README_zh.md)

## Features

- **Multi-instance management** — Create, start, stop, restart, and delete OpenCode instances
- **Shared global config** — Manage `opencode.json`, `AGENTS.md`, auth tokens, custom commands, agents, skills, and plugins from a unified Settings UI
- **Reverse proxy** — Access each instance's Web UI through a single entry point (`/instance/{id}/`)
- **Auto-updating containers** — Base image includes OpenCode + Oh My OpenCode, updated on each container start
- **Dark-themed dashboard** — Server-rendered HTMX frontend, no build step required

## Quick Start

### Docker Compose (Recommended)

```bash
mkdir cloudcode && cd cloudcode
curl -O https://raw.githubusercontent.com/naiba/cloudcode/main/docker-compose.yml
docker compose up -d
```

Open http://localhost:8080 in your browser.

Images are pulled from `ghcr.io/naiba/cloudcode` and `ghcr.io/naiba/cloudcode-base` automatically.

## Architecture

```
Browser → CloudCode Platform (Go + HTMX)
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

Each container runs `opencode web` and is accessible through the platform's reverse proxy. All containers share the same global configuration via bind mounts.

## Configuration

Global config is managed through the Settings page and bind-mounted into all containers:

| Host Path | Container Path | Contents |
|---|---|---|
| `data/config/opencode/` | `/root/.config/opencode/` | `opencode.json`, `AGENTS.md`, `package.json`, etc. |
| `data/config/opencode-data/` | `/root/.local/share/opencode/` | `auth.json` |
| `data/config/dot-opencode/` | `/root/.opencode/` | `package.json` |

Subdirectories `commands/`, `agents/`, `skills/`, and `plugins/` are also managed through the Settings UI.

Environment variables (e.g. `ANTHROPIC_API_KEY`, `GH_TOKEN`) are configured in Settings and injected into all containers.

## Tech Stack

- **Backend**: Go 1.25, `net/http` stdlib router, SQLite (via `modernc.org/sqlite`)
- **Frontend**: `html/template` + HTMX, vanilla CSS/JS
- **Containers**: Docker SDK (`github.com/moby/moby/client`)
- **Base Image**: Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode + Oh My OpenCode

## Development

```bash
# Run in dev mode (no Docker required)
go run . --no-docker --addr :8080

# Static analysis
go vet ./...

# Build check
go build ./...
```

