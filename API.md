# CloudCode JSON API Reference

This document reflects the actual Go backend implementation.
All endpoints are served by the Go backend. The frontend communicates exclusively via these APIs.

## Conventions

### Base URL
```
http://localhost:8080
```

### Authentication

All endpoints except `POST /api/auth/login` require a valid session cookie (`_cc_session`).

Unauthenticated requests to `/api/*` paths return `401 {"error":"authentication required"}`.
Unauthenticated browser requests to other paths are redirected to `/login`.

### Request format
- All request bodies are `application/json`
- `GET` / `DELETE` parameters are passed as query strings

### Response format
All responses are `application/json` unless noted otherwise (WebSocket, file proxy).

**Success** — the raw object or array is returned directly (no wrapper).

**Error** — all errors use a consistent shape:
```json
{ "error": "human-readable message" }
```

### Common HTTP status codes

| Code | Meaning |
|------|---------|
| `200` | Success with body |
| `201` | Resource created |
| `204` | Success, no body |
| `400` | Bad request / validation error |
| `401` | Authentication required |
| `404` | Resource not found |
| `409` | Conflict (e.g. duplicate name) |
| `429` | Too many requests (login rate limit) |
| `500` | Internal server error |

### Timestamps
ISO 8601 strings: `"2026-03-05T01:37:44Z"`

---

## Authentication API

### Login

```
POST /api/auth/login
```

Validates the admin token and creates a session. Sets the `_cc_session` cookie (HttpOnly, SameSite=Lax, 30-day MaxAge, Secure on HTTPS). Any existing session cookie for the request is invalidated first.

**Rate limit:** 10 attempts per IP per 60 seconds.

**Request body:**
```json
{ "token": "your-admin-token" }
```

**Response `200`:**
```json
{ "status": "ok" }
```

**Response `401`:**
```json
{ "error": "invalid token" }
```

**Response `429`:**
```json
{ "error": "too many login attempts, try again later" }
```

---

### Logout

```
POST /api/auth/logout
```

Invalidates the current session and clears the `_cc_session` cookie. Safe to call even when not logged in.

**Response `200`:**
```json
{ "status": "ok" }
```

---

### Get WebSocket auth token

```
GET /api/auth/ws-token
```

Issues a single-use token for authenticating a cross-origin WebSocket connection. The token is consumed on first use and expires after 60 seconds if unused.

Use this when the frontend runs on a different origin from the Go backend (dev mode), since browsers do not send cookies on cross-origin WebSocket upgrades.

**Response `200`:**
```json
{ "token": "a3f9..." }
```

