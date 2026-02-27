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

### Go 规范

- **Go 1.25**，使用 `net/http` 标准库路由（`mux.HandleFunc("GET /path", handler)`）
- 标准库优先，不引入 web 框架（无 gin/echo/chi）
- import 分组：标准库 → 空行 → 第三方 → 空行 → 本项目（`github.com/naiba/cloudcode/...`）
- 错误处理：`fmt.Errorf("动作: %w", err)` 包装，不丢弃错误（除非显式 `_ =`）
- 不使用 `panic`，入口 `main` 用 `log.Fatalf`，其余返回 error
- 命名：Go 标准风格，unexported 驼峰（`containerName`），exported 大驼峰（`NewManager`）
- 结构体方法接收者用一到两个字母缩写（`m *Manager`、`s *Store`、`h *Handler`、`rp *ReverseProxy`）
- 日志：`log.Printf` / `log.Println`，不用第三方 logger
- 并发：`sync.Mutex` / `sync.RWMutex` 保护共享状态，锁粒度尽量小

### 错误处理模式

```go
result, err := doSomething()
if err != nil {
    return fmt.Errorf("描述: %w", err)
}
```

对于不影响主流程的操作（如状态同步）使用 `_ =` 显式忽略：
```go
_ = h.store.Update(inst)
```

### 数据库

- **modernc.org/sqlite**（纯 Go SQLite，无 CGO）
- 启用 WAL 模式
- JSON 字段用 `TEXT` 存储，Go 侧 `json.Marshal` / `json.Unmarshal`
- 参数化查询，不拼接 SQL

### Docker 集成

- 使用 `github.com/moby/moby/client` 官方 SDK
- 容器命名规则：`cloudcode-{instanceID}`
- 网络：自建 bridge 网络 `cloudcode-net`
- 全局配置通过 bind mount 注入到容器内 `/root/.config/opencode/`、`/root/.opencode/`、`/root/.agents/`
- 每个实例使用 Docker named volume (`cloudcode-home-{id}`) 挂载 `/root`，持久化工作目录（clone 的代码、session 数据等）
- `auth.json` 全局共享，直接 bind mount 到所有实例的 `/root/.local/share/opencode/auth.json`
- Bind mount 子路径优先级高于父路径 volume，全局配置和 auth.json 会覆盖 volume 中的对应路径
- Restart 通过删除容器并重建实现（volume 保留），触发 entrypoint 更新依赖
- 删除实例时通过 `RemoveContainerAndVolume` 同时清理容器和 named volume

### 前端

- **html/template** 服务端渲染，不用前端框架
- **HTMX** 处理交互（`hx-post`、`hx-delete`、`hx-swap`、`HX-Redirect`、`HX-Trigger`）
- **WebSocket** 用于实时日志流和交互式终端，不用 HTTP 轮询
- CSS：自定义变量主题（`var(--bg)`、`var(--primary)` 等），暗色/亮色双主题（`[data-theme="light"]`）
- JS：仅原生 JS，不引入构建工具或 npm 依赖
- 终端页面使用 CDN 加载 xterm.js（`@xterm/xterm`、`@xterm/addon-fit`、`@xterm/addon-web-links`）
- 模板结构：`templates/layouts/base.html`（布局）、`templates/*.html`（页面）、`templates/partials/`（片段）

### WebSocket 规范

- 使用 `github.com/gorilla/websocket` 库
- WebSocket handler 中使用 `r.Context()` 管理生命周期（upgrade 后 context 跟底层连接绑定，用户关闭页面时自动取消）
- 服务端主动关闭时必须先发送 close frame（`websocket.CloseMessage`），避免客户端触发 `onerror`
- 日志流：`/instances/{id}/logs/ws` — Docker logs follow 模式，经 `stdcopy.StdCopy` 解码后推送
- 终端：`/instances/{id}/terminal/ws` — Docker exec TTY，双向桥接浏览器和容器 shell
- 终端 resize 通过 JSON 消息 `{"type":"resize","cols":N,"rows":N}` 传递，服务端调用 `ExecResize`

### CSS 规范

- 使用 CSS 变量（`:root` 中定义），不硬编码颜色值
- class 命名：`kebab-case`（`.config-editor`、`.dir-file-item`）
- 每个区块用 `/* Section Name */` 注释分隔

## 配置管理架构

平台管理的 OpenCode 配置文件和目录：

