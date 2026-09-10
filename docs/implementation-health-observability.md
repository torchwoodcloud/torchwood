# Torchwood Health & 可观测性实现方案

> 状态：**已实现**（2026-08-09 验收通过：4 项任务全部落地，含真实 server 实测
> 版本端点/健康明细/readiness 503；7 项实现偏差经裁决全部接受）
> 目标读者：维护者与后续扩展
> 关联：`docs/roadmap.md` §2.7（Health & 可观测性）、`AGENTS.md`（开发约定，必读）
> 参考：`docs/implementation-account-completion.md`（上一轮同类方案：先审查、后实现、再汇报）
> 修订记录：2026-08-09 v2（独立评审修订：WithHealthCheckers 签名与双端注册、ObjectStore 加 Ping、
> hook 挂载点修正、request log Debug 级别、BuildInfo 包位置、slow query 默认值语义等）

---

## 1. 目标与验收标准

落地 roadmap §2.7 四项任务：**健康检查（DB/Redis/Storage 探测）、版本端点、
结构化日志（HTTP 请求日志 + 统一 logger）、慢查询日志**。

**验收标准**：

1. `GET /v1/health` 返回依赖明细：`{"status":"ok"|"unavailable","dependencies":[{name,status,error?}]}`
   —— Postgres/Redis/MinIO 任一探测失败时该依赖 `status=unavailable` + error，
   整体 `status=unavailable`；探测各有超时（默认 2s），并行执行，失败不影响其他依赖。
2. lynx 自带的 `/healthz/readiness` 走 checkers：全部健康 200 / 任一失败 503；
   `/healthz/liveness` 恒 200。**必须同时在 gRPC 与 gateway HTTP 两侧注册 checkers**
   （gRPC 侧同步 `grpc.health.v1.Health`；HTTP 侧驱动 readiness，见 §3.1 阻断修正）。
3. `GET /v1/server/health/version` 返回 `{"version":"...","commit":"...","date":"..."}`；
   Taskfile ldflags 注入生效（`-X main.version=` 变量名匹配修复，全小写）。
4. HTTP gateway 请求日志启用（**Debug 级别**，需 `--log-level debug` 可见）；
   `file_handler.go`、`functions_handler.go`、`worker.go`、`cors.go` 的零散日志统一走
   注入的 app logger（不再 `slog.Default()` / `log.Printf`）；JWT 拦截器复用现成
   `WithLogger` 链式方法。
5. 慢查询日志：**空配置默认 500ms**、显式 `"0"` 禁用、显式值生效；超阈值 SQL 输出
   结构化 Warn（operation/query/duration）；`data.database.debug=true` 时额外记录
   全量 SQL（Debug 级别）。
6. `go test ./...`、`task lint`、`task build` 全绿；新增测试覆盖 checkers、dbhook、
   health handler、readiness 503（真实监听）。

---

## 2. 现状盘点（调研结论）

| 任务 | 已有资产 | 缺口 |
|---|---|---|
| 健康检查 | `GET /v1/health` RPC+gateway（ACCESS_PUBLIC）；启动期一次性 ping（database.go:88 PG / :36 Redis）；lynx `Checker` 接口（`CheckHealth() error`，无 ctx）；lynxhttp 自动 `/healthz/liveness\|readiness`（server/http/server.go:224-226）；MinIO `client.BucketExists` 内部可用 | handler 硬编码 ok（health.go:17-19）；gRPC/HTTP 两侧均未注册 checkers；响应无依赖明细；MinIO 端口无探测方法 |
| 版本端点 | `cmd/server/main.go:14 var version string`（小写）；`lynx.WithVersion`（仅日志字段） | 无 RPC/路由；Taskfile ldflags `-X main.Version=` 与 `var version` 大小写不匹配（**注入失效**） |
| 结构化日志 | gRPC 请求日志拦截器已生效（Info 级）；JWT 拦截器已有 `WithLogger(l)` 链式方法（jwt.go:56-61）；全链路 `*slog.Logger`（zap 后端） | gateway 未启用 `WithRequestLog`；file_handler.go:63 / functions_handler.go:47 / worker.go:39 用 `slog.Default()`；cors.go:19 stdlib `log.Printf` |
| 慢查询日志 | bun v1.2.18 `AddQueryHook`/`QueryHook`/`QueryEvent`（StartTime/Query/Args/Err；`Operation()` 是**方法**）；`newDatabase`（database.go:58）建库，`database.go:92-94` TODO 挂载点 | 无 hook 实现；无阈值配置；`debug` 配置为死配置（从未接线） |