**Usage:**
```js
const { token } = await fetch(`${BASE}/api/auth/ws-token`, { credentials: 'include' }).then(r => r.json())
const ws = new WebSocket(`ws://localhost:8080/instances/${id}/logs/ws?token=${encodeURIComponent(token)}`)
```

---

## Data Models

### Instance object

```json
{
  "id":           "4fa20c08",
  "name":         "my-project",
  "container_id": "14302ee2393541cc2eaf...",
  "status":       "running",
  "error_msg":    "",
  "port":         10008,
  "work_dir":     "/root",
  "memory_mb":    2048,
  "cpu_cores":    2.0,
  "created_at":   "2026-03-05T01:37:44Z",
  "updated_at":   "2026-03-05T01:38:10Z"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | 8-character unique ID (first 8 chars of UUIDv4) |
| `name` | string | Human-readable name |
| `container_id` | string | Full Docker container ID; empty string if not yet created |
| `status` | string | See status values below |
| `error_msg` | string | Last error message; empty string when healthy |
| `work_dir` | string | Working directory inside the container (always `/root`) |
| `memory_mb` | integer | Memory limit in MB; `0` = unlimited |
| `cpu_cores` | number | CPU core limit (fractional allowed); `0` = unlimited |
| `access_token` | string | Per-instance access token. Required to open the web UI or connect via SDK. |
| `created_at` | string | ISO 8601 timestamp |
| `updated_at` | string | ISO 8601 timestamp |

> **Note:** `env_vars` is intentionally excluded from all API responses to prevent leaking secrets. Env vars are managed via the Settings API.

### Per-instance access token

Each instance has a unique `access_token` generated at creation. It is required to access the instance's web UI or connect via the OpenCode SDK/CLI.

**Web UI:** Navigate to `/instance/{id}/?token={access_token}`. The proxy validates the token, sets a cookie (`_cc_inst_token_{id}`), and redirects to strip the token from the URL. Subsequent requests (assets, WebSocket) use the cookie automatically.

**SDK / CLI:**
```bash
opencode attach http://your-cloudcode-host/instance/{id}/ --password {access_token}
```
```ts
import { createOpencodeClient } from "@opencode-ai/sdk"
const client = createOpencodeClient({
  baseUrl: "http://your-cloudcode-host/instance/{id}/",
  headers: { Authorization: `Bearer ${accessToken}` },
})
```

The token is enforced at two layers:
1. The CloudCode proxy validates it before forwarding (cookie, `?token=` param, or `Authorization: Bearer`)
2. OpenCode's own `OPENCODE_SERVER_PASSWORD` Basic Auth inside the container provides defense-in-depth

Container ports are published to `127.0.0.1` on a random loopback-only host port (inaccessible from the network). All external traffic routes through the CloudCode proxy.

**Status values:**
- `created` — record exists, container not yet started
- `running` — container is running, OpenCode web UI is accessible
- `stopped` — container stopped gracefully
- `exited` — container exited on its own (detected on next sync)
- `error` — last operation failed; see `error_msg`

---

## Instances API

### List instances

```
GET /api/instances
```

Returns all instances ordered by `created_at` descending. Syncs Docker status for any instance that has a `container_id`.

**Response `200`:** Array of Instance objects. Returns `[]` (not `null`) when empty.

---

### Get instance

```
GET /api/instances/{id}
```

Returns a single instance. Syncs Docker status if the instance has a `container_id`.

**Response `200`:** Instance object

**Response `404`:**
```json
{ "error": "instance not found" }
```

---

### Create instance

```
POST /api/instances
```

Creates the DB record, generates a per-instance access token, and starts the Docker container synchronously.
Blocks until the container is running or fails.

**Request body:**
```json
{
  "name":       "my-project",
  "memory_mb":  2048,
  "cpu_cores":  2.0
}
```

| Field | Type | Required | Default | Notes |
|-------|------|----------|---------|-------|
| `name` | string | yes | — | Whitespace trimmed; must be non-empty after trim |
| `memory_mb` | integer | no | `0` | `0` = unlimited |
| `cpu_cores` | number | no | `0` | `0` = unlimited |

**Response `201`:** Instance object. `status` is `"running"` on success or `"error"` if the container failed to start (the record is still saved).

**Response `400`:**
```json
{ "error": "name is required" }
{ "error": "invalid request body" }
```

**Response `409`:**
```json
{ "error": "instance name already exists" }
```

---

### Regenerate instance token

```
POST /api/instances/{id}/regenerate-token
```

Generates a new `access_token` for the instance and updates the DB and proxy immediately. The **container must be restarted** for the new token to take effect inside the OpenCode server (it updates `OPENCODE_SERVER_PASSWORD` only at container creation).

**Response `200`:**
```json
{ "access_token": "new64charhextoken..." }
```

**Response `404`:**
```json
{ "error": "instance not found" }
```

---

### Delete instance

```
DELETE /api/instances/{id}
```

Stops and removes the Docker container and its named volume (`cloudcode-home-{id}`), releases the port, cleans up instance config data, and deletes the DB record. Irreversible.

Container removal uses a 30-second timeout. Errors during container removal are logged but do not block the deletion of the DB record.

**Response `204`:** No body

**Response `404`:**
```json
{ "error": "instance not found" }
```

---

### Start instance

```
POST /api/instances/{id}/start
```

If the instance has no `container_id`, creates a new container. If it has an existing container, starts it.

**Response `200`:** Updated Instance object (status `"running"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

---

### Stop instance

```
POST /api/instances/{id}/stop
```

Sends a stop signal to the container (30-second graceful timeout via Docker). Unregisters the reverse proxy.

**Response `200`:** Updated Instance object (status `"stopped"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

> **Note:** This call blocks for up to 30 seconds while Docker waits for the container to exit gracefully.

---

### Restart instance

```
POST /api/instances/{id}/restart
```

Stops and removes the old container, then creates a fresh one. Re-runs `entrypoint.sh`, updating OpenCode and all dependencies. The named volume (`/root`) is preserved — code and session data survive. The existing `access_token` is reused (no change needed).

**Response `200`:** Updated Instance object (status `"running"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

---

### Poll instance status

```
GET /api/instances/{id}/status?s={currentStatus}
```

Lightweight status check. Syncs Docker status and returns the instance only if status changed.

| Param | Type | Description |
|-------|------|-------------|
| `s` | string | The status currently displayed in the frontend (e.g. `running`) |

**Response `204`:** No body — status is unchanged

**Response `200` with body:** Updated Instance object — status changed

**Response `200` with empty body:** Instance not found (deleted) — frontend should remove it

---

### Batch poll instance statuses

```
POST /api/status/instances
```

Check multiple instances in a single request. Each instance is checked against Docker in parallel. Only instances whose status changed (or were deleted) are included in the response — unchanged instances are absent.

Use this on the dashboard instead of polling each instance individually.

**Request body:**
```json
{
  "ids": {
    "4fa20c08": "running",
    "a1b2c3d4": "stopped"
  }
}
```

The map value is the status currently known to the client.

**Response `200`:**
```json
{
  "changed": {
    "4fa20c08": { ...Instance object... },
    "a1b2c3d4": null
  }
}
```

- Instance object — status changed; use updated data
- `null` — instance was deleted; remove from UI

Instances whose status matches the client's known value are absent from `changed`.

**Frontend polling pattern:**
```js
// Build a map of current known statuses
const statuses = Object.fromEntries(instances.map(i => [i.id, i.status]))
const { changed } = await fetch('/api/status/instances', {
  method: 'POST',
  credentials: 'include',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ ids: statuses }),
}).then(r => r.json())

for (const [id, updated] of Object.entries(changed)) {
  if (updated === null) removeInstance(id)
  else updateInstance(updated)
}
```

---

## System API

### Get host resource info

```
GET /api/system/resources
```

Returns the host machine's total memory and CPU count. On Linux, reads `/proc/meminfo` for accurate total memory. Falls back to Go's `runtime.MemStats.Sys` on non-Linux.

**Response `200`:**
```json
{
  "total_memory_mb": 16384,
  "total_cpu_cores": 8
}
```

---

## Real-time Endpoints (WebSocket)

All WebSocket endpoints accept authentication via:
1. Session cookie (`_cc_session`) — works for same-origin connections
2. One-time token query param (`?token=...`) — for cross-origin dev setups (see `GET /api/auth/ws-token`)

### Log stream

```
WS /instances/{id}/logs/ws
```

Streams Docker container logs (last 200 lines + follow). Returns HTTP 400 if the instance has no container. The server does not process incoming messages (read limit: 512 bytes).

**Protocol:**
- Server → Client: `BinaryMessage` — raw log chunk (may contain ANSI escape codes)
- Server → Client: `CloseMessage` with `CloseNormalClosure` when the log stream ends

**Usage:**
```js
const ws = new WebSocket(`ws://localhost:8080/instances/${id}/logs/ws`)
ws.binaryType = 'arraybuffer'
ws.onmessage = (e) => {
  const text = new TextDecoder().decode(e.data)
  // text may contain ANSI escape codes — sanitize before rendering
}
```

---

### Terminal

```
WS /instances/{id}/terminal/ws
```

Full interactive PTY session inside the container (`/bin/bash -l`). Returns HTTP 400 if the instance has no container.

Resize dimensions are clamped to 1–500 for both cols and rows.

**Protocol:**
- Client → Server: `BinaryMessage` — raw terminal input (stdin bytes)
- Client → Server: `TextMessage` JSON — terminal resize: `{"type":"resize","cols":220,"rows":50}`
- Server → Client: `BinaryMessage` — raw terminal output (stdout + stderr combined, TTY mode)

**Usage (with xterm.js):**
```js
const ws = new WebSocket(`ws://localhost:8080/instances/${id}/terminal/ws`)
ws.binaryType = 'arraybuffer'
ws.onopen = () => ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
ws.onmessage = (e) => term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data)
term.onData((data) => ws.send(new TextEncoder().encode(data)))
term.onResize(({ cols, rows }) => ws.send(JSON.stringify({ type: 'resize', cols, rows })))
```

---

## Proxy Routes

These routes proxy traffic directly to the OpenCode web UI running inside the container.
All proxy routes require authentication.

### Open OpenCode Web UI

```
GET /instance/{id}/
```

Reverse-proxies all traffic (HTTP + WebSocket) to `http://127.0.0.1:{port}`. Sets `_cc_inst` cookie (path `/`, HttpOnly, SameSite=Lax) for SPA fallback routing.

