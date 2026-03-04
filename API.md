# CloudCode JSON API Reference

This document reflects the actual Go backend implementation.
All endpoints are served by the Go backend. The frontend communicates exclusively via these APIs.

## Conventions

### Base URL
```
http://localhost:9090
```

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
| `404` | Resource not found |
| `409` | Conflict (e.g. duplicate name) |
| `500` | Internal server error |
| `503` | Service unavailable (no ports left) |

### Timestamps
ISO 8601 strings, timezone-local (not forced UTC): `"2026-03-05T01:37:44.262535+08:00"`

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
  "env_vars":     {},
  "memory_mb":    2048,
  "cpu_cores":    2.0,
  "created_at":   "2026-03-05T01:37:44.262535+08:00",
  "updated_at":   "2026-03-05T01:38:10.470417+08:00"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | 8-character unique ID (first 8 chars of UUIDv4) |
| `name` | string | Human-readable name |
| `container_id` | string | Full Docker container ID; empty string if not yet created |
| `status` | string | See status values below |
| `error_msg` | string | Last error message; empty string when healthy |
| `port` | integer | Host port (10000–10100) the OpenCode web UI is published on |
| `work_dir` | string | Working directory inside the container (always `/root`) |
| `env_vars` | object | Per-instance env var map (currently always `{}` — global env is managed separately via Settings) |
| `memory_mb` | integer | Memory limit in MB; `0` = unlimited |
| `cpu_cores` | number | CPU core limit (fractional allowed); `0` = unlimited |
| `created_at` | string | ISO 8601 timestamp |
| `updated_at` | string | ISO 8601 timestamp |

**Status values:**
- `created` — record exists, container not yet started
- `running` — container is running, OpenCode web UI is accessible
- `stopped` — container stopped gracefully
- `exited` — container exited on its own (detected on next sync)
- `error` — last operation failed; see `error_msg`

> **Note:** Unlike the original design, there is no `removed` status. If an instance is deleted externally, the next `GET /api/instances` sync may update the status; the record is only removed by `DELETE /api/instances/{id}`.

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

Creates the DB record, allocates a port, and starts the Docker container synchronously.
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

**Response `503`:**
```json
{ "error": "no available ports in range 10000-10100" }
```

**Response `500`:**
```json
{ "error": "failed to create instance" }
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

**Response `500`:**
```json
{ "error": "failed to delete instance" }
```

---

### Start instance

```
POST /api/instances/{id}/start
```

If the instance has no `container_id`, creates a new container. If it has an existing container, starts it.

**Request body:** empty / none

**Response `200`:** Updated Instance object (status `"running"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

**Response `500`:**
```json
{ "error": "docker is not available" }
{ "error": "failed to create container: ..." }
{ "error": "failed to start container: ..." }
```

---

### Stop instance

```
POST /api/instances/{id}/stop
```

Sends a stop signal to the container (30-second graceful timeout via Docker). Unregisters the reverse proxy. If the instance has no container, only updates the DB record status to `"stopped"`.

**Request body:** empty / none