| 存储位置 | 容器内路径 | 范围 | 内容 |
|---|---|---|---|
| `{dataDir}/config/opencode/` (bind mount) | `/root/.config/opencode/` | 全局 | opencode.jsonc, AGENTS.md, package.json, commands/, agents/, skills/, plugins/ |
| `{dataDir}/config/opencode-data/auth.json` (bind mount) | `/root/.local/share/opencode/auth.json` | 全局 | 认证信息（所有实例共享） |
| `{dataDir}/config/dot-opencode/` (bind mount) | `/root/.opencode/` | 全局 | package.json |
| `{dataDir}/config/agents-skills/` (bind mount) | `/root/.agents/` | 全局 | skills.sh 安装的技能和 lock file |
| `cloudcode-home-{id}` (named volume) | `/root` | 按实例 | 工作目录、clone 的代码、session 数据、数据库等 |

子目录：`commands/`、`agents/`、`skills/`、`plugins/` — 通过 Settings 页面在线管理。

内置 plugin 通过 `//go:embed` 嵌入二进制，每次启动强制写入 `plugins/` 目录（覆盖旧版本）：

#### `_cloudcode-telegram.ts` — 会话通知
- 监听 `session.idle` 和 `session.error` 事件，通过 Telegram Bot API 发送通知
- 读取 `CC_TELEGRAM_BOT_TOKEN` 和 `CC_TELEGRAM_CHAT_ID` 环境变量，未配置则静默跳过

#### `_cloudcode-prompt-watchdog.ts` — System Prompt 监控

通过 `experimental.chat.system.transform` hook 拦截每次 LLM 调用的 system prompt，自动过滤时间行并检测结构变化。所有通知中的内容变化均使用 git unified diff 格式展示。

**核心机制：**
- `analyzeTemporalLine()` 判断每行是否包含日期/时间/时间戳（多组正则按优先级排列）
- 短行（去除时间内容后剩余 ≤ 30 字符）→ 直接删除，Telegram 通知一次（git-diff 格式展示被删除的行及上下文）
- 长行（剩余 > 30 字符）→ 保留不删除，Telegram 简要提示行数
- `temporalLineSignature()` 生成内容签名（去除时间数值后的结构指纹），按 modelID 去重，相同签名只通知一次
- 签名前缀区分类型：`removed:` = 短行删除，`temporal-alert:` = 长行告警
- 小幅 diff 告警：对过滤后的文本做行级 diff，diff < 10 行时替换基线并发送 git unified diff 风格告警（带 `@@` hunk 头和 `-`/`+` 前缀），≥ 10 行视为大变更不替换不告警
- session 结束（`session.idle`）时发送监控总结报告（含最近一次 diff 的 git-diff 格式展示）

**通知格式（统一 git-diff 风格）：**
- 时间行过滤通知：`formatRemovedLinesDiff()` 根据被删除行号在原始文本中生成 diff（带上下文）
- 小幅 diff 告警：`formatUnifiedDiff()` 对比过滤后文本的前后基线生成完整 unified diff
- Report 总结：展示最近一次 diff 的 git-diff 代码块

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
- `formatRemovedLinesDiff` 不能用 `formatUnifiedDiff(rawText, filteredText)` 替代，因为简单的行号对齐 diff 在删除行后会导致下方所有内容错位

**本地测试方法（tmux + opencode TUI）：**

插件状态（`globalPrevFilteredText` 等 Map）存在内存中，只有 TUI 模式能在同一进程内多轮测试。`opencode run` 每次是新进程，插件状态不保留。

核心难点：修改 config 目录下的文件（AGENTS.md、plugins/）会触发 opencode 的 config watcher → bun install 重载 → TUI 重绘，如果 `send-keys` 的文字正在输入，会和 bun install 的 stdout 混在一起污染输入框。

可靠的测试时序：

