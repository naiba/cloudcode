# CloudCode

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![Docker](https://img.shields.io/badge/Docker-Required-2496ED?logo=docker)](https://www.docker.com)

A self-hosted management platform for [OpenCode](https://opencode.ai) instances. Spin up multiple isolated OpenCode environments as Docker containers and manage them through a single web dashboard.

[中文说明](README_zh.md)

## Features

- **Multi-instance management** — Create, start, stop, restart, and delete OpenCode instances
- **Session isolation** — Each instance has its own workspace; auth tokens are shared globally
- **Shared global config** — Manage `opencode.jsonc`, `AGENTS.md`, auth tokens, custom commands, agents, skills, and plugins from a unified Settings UI
- **skills.sh integration** — Install [skills.sh](https://skills.sh) skills inside any container, shared across all instances
- **Telegram notifications** — Built-in plugin sends Telegram messages on task completion/error
- **Dark/Light theme** — Follows system preference with manual toggle
- **Reverse proxy** — Access each instance's Web UI through a single entry point (`/instance/{id}/`)
- **Auto-updating containers** — OpenCode + Oh My OpenCode updated on each container start

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

### Telegram Notifications

Set these environment variables in Settings to receive notifications:

- `CC_TELEGRAM_BOT_TOKEN` — Your Telegram Bot API token
- `CC_TELEGRAM_CHAT_ID` — Target chat/group ID

The built-in plugin listens for `session.idle` (task completed) and `session.error` events.

## Tech Stack

- **Backend**: Go 1.25, `net/http` stdlib router, SQLite (via `modernc.org/sqlite`)
- **Frontend**: `html/template` + HTMX, vanilla CSS/JS, dark/light theme
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