**Response `200`:** Updated Instance object (status `"stopped"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

**Response `500`:**
```json
{ "error": "failed to stop container: ..." }
```

> **Note:** This call blocks for up to 30 seconds while Docker waits for the container to exit gracefully. Ensure the HTTP client timeout is longer than 30 seconds.

---

### Restart instance

```
POST /api/instances/{id}/restart
```

Stops and removes the old container (errors ignored), then creates a fresh one. Re-runs `entrypoint.sh`, updating OpenCode and all dependencies. The named volume (`/root`) is preserved — code and session data survive.

**Request body:** empty / none

**Response `200`:** Updated Instance object (status `"running"`)

**Response `404`:**
```json
{ "error": "instance not found" }
```

**Response `500`:**
```json
{ "error": "docker is not available" }
{ "error": "failed to restart container: ..." }
```

---

### Poll instance status

```
GET /api/instances/{id}/status?s={currentStatus}
```

Lightweight status check. Syncs Docker status and returns the instance only if status changed.

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `s` | string | The status currently displayed in the frontend (e.g. `running`) |

**Response `204`:** No body — status is unchanged

**Response `200` with body:** Updated Instance object — status changed

**Response `200` with empty body:** Instance not found (deleted) — frontend should remove it from the list

**Frontend polling pattern:**
```js
async function pollStatus(id, currentStatus) {
  const res = await fetch(`/api/instances/${id}/status?s=${currentStatus}`)
  if (res.status === 204) return null                    // no change
  const text = await res.text()
  if (!text) return { deleted: true }                    // instance removed
  return JSON.parse(text)                                // updated instance
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

### Log stream

```
WS /instances/{id}/logs/ws
```

Streams Docker container logs (last 200 lines + follow). Returns HTTP 400 if the instance has no container.

**Protocol:**
- Server → Client: `TextMessage` — raw log chunk (may contain ANSI escape codes)
- Server → Client: `CloseMessage` with `CloseNormalClosure` when the log stream ends
- Client → Server: any message causes the stream to close

**Usage:**
```js
const ws = new WebSocket(`ws://localhost:9090/instances/${id}/logs/ws`)
ws.onmessage = (e) => { /* e.data is a raw text chunk with ANSI codes */ }
```

---

### Terminal

```
WS /instances/{id}/terminal/ws
```

Full interactive PTY session inside the container (`/bin/bash -l`). Returns HTTP 400 if the instance has no container.

**Protocol:**
- Client → Server: `BinaryMessage` — raw terminal input (stdin bytes)
- Client → Server: `TextMessage` JSON — terminal resize: `{"type":"resize","cols":220,"rows":50}`
- Server → Client: `BinaryMessage` — raw terminal output (stdout + stderr combined, TTY mode)

**Usage (with xterm.js):**
```js
const ws = new WebSocket(`ws://localhost:9090/instances/${id}/terminal/ws`)
ws.binaryType = 'arraybuffer'
ws.onopen = () => ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
ws.onmessage = (e) => term.write(e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data)
term.onData((data) => ws.send(new TextEncoder().encode(data)))
term.onResize(({ cols, rows }) => ws.send(JSON.stringify({ type: 'resize', cols, rows })))
```

---

## Proxy Routes

These routes proxy traffic directly to the OpenCode web UI running inside the container.

### Open OpenCode Web UI

```
GET /instance/{id}/
```

Reverse-proxies all traffic (HTTP + WebSocket) to `http://127.0.0.1:{port}`. Sets `_cc_inst` cookie (path `/`, HttpOnly, SameSite=Lax) for SPA fallback routing.

**Frontend usage:**
```js
window.open(`http://localhost:9090/instance/${id}/`, '_blank')
```

> **Important:** The trailing slash is required. The URL must go directly to the Go backend — do not route through the Next.js dev server, which will strip the trailing slash and break the proxy.

### Catch-all fallback

```
GET /   (all unmatched paths)
```

- Paths starting with `/api/` → `404 {"error":"not found"}`
- Requests with a `Referer` containing `/instance/{id}/` → proxy to that instance
- Requests with a `_cc_inst` cookie → proxy to the cookie's instance
- Otherwise → serve `frontend/dist/index.html` (SPA fallback for production)

---

## Settings API

### Get all settings

```
GET /api/settings
```

Returns all global settings data needed to render the settings page.

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
      "name":     "oh-my-opencode.json",
      "rel_path": "opencode/oh-my-opencode.json",
      "hint":     "Oh My OpenCode config (agent/category model assignments)",
      "content":  "{}"
    },
    {
      "name":     "AGENTS.md",
      "rel_path": "opencode/AGENTS.md",
      "hint":     "Global rules shared across all instances (~/.config/opencode/AGENTS.md)",
      "content":  ""
    },
    {
      "name":     "auth.json",
      "rel_path": "opencode-data/auth.json",
      "hint":     "API keys and OAuth tokens (Anthropic, OpenAI, etc.)",
      "content":  "{}"
    },
    {
      "name":     "~/.config/opencode/package.json",
      "rel_path": "opencode/package.json",
      "hint":     "OpenCode plugin dependencies",
      "content":  "{}"
    },
    {
      "name":     "~/.opencode/package.json",
      "rel_path": "dot-opencode/package.json",
      "hint":     "Core plugin dependencies",
      "content":  "{}"
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
    { "host": "/path/to/data/config/opencode/",                  "container": "/root/.config/opencode/" },
    { "host": "/path/to/data/config/opencode-data/auth.json",    "container": "/root/.local/share/opencode/auth.json" },
    { "host": "/path/to/data/config/dot-opencode/",              "container": "/root/.opencode/" },
    { "host": "/path/to/data/config/agents-skills/",             "container": "/root/.agents/" }
  ]
}
```

**Notes:**
- `env_vars` is an array (not a map) but **order is not guaranteed** — the Go map iteration is non-deterministic.
- `dirs.plugins` will always contain `_cloudcode-instructions.md` (written on every server start). This should be treated as a read-only built-in plugin in the UI.
- `directory_mappings.host` uses the actual filesystem path (not `{config_dir}` placeholder).

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

**Response `400`:**
```json
{ "error": "invalid request body" }
```

**Response `500`:**
```json
{ "error": "failed to save environment variables: ..." }
```

---

### Read config file

```
GET /api/settings/file?path={rel_path}
```

Reads a config file by its `rel_path` (relative to `data/config/`).

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `path` | string | Relative path, e.g. `opencode/opencode.jsonc` |

**Response `200`:**
```json
{
  "rel_path": "opencode/opencode.jsonc",
  "content":  "{}"
}
```

Returns `{"rel_path": "...", "content": ""}` if the file does not exist yet.

**Response `400`:**
```json
{ "error": "path is required" }
```

**Response `500`:**
```json
{ "error": "failed to read file: ..." }
```

---

### Save config file

```
PUT /api/settings/file
```

Writes content to a config file (relative to `data/config/`). Creates the file and any missing parent directories if they don't exist.

**Request body:**
```json
{
  "path":    "opencode/opencode.jsonc",
  "content": "{}"
}
```

**Response `204`:** No body

**Response `400`:**
```json
{ "error": "path is required" }
{ "error": "invalid request body" }
```

**Response `500`:**
```json
{ "error": "failed to save file: ..." }
```

---

### List directory files

```
GET /api/settings/dir-files?dir={dir_name}
```

Lists files inside a managed config directory (`data/config/opencode/{dir}/`).
Directories containing a `SKILL.md` are returned as `dirname/SKILL.md` entries.

**Query parameters:**

| Param | Type | Valid values |
|-------|------|-------------|
| `dir` | string | `commands` · `agents` · `skills` · `plugins` |

**Response `200`:** Array of DirFile objects. Returns `[]` when empty.

```json
[
  { "name": "daily-standup.md", "rel_path": "opencode/commands/daily-standup.md" }
]
```

**Response `400`:**
```json
{ "error": "dir is required" }
```

**Response `500`:**
```json
{ "error": "failed to list files: ..." }
```

---

### Create or update directory file

```
PUT /api/settings/dir-file
```

Creates or overwrites a file at `data/config/opencode/{dir}/{filename}`.

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
{ "error": "dir and filename are required" }
{ "error": "invalid request body" }
```

**Response `500`:**
```json
{ "error": "failed to save file: ..." }
```

---

### Delete directory file

```
DELETE /api/settings/dir-file?path={rel_path}
```

Deletes a file from a managed config directory. Also removes the parent directory if it becomes empty afterward.

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `path` | string | Relative path, e.g. `opencode/commands/daily-standup.md` |

**Response `204`:** No body

**Response `400`:**
```json
{ "error": "path is required" }
```

**Response `500`:**
```json
{ "error": "failed to delete file: ..." }
```

---

### Delete agents skill

```
DELETE /api/settings/agents-skill?name={skill_name}
```

Removes an entire skill directory from `data/config/agents-skills/skills/{name}/`.

**Query parameters:**

| Param | Type | Description |
|-------|------|-------------|
| `name` | string | Skill directory name, e.g. `some-skill` |

**Response `204`:** No body

**Response `400`:**
```json
{ "error": "name is required" }
```

**Response `500`:**
```json
{ "error": "failed to delete skill: ..." }
```

---

## CORS

Enabled only when the `--cors-origin` flag is passed to the server:

```
Access-Control-Allow-Origin: <value of --cors-origin>
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type
```

In development:
```bash
./bin/cloudcode --addr :9090 --cors-origin http://localhost:4000
```

---

## Complete endpoint index

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/instances` | List all instances (syncs Docker status) |
| `POST` | `/api/instances` | Create and start instance |
| `GET` | `/api/instances/{id}` | Get instance (syncs Docker status) |
| `DELETE` | `/api/instances/{id}` | Delete instance + container + volume |
| `POST` | `/api/instances/{id}/start` | Start instance |
| `POST` | `/api/instances/{id}/stop` | Stop instance (blocks up to 30s) |
| `POST` | `/api/instances/{id}/restart` | Restart instance (recreates container) |
| `GET` | `/api/instances/{id}/status?s=` | Poll status (204 if unchanged) |
| `GET` | `/api/system/resources` | Host memory + CPU totals |
| `WS` | `/instances/{id}/logs/ws` | Live log stream (ANSI encoded) |
| `WS` | `/instances/{id}/terminal/ws` | Interactive PTY terminal |
| `GET` | `/api/settings` | Get all settings |
| `PUT` | `/api/settings/env` | Replace all env vars |
| `GET` | `/api/settings/file?path=` | Read config file |
| `PUT` | `/api/settings/file` | Write config file |
| `GET` | `/api/settings/dir-files?dir=` | List managed dir files |
| `PUT` | `/api/settings/dir-file` | Create/update dir file |
| `DELETE` | `/api/settings/dir-file?path=` | Delete dir file |
| `DELETE` | `/api/settings/agents-skill?name=` | Delete agents skill directory |
| `GET` | `/instance/{id}/` | Proxy to OpenCode Web UI (trailing slash required) |
| `GET` | `/` (catch-all) | API 404 / proxy fallback / SPA index |
