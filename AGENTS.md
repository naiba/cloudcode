# AGENTS.md — CloudCode

- 使用中文回复，思考过程也用中文展示。
- 优先使用bun而不是npm。

## 项目概述

CloudCode 是一个 OpenCode 实例管理平台。Go 后端 + Docker 容器编排 + HTMX 前端。
每个实例是一个运行 `opencode web` 的 Docker 容器，平台通过反向代理暴露 Web UI。

### 架构

```
main.go                     入口，加载模板，启动 HTTP server
internal/
  config/config.go          配置文件管理（读写宿主机配置，生成容器 bind mount）
  config/plugins/            embed 的内置 plugin（如 _cloudcode-telegram.ts），启动时写入 plugins/
  docker/manager.go         Docker 容器生命周期（创建/启动/停止/删除）
  handler/handler.go        所有 HTTP handler（页面渲染 + HTMX API）
  proxy/proxy.go            动态反向代理到各实例的 opencode web UI
  store/store.go            SQLite 持久化（实例 CRUD）
docker/
  Dockerfile                base 镜像（Ubuntu + Go + Node + Bun + OpenCode）
  entrypoint.sh             容器启动脚本（更新依赖 + 启动 opencode web）
Dockerfile.platform         平台自身的多阶段构建镜像
templates/                  Go html/template 模板（HTMX）
static/                     CSS + JS
```

## 构建与运行

```bash
# 构建
go build -o bin/cloudcode .

# 运行（开发模式，跳过 Docker）
go run . --no-docker --addr :8080

# 运行（需要 Docker）
go run . --addr :8080 --image cloudcode-base:latest

# 构建 base 镜像
docker build -t cloudcode-base:latest -f docker/Dockerfile docker/

# 构建平台镜像
docker build -t cloudcode:latest -f Dockerfile.platform .
```

## 检查与验证

```bash
# 静态分析（CI 中运行）
go vet ./...

# 编译检查
go build ./...

# 目前无测试文件，未来添加后使用：
# go test ./...
# go test -run TestXxx ./internal/store/
```

## 代码风格

- **Go 1.25**，使用 `net/http` 标准库路由（`mux.HandleFunc("GET /path", handler)`），不引入 web 框架
- **modernc.org/sqlite**（纯 Go，无 CGO），JSON 字段用 `TEXT` 存储
- **html/template** + HTMX，无前端框架。JS 仅原生 JS，无构建工具
- 终端页面使用 CDN 加载 xterm.js（`@xterm/xterm`、`@xterm/addon-fit`、`@xterm/addon-web-links`）
- CSS 自定义变量主题，暗色/亮色双主题（`[data-theme="light"]`）

### Docker 集成

- 使用 `github.com/moby/moby/client` 官方 SDK
- 容器命名规则：`cloudcode-{instanceID}`
- 网络：自建 bridge 网络 `cloudcode-net`
- 全局配置通过 bind mount 注入容器，每个实例使用 named volume (`cloudcode-home-{id}`) 挂载 `/root`
- Bind mount 子路径优先级高于父路径 volume，全局配置和 auth.json 会覆盖 volume 中的对应路径
- Restart 通过删除容器并重建实现（volume 保留），触发 entrypoint 更新依赖
- 删除实例时通过 `RemoveContainerAndVolume` 同时清理容器和 named volume

### WebSocket

- 使用 `github.com/gorilla/websocket`
- handler 中用 `r.Context()` 管理生命周期（upgrade 后 context 跟底层连接绑定，用户关闭页面时自动取消）
- 服务端主动关闭时必须先发送 close frame（`websocket.CloseMessage`），避免客户端触发 `onerror`
- 日志流：`/instances/{id}/logs/ws` — Docker logs follow，经 `stdcopy.StdCopy` 解码
- 终端：`/instances/{id}/terminal/ws` — Docker exec TTY，双向桥接
- 终端 resize 通过 JSON 消息 `{"type":"resize","cols":N,"rows":N}` 传递，服务端调用 `ExecResize`

## 配置管理架构

| 存储位置 | 容器内路径 | 范围 | 内容 |
|---|---|---|---|
| `{dataDir}/config/opencode/` (bind mount) | `/root/.config/opencode/` | 全局 | opencode.jsonc, AGENTS.md, package.json, commands/, agents/, skills/, plugins/ |
| `{dataDir}/config/opencode-data/auth.json` (bind mount) | `/root/.local/share/opencode/auth.json` | 全局 | 认证信息（所有实例共享） |
| `{dataDir}/config/dot-opencode/` (bind mount) | `/root/.opencode/` | 全局 | package.json |
| `{dataDir}/config/agents-skills/` (bind mount) | `/root/.agents/` | 全局 | skills.sh 安装的技能和 lock file |
| `cloudcode-home-{id}` (named volume) | `/root` | 按实例 | 工作目录、clone 的代码、session 数据、数据库等 |

子目录 `commands/`、`agents/`、`skills/`、`plugins/` 通过 Settings 页面在线管理。

内置 plugin 通过 `//go:embed` 嵌入二进制，每次启动强制写入 `plugins/` 目录（覆盖旧版本）：

#### `_cloudcode-telegram.ts` — 会话通知
- 监听 `session.idle` 和 `session.error` 事件，通过 Telegram Bot API 发送通知
- 读取 `CC_TELEGRAM_BOT_TOKEN` 和 `CC_TELEGRAM_CHAT_ID` 环境变量，未配置则静默跳过

#### `_cloudcode-prompt-watchdog.ts` — System Prompt 监控

通过 `experimental.chat.system.transform` hook 拦截 system prompt，自动过滤时间行并检测结构变化。通知使用 git unified diff 格式。