```js
window.open(`http://localhost:8080/instance/${id}/`, '_blank')
```

> **Important:** The trailing slash is required. Go to the backend directly — do not route through the Next.js dev server.

### Catch-all fallback

```
GET /   (all unmatched paths)
```

- `/login` — always public; serves the embedded SPA
- `/api/*` unmatched → `404 {"error":"not found"}`
- `/instance/{id}` (no trailing slash) → `301` redirect to `/instance/{id}/`
- Authenticated requests with a `Referer` containing `/instance/{id}/` → proxy to that instance
- Authenticated requests with a `_cc_inst` cookie → proxy to the cookie's instance
- Authenticated other paths → serve embedded SPA (`frontend/dist/index.html`)
- Unauthenticated non-login paths → `302` redirect to `/login`

---

## Settings API

### Get all settings

```
GET /api/settings
```

Returns all global settings data needed to render the settings page.

> **Note:** `auth.json` content is **not** included in this response (`content` is `null`). Fetch it explicitly via `GET /api/settings/file?path=opencode-data/auth.json` when the user requests it.

**Response `200`:**
```json
{
  "config_dir": "/path/to/data/config",
  "env_vars": [
    { "key": "ANTHROPIC_API_KEY", "value": "sk-ant-..." }
  ],
  "config_files": [
    {
      "name":     "opencode.jsonc",
      "rel_path": "opencode/opencode.jsonc",
      "hint":     "OpenCode main config (providers, MCP servers, plugins)",
      "content":  "{}"
    },
    {
      "name":     "auth.json",
      "rel_path": "opencode-data/auth.json",
      "hint":     "API keys and OAuth tokens (Anthropic, OpenAI, etc.)",
      "content":  null
    }
  ],
  "dirs": {
    "commands": [ { "name": "daily-standup.md", "rel_path": "opencode/commands/daily-standup.md" } ],
    "agents":   [],
    "skills":   [],
    "plugins":  [
      { "name": "_cloudcode-instructions.md", "rel_path": "opencode/plugins/_cloudcode-instructions.md" }
    ]
  },
  "agents_skills": [
    { "skill_name": "some-skill", "rel_path": "agents-skills/skills/some-skill/SKILL.md" }
  ],
  "directory_mappings": [
    { "host": "/path/to/data/config/opencode/",               "container": "/root/.config/opencode/" },
    { "host": "/path/to/data/config/opencode-data/auth.json", "container": "/root/.local/share/opencode/auth.json" },
    { "host": "/path/to/data/config/dot-opencode/",           "container": "/root/.opencode/" },
    { "host": "/path/to/data/config/agents-skills/",          "container": "/root/.agents/" }
  ]
}
```

**Notes:**
- `env_vars` is an array (not a map); order is not guaranteed.
- `dirs.plugins` always contains `_cloudcode-instructions.md` (written on every server start). Treat it as a read-only built-in in the UI.
- `config_files[].content` is `null` for `auth.json` — load it on-demand with `GET /api/settings/file`.

---

### Save environment variables

```
PUT /api/settings/env
```

Replaces the entire set of global environment variables stored in `data/config/env.json`. Empty keys are silently ignored.

**Request body:**
```json
{
  "vars": [
    { "key": "ANTHROPIC_API_KEY", "value": "sk-ant-..." }
  ]
}
```

To clear all variables: `{"vars": []}`

**Response `204`:** No body

---

### Read config file

```
GET /api/settings/file?path={rel_path}
```

Reads a config file by its `rel_path` (relative to `data/config/`). Use this to load `auth.json` on demand.

| Param | Type | Description |
|-------|------|-------------|
| `path` | string | Relative path, e.g. `opencode/opencode.jsonc` or `opencode-data/auth.json` |

**Response `200`:**
```json
{
  "rel_path": "opencode/opencode.jsonc",
  "content":  "{}"
}
```

Returns `{"rel_path": "...", "content": ""}` if the file does not exist yet.

---

### Save config file

```
PUT /api/settings/file
```

Writes content to a config file (relative to `data/config/`). Creates the file and any missing parent directories.

**Request body:**
```json
{
  "path":    "opencode/opencode.jsonc",
  "content": "{}"
}
```

**Response `204`:** No body

---

### List directory files

```
GET /api/settings/dir-files?dir={dir_name}
```

Lists files inside a managed config directory (`data/config/opencode/{dir}/`).
Directories containing a `SKILL.md` are returned as `dirname/SKILL.md` entries.

| Param | Type | Valid values |
|-------|------|-------------|
| `dir` | string | `commands` · `agents` · `skills` · `plugins` |

Any other value returns `400`. This is an explicit allowlist — no other directories can be listed.

**Response `200`:** Array of DirFile objects. Returns `[]` when empty.

```json
[
  { "name": "daily-standup.md", "rel_path": "opencode/commands/daily-standup.md" }
]
```

**Response `400`:**
```json
{ "error": "invalid dir: must be one of commands, agents, skills, plugins" }
```

---

### Create or update directory file

```
PUT /api/settings/dir-file
```

Creates or overwrites a file at `data/config/opencode/{dir}/{filename}`.

`dir` must be one of `commands`, `agents`, `skills`, `plugins`.

**Request body:**
```json
{
  "dir":      "commands",
  "filename": "daily-standup.md",
  "content":  "---\ndescription: Run daily standup\n---\n..."
}
```

**Response `204`:** No body

**Response `400`:**
```json
{ "error": "invalid dir: must be one of commands, agents, skills, plugins" }
```

---

### Delete directory file

```
DELETE /api/settings/dir-file?path={rel_path}
```

Deletes a file from a managed config directory. Also removes the parent directory if it becomes empty.

| Param | Type | Description |
|-------|------|-------------|
| `path` | string | Relative path, e.g. `opencode/commands/daily-standup.md` |

**Response `204`:** No body

---

### Delete agents skill

```
DELETE /api/settings/agents-skill?name={skill_name}
```

Removes an entire skill directory from `data/config/agents-skills/skills/{name}/`.

| Param | Type | Description |
|-------|------|-------------|
| `name` | string | Skill directory name, e.g. `some-skill` |

**Response `204`:** No body

---

## CORS

### API CORS (`--cors-origin`)

Applies to all platform API routes. Enabled only when `--cors-origin` is passed.
Accepts a comma-separated list. The matched request `Origin` is reflected back (required for multi-origin CORS with credentials).

```
Access-Control-Allow-Origin: <matched origin>
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type, Authorization
Access-Control-Allow-Credentials: true
```

Used in dev when the frontend runs on a different port:
```bash
./bin/cloudcode --addr :8080 --access-token mytoken --cors-origin http://localhost:3000,http://localhost:4000
```

> **Note:** `--proxy-cors-origin` has been removed. Container ports are bound to `127.0.0.1` (loopback only), so browsers can never reach the OpenCode server directly — only the Go proxy does. The platform CORS middleware (`--cors-origin`) covers all browser-facing traffic.

---

## Complete endpoint index

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/api/auth/login` | public | Log in, get session cookie |
| `POST` | `/api/auth/logout` | public | Log out, clear session cookie |
| `GET` | `/api/auth/ws-token` | session | Get one-time WebSocket auth token |
| `GET` | `/api/instances` | session | List all instances (syncs Docker status) |
| `POST` | `/api/instances` | session | Create and start instance |
| `GET` | `/api/instances/{id}` | session | Get instance (syncs Docker status) |
| `DELETE` | `/api/instances/{id}` | session | Delete instance + container + volume |
| `POST` | `/api/instances/{id}/start` | session | Start instance |
| `POST` | `/api/instances/{id}/stop` | session | Stop instance (blocks up to 30s) |
| `POST` | `/api/instances/{id}/restart` | session | Restart instance (recreates container) |
| `POST` | `/api/instances/{id}/regenerate-token` | session | Generate a new per-instance access token |
| `GET` | `/api/instances/{id}/status?s=` | session | Poll single status (204 if unchanged) |
| `POST` | `/api/status/instances` | session | Batch poll statuses (returns only changed) |
| `GET` | `/api/system/resources` | session | Host memory + CPU totals |
| `WS` | `/instances/{id}/logs/ws` | session or token | Live log stream (binary ANSI) |
| `WS` | `/instances/{id}/terminal/ws` | session or token | Interactive PTY terminal |
| `GET` | `/api/settings` | session | Get all settings |
| `PUT` | `/api/settings/env` | session | Replace all env vars |
| `GET` | `/api/settings/file?path=` | session | Read config file |
| `PUT` | `/api/settings/file` | session | Write config file |
| `GET` | `/api/settings/dir-files?dir=` | session | List managed dir files |
| `PUT` | `/api/settings/dir-file` | session | Create/update dir file |
| `DELETE` | `/api/settings/dir-file?path=` | session | Delete dir file |
| `DELETE` | `/api/settings/agents-skill?name=` | session | Delete agents skill directory |
| `GET` | `/instance/{id}/` | session + instance token | Proxy to OpenCode Web UI (trailing slash required; `?token=` or cookie or Bearer) |
| `GET` | `/login` | public | Login page (SPA) |
| `GET` | `/` (catch-all) | mixed | API 404 / proxy fallback / SPA index |
