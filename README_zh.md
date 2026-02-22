# CloudCode

一个自托管的 [OpenCode](https://opencode.ai) 实例管理平台。将多个隔离的 OpenCode 环境作为 Docker 容器运行，并通过统一的 Web 面板管理。

[English](README.md)

## 功能特性

- **多实例管理** — 创建、启动、停止、重启、删除 OpenCode 实例
- **Session 隔离** — 每个实例拥有独立的 session 数据，认证令牌全局共享
- **共享全局配置** — 在 Settings 页面统一管理 `opencode.jsonc`、`AGENTS.md`、认证令牌、自定义命令、Agent、Skills 和 Plugins
- **skills.sh 集成** — 在任意容器内通过 `bunx skills add` 安装 [skills.sh](https://skills.sh) 技能，所有实例共享
- **Telegram 通知** — 内置插件在任务完成/报错时发送 Telegram 消息（配置 `CC_TELEGRAM_BOT_TOKEN` 和 `CC_TELEGRAM_CHAT_ID`）
- **暗色/亮色主题** — 跟随系统偏好，支持手动切换，localStorage 记忆
- **反向代理** — 通过 `/instance/{id}/` 路径访问每个实例的 Web UI
- **容器自动更新** — base 镜像包含 OpenCode + Oh My OpenCode，每次启动时自动更新

## 快速开始

### Docker Compose（推荐）

```bash
mkdir cloudcode && cd cloudcode
curl -O https://raw.githubusercontent.com/naiba/cloudcode/main/docker-compose.yml
docker compose up -d
```

浏览器打开 http://localhost:8080。

镜像从 `ghcr.io/naiba/cloudcode` 和 `ghcr.io/naiba/cloudcode-base` 自动拉取。

## 架构

```
浏览器 → CloudCode 平台 (Go + HTMX)
              ├── 面板           — 实例列表与管理
              ├── 设置           — 全局配置编辑器
              └── /instance/{id}/ — 反向代理 → 容器端口
                                        │
                            ┌────────────┼────────────┐
                            ▼            ▼            ▼
                         容器 1       容器 2       容器 N
                       (opencode    (opencode    (opencode
                        web :10000)  web :10001)  web :10002)
```

每个容器运行 `opencode web`，通过平台的反向代理访问。

## 配置管理

全局配置通过 Settings 页面管理，并挂载到所有容器中：

| 存储位置 | 容器内路径 | 范围 | 内容 |
|---|---|---|---|
| `data/config/opencode/` (bind mount) | `/root/.config/opencode/` | 全局 | `opencode.jsonc`、`AGENTS.md`、`package.json`、commands/、agents/、skills/、plugins/ |
| `data/config/opencode-data/auth.json` (bind mount) | `/root/.local/share/opencode/auth.json` | 全局 | 认证信息（所有实例共享） |
| `data/config/dot-opencode/` (bind mount) | `/root/.opencode/` | 全局 | `package.json` |
| `data/config/agents-skills/` (bind mount) | `/root/.agents/skills/` | 全局 | 通过 [skills.sh](https://skills.sh) 安装的技能 |
| `cloudcode-home-{id}` (named volume) | `/root` | 按实例 | 工作目录、clone 的代码、session 数据、数据库等 |

环境变量（如 `ANTHROPIC_API_KEY`、`GH_TOKEN`）在 Settings 中配置，自动注入所有容器。

### Telegram 通知

在 Settings 中设置以下环境变量即可接收通知：

- `CC_TELEGRAM_BOT_TOKEN` — Telegram Bot API Token
- `CC_TELEGRAM_CHAT_ID` — 目标聊天/群组 ID

内置插件监听 `session.idle`（任务完成）和 `session.error`（会话报错）事件。

## 技术栈

- **后端**：Go 1.25，`net/http` 标准库路由，SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）
- **前端**：`html/template` + HTMX，原生 CSS/JS，暗色/亮色主题
- **容器**：Docker SDK（`github.com/moby/moby/client`）
- **base 镜像**：Ubuntu 24.04 + Go + Node 22 + Bun + OpenCode + Oh My OpenCode

## 开发

```bash
# 开发模式运行（无需 Docker）
go run . --no-docker --addr :8080

# 静态分析
go vet ./...

# 编译检查
go build ./...
```