```bash
# 1. 启动 opencode TUI
tmux new-session -d -s wt -x 200 -y 50
tmux send-keys -t wt 'cd /tmp && CC_WATCHDOG_DEBUG_LOG=/tmp/wt.log opencode' Enter
# 轮询等 MCP 出现在 status bar（TUI 就绪）
for i in $(seq 1 30); do sleep 1; tmux capture-pane -p -t wt | grep -q 'MCP' && break; done

# 2. 发第一条消息（建立基线）
tmux send-keys -t wt 'say 1' Enter
# 轮询 watchdog log 等 Report 出现（session idle）
for i in $(seq 1 60); do sleep 1; grep -q 'Report' /tmp/wt.log && break; done

# 3. 修改 config 触发 prompt 变化
# （编辑 AGENTS.md 等文件）
# 等 bun install 重载完成（轮询 'Checked' 或固定 sleep 5）
sleep 5

# 4. 发第二条消息（触发 diff 告警）
# 先 Escape 清空输入框残留（bun install 输出可能污染输入框）
tmux send-keys -t wt Escape
sleep 2
tmux send-keys -t wt 'say 2' Enter
# 轮询新的 HOOK 或 DIFF 出现
for i in $(seq 1 60); do sleep 1; [ "$(grep -c 'HOOK' /tmp/wt.log)" -gt 2 ] && break; done

# 5. 验证结果
grep -E 'DIFF|Alert' /tmp/wt.log
```

要点：
- 修改文件后必须等 bun install 完成再输入
- bun install 后用 `Escape` 清空输入框残留
- 用 watchdog log 内容轮询（而非固定 sleep）判断就绪状态

## 反向代理架构

OpenCode Web UI 通过 Referer-based routing 方案代理，**不改写**响应内容（无 HTML/CSS/JS 路径重写）。

### 路由策略

1. **入口代理** `/instance/{id}/` — strip prefix 后转发到容器（`ServeHTTP`），同时设置 `_cc_inst` cookie 记录当前实例 ID
2. **Catch-all fallback** `"/"` — 注册在所有平台路由之后，匹配所有未命中的路径
   - 优先从 `Referer` 头提取 `/instance/{id}/` 中的 instance ID
   - Referer 无法提取时，从 `_cc_inst` cookie 获取（覆盖 SPA pushState 跳转后 Referer 丢失的场景）
   - 原始路径直接转发到容器（`ServeHTTPDirect`），不做任何路径修改
   - `httputil.ReverseProxy` 自动处理 WebSocket 升级（`Upgrade: websocket`）、SSE 等
3. **无 Referer 且无 cookie** 的请求返回 404（不属于任何实例）

### 工作原理

浏览器访问 `/instance/{id}/` → 设置 `_cc_inst` cookie → 容器返回 SPA HTML（资源路径为 `/assets/xxx.js`）  
→ 浏览器请求 `/assets/xxx.js`，带 `Referer: http://host/instance/{id}/`  
→ catch-all handler 从 Referer 提取 ID → 直接代理到容器 → 容器正常响应

SPA 内部通过 `history.pushState` 跳转到 `/L3Jvb3QvY2xvdWRjb2Rl/session` 等路径后：  
→ 后续请求的 Referer 不再包含 `/instance/{id}/`  
→ catch-all handler 从 `_cc_inst` cookie 获取 instance ID → 代理到容器

### 注意事项

- `httputil.ReverseProxy` 原生支持 WebSocket：当请求包含 `Upgrade` 头时自动进行协议升级并双向桥接
- OpenCode SDK 的 API 请求（`/global/`、`/path`、`/project` 等）和静态资源（`/assets/`）都通过同一个 catch-all 机制处理
- 容器内 OpenCode 使用 `window.location.origin` 拼接 API URL，指向平台根路径，因此都会被 catch-all 捕获
- cookie 是全局的（`Path=/`），同时只能有一个活跃的 Web UI 实例，打开新实例会覆盖旧的 cookie

## 关键约束

- 所有实例共享全局配置，容器内修改会影响所有实例（bind mount 读写）
- 端口池范围 10000-10100，每个实例分配一个
- 容器资源限制：2GB 内存、2 CPU
- base 镜像基于 Ubuntu 24.04，包含 Go 1.23、Node 22、Bun
- `oh-my-opencode` 通过 `bun install -g` 全局安装（非 git clone）

## 修改代码时注意

- 改 Go 代码后运行 `go vet ./...` 和 `go build ./...`
- 改 Dockerfile 后本地构建验证：`docker build -t cloudcode-base:latest -f docker/Dockerfile docker/`
- 改模板/CSS/JS 不需要编译，但需要重启服务（模板在启动时加载）
- handler 新增路由时在 `RegisterRoutes` 方法中按已有格式添加
- 新增配置文件管理时更新 `config.go` 的相关切片和 `EditableFiles()`