**核心机制：**
- `analyzeTemporalLine()` 判断每行是否包含日期/时间/时间戳（多组正则按优先级排列）
- 短行（去除时间内容后剩余 ≤ 30 字符）→ 直接删除，Telegram 通知（git-diff 格式）
- 长行（剩余 > 30 字符）→ 保留不删除，代码块展示行号和截断内容
- `temporalLineSignature()` 生成内容签名（去除时间数值后的结构指纹），按 modelID 去重
- 小幅 diff（< 10 行）替换基线并发送 unified diff 告警，≥ 10 行视为大变更不替换
- session 结束时发送监控总结报告

**环境变量：**
- `CC_TELEGRAM_BOT_TOKEN` / `CC_TELEGRAM_CHAT_ID`：Telegram 通知（必需）
- `CC_PROMPT_WATCHDOG_DISABLED`：设为 `"true"` 禁用
- `CC_WATCHDOG_DEBUG_LOG`：设为文件路径开启调试日志（如 `/tmp/watchdog.log`）
- `CC_INSTANCE_NAME`：通知中显示的实例标识

**已知陷阱（修改时务必注意）：**
- 正则顺序：长模式（ISO datetime）必须在短模式（date、time）之前，否则短模式先匹配局部导致长模式失效
- 带 `/g` 的正则在 `test`/`exec` 后 `lastIndex` 不会自动重置，每次使用前必须手动 `pattern.lastIndex = 0`
- 月份正则不能用 `\b(Mar)\w*` 形式（会误匹配 Marking/Market），必须用精确匹配 `\b(March|Mar)\b`
- May 与英文助动词同形，已从正则中移除（接受 May 日期不被检测的代价）
- 行号会因 prompt 上方内容增删而漂移，因此用内容签名（而非行号）判断是否已通知
- `system: string[]` 实际是单元素数组（一个大字符串），不同 agent（如 title agent claude-haiku vs 主 agent claude-opus）会分别触发 hook
- hook 入口必须 try-catch 包裹，避免 plugin bug 导致 opencode 崩溃
- `formatRemovedLinesDiff` 不能用 `formatUnifiedDiff(rawText, filteredText)` 替代，因为删除行后会导致下方所有内容错位

**本地测试方法（tmux + opencode TUI）：**

插件状态存在内存中，只有 TUI 模式能在同一进程内多轮测试。`opencode run` 每次是新进程，状态不保留。

核心难点：修改 config 目录下的文件会触发 opencode 的 config watcher → bun install 重载 → TUI 重绘，`send-keys` 的文字会和 bun install 的 stdout 混在一起污染输入框。

```bash
# 1. 启动 opencode TUI
tmux new-session -d -s wt -x 200 -y 50
tmux send-keys -t wt 'cd /tmp && CC_WATCHDOG_DEBUG_LOG=/tmp/wt.log opencode' Enter
for i in $(seq 1 30); do sleep 1; tmux capture-pane -p -t wt | grep -q 'MCP' && break; done

# 2. 发第一条消息（建立基线）
tmux send-keys -t wt 'say 1' Enter
for i in $(seq 1 60); do sleep 1; grep -q 'Report' /tmp/wt.log && break; done

# 3. 修改 config 触发 prompt 变化，等 bun install 完成
sleep 5

# 4. 发第二条消息（触发 diff 告警）
tmux send-keys -t wt Escape  # 清空输入框残留
sleep 2
tmux send-keys -t wt 'say 2' Enter
for i in $(seq 1 60); do sleep 1; [ "$(grep -c 'HOOK' /tmp/wt.log)" -gt 2 ] && break; done

# 5. 验证
grep -E 'DIFF|Alert' /tmp/wt.log
```

要点：修改文件后必须等 bun install 完成再输入；bun install 后用 `Escape` 清空输入框残留；用 log 内容轮询判断就绪状态。

## 反向代理架构

Referer-based routing，**不改写**响应内容（无 HTML/CSS/JS 路径重写）。

1. **入口代理** `/instance/{id}/` — strip prefix 后转发，设置 `_cc_inst` cookie 记录实例 ID
2. **Catch-all fallback** `"/"` — 注册在所有平台路由之后
   - 优先从 `Referer` 提取 `/instance/{id}/` 中的 ID
   - 回退到 `_cc_inst` cookie（覆盖 SPA pushState 后 Referer 丢失的场景）
   - 原始路径直接转发，不修改
3. **无 Referer 且无 cookie** → 404

cookie 是全局的（`Path=/`），同时只能有一个活跃的 Web UI 实例，打开新实例会覆盖旧的 cookie。

## 关键约束

- 所有实例共享全局配置，容器内修改会影响所有实例（bind mount 读写）
- 端口池范围 10000-10100，每个实例分配一个
- 容器资源限制：创建时可配置内存（MB）和 CPU（核数），0 表示不限制，默认 2GB/2核
- base 镜像基于 Ubuntu 24.04，包含 Go 1.23、Node 22、Bun
- `oh-my-opencode` 通过 `bun install -g` 全局安装（非 git clone）
- 容器内预装 `cloudflared`，可通过 Cloudflare Tunnel 将容器内服务暴露到公网
- 容器内 cloudflared 使用说明通过 `_cloudcode-instructions.md` 注入，启动时自动写入

## 修改代码时注意

- 改 Go 代码后运行 `go vet ./...` 和 `go build ./...`
- 改 Dockerfile 后本地构建验证：`docker build -t cloudcode-base:latest -f docker/Dockerfile docker/`
- 改模板/CSS/JS 不需要编译，但需要重启服务（模板在启动时加载）
- handler 新增路由时在 `RegisterRoutes` 方法中按已有格式添加
- 新增配置文件管理时更新 `config.go` 的相关切片和 `EditableFiles()`
