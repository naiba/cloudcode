# AGENTS.md — CloudCode

- 使用中文回复，思考过程也用中文展示。
- 优先使用bun而不是npm。

## 项目概述

CloudCode 是一个 OpenCode 实例管理平台。Go 后端 + Docker 容器编排 + HTMX 前端。
每个实例是一个运行 `opencode web` 的 Docker 容器，平台通过反向代理暴露 Web UI。

## 构建与运行

```bash
go run . --no-docker --addr :8080          # 开发模式
go run . --addr :8080 --image cloudcode-base:latest  # 需要 Docker
docker build -t cloudcode-base:latest -f docker/Dockerfile docker/
```

## 检查与验证

```bash
go vet ./...
go build ./...
```

## 非显而易见的设计决策

### Docker 容器

- Bind mount 子路径优先级高于父路径 volume，全局配置和 auth.json 会覆盖 volume 中的对应路径
- Restart 通过删除容器并重建实现（非 `docker restart`），volume 保留，触发 entrypoint 更新依赖
- 删除实例时通过 `RemoveContainerAndVolume` 同时清理容器和 named volume

### WebSocket

- 服务端主动关闭时必须先发送 close frame（`websocket.CloseMessage`），避免客户端触发 `onerror`
- 终端 resize 通过 JSON 消息 `{"type":"resize","cols":N,"rows":N}` 传递，服务端调用 `ExecResize`

### 反向代理

Referer-based routing，**不改写**响应内容（无 HTML/CSS/JS 路径重写）。

1. **入口代理** `/instance/{id}/` — strip prefix 后转发，设置 `_cc_inst` cookie 记录实例 ID
2. **Catch-all fallback** `"/"` — 注册在所有平台路由之后
   - 优先从 `Referer` 提取 `/instance/{id}/` 中的 ID
   - 回退到 `_cc_inst` cookie（覆盖 SPA pushState 后 Referer 丢失的场景）
   - 原始路径直接转发，不修改
3. **无 Referer 且无 cookie** → 404

cookie 是全局的（`Path=/`），同时只能有一个活跃的 Web UI 实例，打开新实例会覆盖旧的 cookie。

### 浏览器自动化

- Chromium 由 Playwright 安装，pinchtab server 在 entrypoint.sh 中以 headless + stealth 模式后台启动
- `config.go` 的 `ensurePinchtabMCP()` 在启动时将 pinchtab MCP 注入 opencode.jsonc
- 容器内工具使用说明通过 `_cloudcode-instructions.md` 注入，启动时自动写入

### `_cloudcode-prompt-watchdog.ts` — System Prompt 监控

通过 `experimental.chat.system.transform` hook 拦截 system prompt，自动过滤时间行并检测结构变化。通知使用 git unified diff 格式。

**核心机制：**
- `analyzeTemporalLine()` 判断每行是否包含日期/时间/时间戳（多组正则按优先级排列）
- 短行（去除时间内容后剩余 ≤ 30 字符）→ 直接删除，Telegram 通知（git-diff 格式）
- 长行（剩余 > 30 字符）→ 保留不删除，代码块展示行号和截断内容
- `temporalLineSignature()` 生成内容签名（去除时间数值后的结构指纹），按 modelID 去重
- 小幅 diff（< 10 行）替换基线并发送 unified diff 告警，≥ 10 行视为大变更不替换
- session 结束时发送监控总结报告

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

## 修改代码时注意

- handler 新增路由时在 `RegisterRoutes` 方法中按已有格式添加
- 新增配置文件管理时更新 `config.go` 的相关切片和 `EditableFiles()`