---

## 3. 分层实现规格

### 3.1 健康检查

**proto**（`proto/server/v1/health.proto` 扩展；命名遵循 buf lint 约定）：

```proto
message DependencyStatus {
  string name = 1;        // postgres / redis / minio
  string status = 2;      // ok / unavailable
  string error = 3;       // 仅 unavailable 时非空
}
message HealthCheckResponse {
  string status = 1;                 // ok / unavailable（沿用现有字段，向后兼容）
  repeated DependencyStatus dependencies = 2;
}

rpc Check(HealthCheckRequest) returns (HealthCheckResponse) {
  option (google.api.http) = { get: "/v1/health" };
  option (google.api.http) = { get: "/v1/server/health" };   // roadmap 路径别名
}
rpc GetVersion(GetVersionRequest) returns (GetVersionResponse) {
  option (google.api.http) = { get: "/v1/server/health/version" };
}
message GetVersionRequest {}
message GetVersionResponse { string version = 1; string commit = 2; string date = 3; }
```

（服务级 `ACCESS_PUBLIC` 已存在，新 RPC 自动继承；同一 RPC 多 http 注解是
grpc-gateway 标准能力。）

**checkers**（新建 `internal/infra/health/checks.go`）：

```go
package health

// DependencyChecker 单依赖探测；实现 lynx.Checker（CheckHealth() error，无 ctx，
// 超时必须在内部自控）。
type DependencyChecker struct {
    Name    string
    Timeout time.Duration // 默认 2s
    Check   func(ctx context.Context) error
}

func (c *DependencyChecker) CheckHealth() error {
    ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
    defer cancel()
    return c.Check(ctx)
}

// Checkers 是依赖集合（只读，并发安全）。
type Checkers struct { deps []*DependencyChecker }

// Deps 返回全部依赖（供 lynx HealthCheckersFunc 使用）。
func (c *Checkers) Deps() []lynx.Checker
// Details 并行探测各依赖（各自超时 + recover 兜底），返回逐依赖状态。
func (c *Checkers) Details(ctx context.Context) []DependencyStatus
```

**探测原语**（评审修正——类型断言路径不可行，必须扩展端口）：

- Postgres：`db.PingContext(ctx)`（`*clients.Database` 内嵌 bun.DB）。
- Redis：`rdb.Ping(ctx).Err()`。
- **MinIO：给 `internal/domain/storage/object.go` 的 `ObjectStore` 端口新增
  `Ping(ctx context.Context) error`**：
  - `minioObjectStore` 实现：`_, err := m.client.BucketExists(ctx, m.bucket); return err`；
  - `testutil.MemObjectStore` 实现：返回 nil；
  - 删除方案 v1 的类型断言路径（minioObjectStore 未导出，断言必然失败）。

**handler**（`internal/api/servergrpc/health.go` 改造）：

- `NewHealthService(checkers *health.Checkers, info buildinfo.BuildInfo)`（注入，Wire 自动）。
- `Check`：`checkers.Details(ctx)`（并行，各自 2s 超时）→ 任一失败整体
  `status=unavailable`。gRPC 返回码保持 OK（200）——服务存活，body 表达依赖状态；
  **503 语义由 `/healthz/readiness` 承担**。进程级故障（gRPC 后端不可达时 gateway
  转发失败）会经 HTTPErrorHandler 返回 503 错误体——这是故障路径，与日常语义不同，
  属预期行为。
- `GetVersion`：返回注入的 BuildInfo。

**挂载（评审修正——必须双端注册）**：

- `internal/infra/server/grpc.go`：`NewGRPCServer` 增加 `checkers *health.Checkers`
  形参 + `lynxgrpc.WithHealthCheckers(func() []lynx.Checker { return checkers.Deps() })`
  （**签名是 `lynx.HealthCheckersFunc`，即 `func() []Checker`，不是切片**）→ 同步
  `grpc.health.v1.Health`（10s 轮询快照，gRPC 协议语义）。
