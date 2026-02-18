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
- 全局配置通过 bind mount 注入到容器内 `/root/.config/opencode/`、`/root/.local/share/opencode/`、`/root/.opencode/`

### 前端

- **html/template** 服务端渲染，不用前端框架
- **HTMX** 处理交互（`hx-post`、`hx-delete`、`hx-swap`、`HX-Redirect`、`HX-Trigger`）
- CSS：自定义变量主题（`var(--bg)`、`var(--primary)` 等），暗色系
- JS：仅原生 JS，不引入构建工具或 npm 依赖
- 模板结构：`templates/layouts/base.html`（布局）、`templates/*.html`（页面）、`templates/partials/`（片段）

### CSS 规范

- 使用 CSS 变量（`:root` 中定义），不硬编码颜色值
- class 命名：`kebab-case`（`.config-editor`、`.dir-file-item`）
- 每个区块用 `/* Section Name */` 注释分隔

## 配置管理架构

平台管理的 OpenCode 配置文件和目录：

| 宿主机路径 | 容器内路径 | 内容 |
|---|---|---|
| `{dataDir}/config/opencode/` | `/root/.config/opencode/` | opencode.json, AGENTS.md, package.json 等 |
| `{dataDir}/config/opencode-data/` | `/root/.local/share/opencode/` | auth.json |
| `{dataDir}/config/dot-opencode/` | `/root/.opencode/` | package.json |

子目录：`commands/`、`agents/`、`skills/`、`plugins/` — 通过 Settings 页面在线管理。

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