- `internal/infra/server/grpc_gateway.go`：`lynxhttp.NewServer` 追加
  `lynxhttp.WithHealthCheckers(func() []lynx.Checker { return checkers.Deps() })`
  （**这是 `/healthz/readiness` 503 的唯一实现路径**，grpc 侧对 HTTP 端点无效）+
  `lynxhttp.WithLogger(app.Logger())` + `lynxhttp.WithRequestLog(true)`（见 §3.3）。
- `internal/api/provides.go` / `internal/infra/provides.go`：注册 `health.NewCheckers`、
  `servergrpc.NewHealthService`；`cmd/server/provides.go` 提供 `buildinfo` provider；
  `task wire-all`。

### 3.2 版本端点与 ldflags

- **BuildInfo 类型必须放 `pkg/buildinfo/buildinfo.go`**（评审修正：cmd/server
  是 package main，api 层无法 import 其类型）：

```go
package buildinfo

type BuildInfo struct {
    Version string
    Commit  string
    Date    string
}
```

- `cmd/server/main.go`：`var version, commit, date string`（全小写）。
- `cmd/server/provides.go`：零参 provider（直接读 package 级 var，wire 无歧义）：

```go
func NewBuildInfo() buildinfo.BuildInfo {
    return buildinfo.BuildInfo{Version: version, Commit: commit, Date: date}
}
```

- **修正** `Taskfile.yml` build 的 ldflags（现 :123-124 的 `main.Version` 无效）：

```yaml
VERSION:
  sh: git describe --tags --always
COMMIT:
  sh: git rev-parse --short HEAD
DATE:
  sh: '{{if eq .OS "Windows_NT"}}powershell -NoProfile -Command Get-Date -Format "yyyyMMddHHmmss"{{else}}date +%Y%m%d%H%M%S{{end}}'
# cmds: - go build -ldflags "-X main.version={{.VERSION}} -X main.commit={{.COMMIT}} -X main.date={{.DATE}}" ...
```

  （DATE 用无空格格式 `yyyyMMddHHmmss`，避免 `-X` 值含空格；Windows/Linux 双分支
  参照 build-docker 的 TIMESTAMP 模式。worker 的 build 命令同样修复。）
- worker 不需要版本端点，`NewBuildInfo` 只在 server 图（wire 剪枝自动处理）。

### 3.3 结构化日志

- **gateway 请求日志**（`internal/infra/server/grpc_gateway.go`）：
  `lynxhttp.WithRequestLog(true)` + `lynxhttp.WithLogger(app.Logger())`（两者都要传，
  前者开关、后者补 service.* 字段）。
  **注意：lynx 请求日志输出级别是 Debug**（lynx@v1.2.0 requestlog.go:262），默认
  info 级别不可见——验收以「启用开关 + `--log-level debug` 时可见」为准，与 gRPC 侧
  Info 级拦截器不对称属框架现状。
- **统一 logger 注入**：
  - `file_handler.go:63`、`functions_handler.go:47`：构造函数增加 `logger *slog.Logger`
    参数，删除 `slog.Default()`。
  - **`worker/worker.go:39`**：`NewWorker` 增加 logger 参数（评审补充——方案 v1 遗漏）。
  - `pkg/grpc/interceptor/jwt.go:41`：**复用现成 `WithLogger(l)` 链式方法**
    （jwt.go:56-61），在 `internal/infra/server/grpc.go:71` 处
    `.WithLogger(app.Logger())`——不改签名、不动 wire。
  - `internal/infra/server/cors.go:19`：`log.Printf` → 注入的 `*slog.Logger`；
    **调用点两处**：`grpc_gateway.go:95`（生产）、`observability_acceptance_test.go:42`
    （测试，需同步改）。
- 上述构造函数签名变化触发 wire 重生成（`task wire-all`）。
- 已知噪音（可接受，文档注明）：请求日志启用后 preflight OPTIONS 与 SPA 静态资源
  也会打 Debug 日志；lynx RequestURL 含完整 query string（含 OAuth code 等敏感参数），
  生产启用 debug 前需知悉。

### 3.4 慢查询日志

**配置**（`internal/pkg/config/config.proto` 的 `Database` 增加字段 + 模板）：

```proto
message Database {
  // ...现有 source=1/debug=2/pool=3...
  // 慢查询日志阈值（如 "500ms"）；空字符串 = 默认 500ms；"0" = 禁用。
  string slow_query_threshold = 4;
}
```

- `configs/config.yaml.template`：`data.database.slow_query_threshold: "500ms"`；
  环境变量 `TORCHWOOD_DATA_DATABASE_SLOW_QUERY_THRESHOLD` 自动生效（bind 反射自动收集）。
- `task generate-config`。

**hook**（新建 `internal/infra/clients/dbhook.go`）：

```go
// SlowQueryHook 记录超过阈值的 SQL；LogAll 时记录全部 SQL（debug 模式）。
type SlowQueryHook struct {
    Threshold time.Duration
    LogAll    bool
    Logger    *slog.Logger
}

func (h *SlowQueryHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context { return ctx }
func (h *SlowQueryHook) AfterQuery(ctx context.Context, e *bun.QueryEvent) {
    if h.LogAll {
        h.Logger.Debug("sql", slog.String("operation", e.Operation()),
            slog.String("query", e.Query), slog.Duration("duration", time.Since(e.StartTime)))
        return
    }
    if h.Threshold <= 0 { return }
    if d := time.Since(e.StartTime); d >= h.Threshold {
        h.Logger.Warn("slow query", slog.String("operation", e.Operation()),
            slog.String("query", e.Query), slog.Duration("duration", d),
            slog.String("error", errorString(e.Err)))
    }
}
```

- **`e.Operation` 是方法不是字段**：必须写 `e.Operation()`；`e.Query` 是含内联参数的
  格式化 SQL（可能含 PII，文档注明）。
- **挂载点（评审修正）**：`NewDatabase(dataClients)` 是 wire 访问器（无 config）——
  hook 必须挂在 `newDatabase(cfg *config.Database)`（database.go:58，由
  `NewDataClients(cfg, logger)` 内部调用）内、`bun.NewDB` 之后：
  `db.AddQueryHook(&SlowQueryHook{...})`。**testutil.SetupTestDB 不经过 NewDataClients，
  不受影响**；受影响的只有 wire_gen.go 的 server/worker 调用点（`task wire-all` 重生成）。
- 语义定案：
  - 阈值：`""` → 500ms；`"0"` → 禁用；其他 → `time.ParseDuration`（解析失败 Warn 并禁用）。
  - `debug=true` → `LogAll=true`（全量 SQL Debug 日志，评审补充——v1 未定义 debug 行为）；
    `debug=false` → 仅慢查询。

---

## 4. 实现顺序（建议）

| 步骤 | 内容 | 验证 |
|------|------|------|
| 1 | proto 扩展 + `task generate-proto` | 编译通过 |
| 2 | config.proto 加 slow_query_threshold + `task generate-config` + 模板 | 编译通过 |
| 3 | domain：`ObjectStore` 加 Ping（minio + memstore 实现） | `go build ./...` |
| 4 | infra：health/checks.go、clients/dbhook.go、newDatabase/NewDataClients 改造 | infra 单测 |
| 5 | handler：health.go（并行探测/version）、logger 注入（file/functions/worker/cors + jwt WithLogger）、gateway 双注册 + 请求日志 | `go build ./...` |
| 6 | buildinfo 包 + provides + Taskfile ldflags 修复 + main.go var 扩展 | `task wire-all` + wire_gen 检查 |
| 7 | 测试补齐 + 全量验证 | 见 §5 |

每步完成跑 `gofmt -l .`（必须空）+ `go vet ./...`。

---

## 5. 测试与验证

- **checkers 单测**（`internal/infra/health/checks_test.go`）：fake Check 函数，验证
  超时（sleep > Timeout）、失败传播（Details 逐依赖状态）、recover 兜底（panic 的
  fake → unavailable）。
- **dbhook 单测**（`internal/infra/clients/dbhook_test.go`）：构造 `*bun.QueryEvent`
  （StartTime 回拨），验证阈值命中/不命中、阈值 0 禁用、LogAll 分支、Err 字段；
  `e.Operation()` 调用正确。
- **handler 测试**（`internal/api/servergrpc/health_test.go`）：fake checkers
  （全 ok → ok；一个失败 → unavailable + 明细）；`GetVersion` 返回注入值。
- **readiness 503 测试**（评审修正——不能依赖 grpc_gateway_test 模式；真实监听）：
  `net.Listen("tcp", "127.0.0.1:0")` 取端口 → `lynxhttp.NewServer(h,
  lynxhttp.WithAddr(port), lynxhttp.WithHealthCheckers(fake-func))` → goroutine
  `Start(ctx)` → HTTP GET `/healthz/readiness`：fake 全 ok → 200；一个失败 → 503 →
  cancel。
- **全量验证**：`go test ./...`（.env 提供 TORCHWOOD_TEST_DATABASE_SOURCE）、
  `task lint`、`task build`；构建后手动验证
  `go build -ldflags "-X main.version=v-test -X main.commit=abc -X main.date=20260809" -o bin/ ./cmd/server`
  并请求 `/v1/health`、`/v1/server/health/version`（**路径用 `bin/server` 而非
  `server.exe`，跨平台**；CI Linux 上 `task build` 自动带上修复后的 ldflags）。
- CI 无需改动（无 Docker 依赖）。

---

## 6. 范围外（明确不做）

- OpenTelemetry 追踪 / OTLP 上报（`telemetry.enabled` 保持未接线）。
- 自定义 Prometheus 业务指标（现有 `/metrics` 仅 runtime 采集器）。
- gRPC health Watch 流、健康状态缓存/熔断（每次实时探测）。
- 慢查询统计表 / 采样率。
- Console Dashboard 健康/版本展示（可选加分项）。
- HTTP handler panic recovery 中间件（lynxhttp 未挂 `WithMiddleware(Recovery())`；
  可作为可选加分项，非验收必需）。

---

## 7. 关键坑（实现时必须注意）

1. **authz fail-closed**：`GetVersion` 继承服务级 `ACCESS_PUBLIC` 即可（health.proto:11）；
   生成后确认 `File_server_v1_health_proto` 已在 collectMethodsByAccess 列表。
2. **ldflags 变量名**：`main.version` 全小写；Taskfile 现用 `main.Version` 无效——必须
   修正并验证（`-X main.version=v-test` 构建后请求版本端点）。
3. **Checker 无 ctx**：`CheckHealth() error` 无 context——超时必须在 Checker 内部用
   `context.WithTimeout(context.Background(), 2s)` 自控。
4. **`WithHealthCheckers` 签名**：参数是 `func() []lynx.Checker`（`lynx.HealthCheckersFunc`），
   不是切片；且 **gRPC 与 gateway HTTP 两侧都要注册**（HTTP 侧驱动
   `/healthz/readiness`，gRPC 侧同步 health 协议）。
5. **MinIO 探测走端口**：`ObjectStore` 加 `Ping` 方法（minio: BucketExists；memstore:
   nil）——不要用类型断言（minioObjectStore 未导出，断言必失败）。
6. **Check 响应兼容**：`dependencies` 为 proto3 追加字段；`status` 保持 `ok`/`unavailable`
   （200 语义），503 由 readiness 承担；gRPC 不可达时 gateway 503 属进程级故障路径。
7. **BuildInfo 包位置**：类型必须放 `pkg/buildinfo`（package main 无法被 api
   层 import）。
8. **hook 挂载点**：`newDatabase`（database.go:58）内、`NewDataClients(cfg, logger)` 链；
   `NewDatabase` 访问器不动；testutil 不受影响。
9. **请求日志级别**：lynx 请求日志是 Debug 级（默认 info 不可见）；`WithRequestLog`
   与 `WithLogger` 都要传。
10. **并行探测**：`Details` 内 goroutine + WaitGroup（各自 2s 超时，总时长 ≤ 2s）；
    panic recover → unavailable。
11. **CORS 测试波及**：`CORSMiddleware` 加 logger 后
    `observability_acceptance_test.go:42` 需同步改。
12. **JWT 拦截器**：用现成 `WithLogger` 链式方法（grpc.go:71 处），不改签名。
