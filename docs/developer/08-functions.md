# 08 函数：执行器、并发与异步

> 面向后端开发者：Docker 构建/运行、7 个写方法鉴权、全局信号量、裁剪与 Outbox 超时。
> 源码：`internal/domain/functions/`、`internal/infra/functions/docker.go`、`internal/app/functions/`、`pkg/semaphore/semaphore.go`、`cmd/worker/`、`internal/infra/events/outbox_worker.go`。
> 对应 `AGENTS.md`：Clean Architecture（`api→app→domain→infra`）、Wire（`cmd/server/provides.go→wire_gen.go`）、Proto 单一事实来源。
> 阅读顺序：`06-databases.md`（三层与出箱）→ `07-storage.md`（分片锁对照）→ 本章 → `09-api-guide.md`（新增 RPC）。

## 1 架构

```
gRPC FunctionsService (proto/server/v1/functions.proto) ─→ app/functions ─┬→ FunctionRepo (bun: functions/function_deployments/function_variables/function_executions)
                                                                           ├→ Executor (Docker: Build/Execute/RemoveImage, internal/infra/functions/docker.go)
                                                                           └→ Queue (Redis: torchwood:queue:functions-executions, internal/infra/queue/redis_queue.go) ─→ cmd/worker (4×BRPOP 1s)
HTTP multipart FunctionsHandler (internal/api/serverhttp/functions_handler.go, POST .../deployments/code, ≤50MiB) ─┘
```

- 真实 Docker（`internal/infra/functions/docker.go:Build/Execute`），非 stub；MVP 单机与 `os.TempDir()/torchwood-functions/<project>/<function>/<deployment>.zip` 共享文件系统，多机需对象存储。
- 四表 `db/migrations/000010_functions.*.sql`，`internal/infra/bun/model/function.go`；`internal/domain/functions/` 定义 `Execution`/`Deployment` 模型与 `Repository`/`Executor` 端口。

## 2 7 个写方法与鉴权

`proto/server/v1/functions.proto:61` `FunctionsService` 共 13 RPC（`ACCESS_SERVER` 默认），其中 **7 个写方法**在用例层以 `appshared.RequireServerPrincipal` 纵深防御（`internal/app/functions/*.go`）：

| RPC | HTTP | 写语义 |
|---|---|---|
| `CreateFunction` | `POST /v1/server/functions` | `timeout_seconds∈[1,300]`，缺省 `shared-1x/15s` |
| `UpdateFunction` | `PATCH .../{function_id}` | `optional name/entrypoint/timeout/spec/enabled` |
| `DeleteFunction` | `DELETE .../{function_id}` | 级联删部署+`RemoveImage`+删 zip（幂等） |
| `CreateDeployment` | `POST .../{function_id}/deployments` | gRPC `bytes code` ≤1MiB；大包走 `POST .../deployments/code` multipart ≤50MiB |
| `DeleteDeployment` | `DELETE .../{function_id}/deployments/{deployment_id}` |  |
| `SetVariables` | `PUT .../{function_id}/variables` | 全量替换，`repeated Variable`，明文存储（`function_variables`） |
| `CreateExecution` | `POST .../{function_id}/executions` | 同/异步二选一（见 §5） |

`RequireServerPrincipal` 允许 `System`/`PlatformAdmin`/`keys`；`viewer` 等细粒度由拦截器按 proto `method_auth` 的 `admin_roles` 门禁把关（Functions 写方法为 delegated_platform 档 = admin/owner，见 `authz-matrix.md`）。读方法（`List*/Get*`/`ListRuntimes/Specifications`/`GetVariables`）不强制写角色。

## 3 运行时与构建

支持 `runtimes.go`：`node-18.0`（`index.js:main`）/`python-3.11`（`main.py:main`），`spec`: `shared-1x(0.5CPU/256MB)`/`shared-2x(1CPU/512MB)`。

`dockerfileFor`：

```dockerfile
FROM node:18-alpine
COPY . .; USER node
CMD ["node","-e","const {main}=require('./index');Promise.resolve(main(JSON.parse(process.env.TW_DATA||'{}'))).then(r=>console.log(JSON.stringify(r)))"]
# python: FROM python:3.11-alpine; python -c "import json,os,main;print(json.dumps(main.main(...)))"
```

流程（`internal/app/functions/deployments.go`）：校验 zip 魔数 `PK\x03\x04` + 50MiB 限制 → 落库 `pending` → 占构建信号量 → `building` → `executor.Build`（解压≤1000 条/单条≤100MiB/总量≤200MiB，拒绝符号链接与 `zip slip`）→ `ready`/`failed`。镜像名 `<registry>/func-<fid>-<did>`（`storage: functions.docker.registry`，默认 `torchwood-funcs`）。

## 4 执行（同步/异步）

`CreateExecution` 校验 `data≤32KB` 且为 JSON object（数组/标量/null 拒绝），`data+env≤32KB`（`maxEnvBytes`），缺省取最新 `ready` 部署。

- **同步**（`async=false`）：`timeout_seconds>30` 拒绝（网关 `WriteTimeout` 余量，`maxSyncTimeoutSeconds`），占运行信号量后 `executor.Execute` 写回 `stdout/stderr/response`（各≤64KB 截断，`maxOutputBytes`）→ `completed/failed`。
- **异步**：`status=queued` → `LPUSH torchwood:queue:functions-executions`（`internal/infra/queue/redis_queue.go`）payload `{execution_id,function_id,project_id,data,attempt?}` → `Queued`，首次无 `attempt`，重试 +1 持久化于消息体。
- 状态机 `queued→building(补构建)→running→completed|failed`；`failed` 聚合 `error`（stderr/`timed out`/`build failed`），`duration_ms`/`status_code` 落库；每函数保留最近 100 条（`PruneOldExecutions`）。

安全基线（`docker.go:Execute`）：`CapDrop ALL`、`no-new-privileges`、只读根文件系统+`/tmp` tmpfs、`memory/cpu/pids(512)` 按 spec、网络默认 per-project 隔离 bridge `tw-func-<project.id>`（项目间函数容器互不可达；显式配置 `functions.docker.network` 时 opt-in 全局网络，跨项目容器同网互通有横向访问风险，见 `configs/config.yaml.template` 警告），`TW_DATA` 传参，超时强制删容器。

### 4.1 事务边界（redesign §4.8 Phase 2 形态乙，阶段③-b 定稿）

函数代码运行在**外部 Docker 容器**（node/python 镜像，进程隔离）：输入经 `TW_DATA` 环境变量注入、输出从 stdout 收集，与 server 不共享 `context.Context` 或数据库事务连接（探查结论：容器内无 SDK 注入、无回环网络通道、无 `JoinTx` 机制）。因此 **Phase 2 的事务上下文注入（形态甲）前提不成立，声明降级为形态乙**：

- 函数内的**多写原子性**统一由 Phase 1 的 `DatabasesService/ExecuteTransactions`（`POST /v1/server/databases/{db}/collections` 面 `documents:execute-tx`，见 `06-databases.md` §8.1）提供——函数代码通过 API/SDK（scoped API Key）调用即可，ATOMIC 批成功全提交、任一失败整批回滚，批内事件序 = op 序。
- 单条文档写本身的原子性（数据行 + `_acl` + outbox 事件同事务）由服务端保证，与调用方是否为函数无关。
- 跨进程事务协调（两阶段提交/补偿协调器）不在 POC 范围；如未来函数改宿主进程内运行时（如嵌入式解释器），可再评估形态甲。

## 5 全局信号量（`pkg/semaphore`）

`pkg/semaphore/semaphore.go:52` `RedisSemaphore` + `InMemorySemaphore` 回退（`ProvideSemaphores`，`internal/app/functions/semaphores.go:21`），`Semaphore` 接口 `TryAcquire(ctx)(bool, func(), error)`：

| 信号量 | max | TTL | key 前缀 |
|---|---|---|---|
| `Build` | 4 | 360s | `torchwood:sem:build:slot:<idx>` |
| `Run` | 16 | 400s | `torchwood:sem:run:slot:<idx>` |

实现：依次 `SETNX key token EX ttl`（`token=uuid`）抢槽位，命中即成功，返回 `release` 闭包 `Eval Lua "if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) else return 0 end"`（防误删过期后被他人占用的槽位，`context.Background()` 释放，`semaphore.go:88`）。TTL 覆盖最长持有：`360s>workerRebuildTimeout 5m`，`400s>300s timeout +60s 余量`。`client==nil` 时回退 `InMemorySemaphore{ch: make(chan struct{},max)}`（`chan` 非阻塞 `TryAcquire`）；`NoopSemaphore` 供测试。

```go
ok, release, err := semaphores.Build.TryAcquire(ctx)
if err != nil { return status.Error(codes.Internal, ...) }
if !ok { return status.Error(codes.ResourceExhausted, "too many builds") }
defer release()
```

构建与运行分别计数（`internal/app/functions/semaphores.go:13` `Semaphores{Build,Run}`），Wire 以类型区分同接口不同配额。

## 6 Worker 与 Trim

`cmd/worker`（独立 lynx 二进制，无 `api` 层，`cmd/worker/provides.go`）：

- 消费：4 goroutine `BRPOP`（`1s` 超时配合退出）→ `ProcessExecutionPayload`（见 `internal/app/functions/executions.go:242`）。
- 领取：`TransitionExecutionStatus(queued→building)` CAS 防重复投递，重复消息静默跳过（at-least-once 收敛）。
- 补构建：非 `ready` 时 `context.WithTimeout(5m)` 同步 `buildDeployment`，失败归还 `building→queued` 并 `requeue`（见下）。
- 重试：`queueMessage.Attempt` 持久化于 payload，瞬时失败 `requeue` 时 `+1 LPUSH`，`>maxProcessAttempts=3` 则 `FailExecutionIfActive` 标记 `failed`；`ErrInvalidQueuePayload` 丢弃不重试。
- 启动对账：`RecoverOrphanExecutions(1h)` 按 `public.projects` 轮转扫描，将 `queued/building/running>1h` 标 `failed`（全局预算 `500`，`scanCursor` 轮转防饥饿）。
- 优雅退出：`Stop` 取消 `BRPOP` 上下文。

`StreamTrimmer`（`cmd/worker/trimmer.go:14`）：每 10min `XTRIM APPROX torchwood:queue:functions-executions MAXLEN 100000`（`XADD` 不设 `MaxLen` 保未投递，裁剪低频 `Trim`，`context.WithTimeout(10s, WithoutCancel)`）。

## 7 per-statement 超时（Functions 侧）

`internal/infra/bun/bunrepo/*.go` 每方法入口 `context.WithTimeout(ctx,5s/10s)`（读 `5s`、写 `10s`，`WithoutCancel` 不受上游取消牵连）：`function_repo.go` 的 `Get/List/Create/Update/Delete`、`assets_repo.go:28` `Create(10s)`/`Get(5s)` 等；`internal/infra/queue/redis_queue.go:44` `Enqueue/Dequeue/Trim` 均为 `5s`；`internal/app/functions/executions.go:309` `workerRebuildTimeout=5m` 仅 `buildDeployment` 侧。

## 8 OutboxWorker 与 per-statement 超时（事件脊柱）

Worker 与事件脊柱共享 `outboxStatementTimeout` 语义（见 `06-databases.md` 三层中的出箱表），Functions 的异步执行投递失败亦通过同一 `outbox` 事件对外可见（通过 `shared.EventPublisher` 在 `documentdb` 写事务内 `INSERT outbox`）。

`internal/infra/events/outbox_worker.go:37` `outboxStatementTimeout=5s`（事务 `2*5s`）：

| 语句 | 超时 | 说明 |
|---|---|---|
| `SELECT COUNT(*) pending` 指标 | 5s | `outboxPending` Gauge，每轮 `pollOnce` 先刷新 |
| `claim` (`SELECT ... FOR UPDATE SKIP LOCKED` + `UPDATE dispatched_at`) | 10s（`RunInTx`） | 2 倍语句超时覆盖事务，`LIMIT 32`，行锁防多副本重复 XADD |
| `XADD` 失败 `failRow` 退避 `UPDATE attempts/available_at` | 5s | 指数 `1<<attempts` 秒，上限 `60s`，`dispatched_at=NULL` 快速重试 |
| 死信迁入 `INSERT ... SELECT → DELETE` | 10s（`RunInTx`） | `attempts≥10` 入 `document_events_outbox_dead`，`pending/dead` 指标更新 |
| 清理 `published>24h`/`dead>30d` | 5s ×3 | `cleanupOnce` 启动即执行，随后 `10m` 周期，`outboxCleanupInterval` |

示例（本地复现信号量与超时）：

```bash
go test ./pkg/semaphore -run TestRedisSemaphore -count=1
TORCHWOOD_RUN_DOCKER_TESTS=1 go test ./internal/infra/functions -run TestDockerBuild -count=1
```

轮询 `200ms`，`batch=32`，`available_at<=NOW()` 且 `(dispatched_at IS NULL OR <2m)` 重领，`maxAttempts=10` 入 `document_events_outbox_dead`，`pending/dead` Gauges 与 `publish_lag` Histogram（Prometheus）。

## 8 配置

`internal/pkg/config/config.proto`：`functions.executor=docker`、`functions.docker.host`（`TORCHWOOD_FUNCTIONS_DOCKER_HOST`，默认 `unix:///var/run/docker.sock`，构造失败延迟到首次调用）、`functions.docker.network`（默认留空 = per-project 网络 `tw-func-<project_id>`，执行器不存在时自动创建 bridge；显式配置为 opt-in 全局网络，见 §4 安全基线与 `configs/config.yaml.template` 警告）、`functions.docker.registry`（小写，默认 `torchwood-funcs`）。`Taskfile.yml` `task dev:worker` 跑 `go run ./cmd/worker`，`task build` 同时产出 `server/worker/torchwood`。

## 9 变量与裁剪

- 变量 `SetVariables` 全量替换，`sanitizeEnv` 丢弃键含 `\n\r\0`，执行时 `envSize(vars)+len(data)≤32KB`（`executions.go:82`）；`GetVariables` 返回掩码 `******`（`variables.go:secretMask`），真实值仅在 `Set` 请求可见一次。
- `PruneOldExecutionsInProject` 每函数保留最近 100 条（`executions.go:152`），`DeleteFunction` 级联清 `deployments/variables/executions` + 逐镜像 `RemoveImage` + 删 `os.TempDir()/torchwood-functions/<pid>/<fid>/<did>.zip`（失败仅日志）。
- `XTRIM` 不在 `Enqueue` 侧做，Worker `StreamTrimmer` 以 `10m` 周期间隔异步 `Trim`（`internal/domain/shared/ports.go:QueueFunctionsExecutions="torchwood:queue:functions-executions"`），`MAXLEN≈100k`，`APPROX` 单次 `O(被裁剪部分)`，水位远高于正常积压。

## 10 超时与可观测性

- 同步执行 `runExecution` 外层 `grpc/interceptor` 已设 `lynxgrpc.WithTimeout`，内层 `executor.Execute` 以 `fn.TimeoutSeconds` 为 `context.WithTimeout`，`servergrpc/functions.go:304` 额外 `+60s` 余量覆盖镜像清理。
- 指标 `torchwood_outbox_*`（`outbox_worker.go`）与 `torchwood_function_duration_ms`（`meterDuration` 以 `200ms` `WithoutCancel` 异步 `Incr` 到 `usage` 表）均 best-effort。
- 日志 `stdout/stderr` 容器侧缓冲 `1MiB`，`executionErrorMessage` 优先取 `status.Message`，`error` 列截断 `64KB`。

## 11 测试与边界

- 单元：`internal/app/functions/functions_test.go`/`executions_test.go`/`mocks_test.go`（`maxConcurrentBuilds/Runs`、截断、队列 payload 校验、`RequireServerPrincipal` 分支）；`internal/infra/queue/redis_queue_test.go`（`LPUSH/BRPOP`、`Trim`）。
- 安全：`security_test.go` 校验代码包 `zip slip`/符号链接/size 上限；`authz_test.go` 校验写方法鉴权；`semaphore_test.go` 校验 `SETNX+Lua` 互斥。
- 集成：`internal/infra/functions/docker_integration_test.go`（`TORCHWOOD_RUN_DOCKER_TESTS=1`，CI 预拉 `node:18-alpine`/`python:3.11-alpine`）；`cmd/worker/consume_test.go` / `requeue_test.go`（`attempt` 持久化、死信未落、`Transition` CAS）。
- 未落地：独立构建队列（`CreateDeployment` 同步构建，Worker 消费前补构建兜底）；重试无死信队列（超限 `FailExecutionIfActive`）；变量明文；`entrypoint` 固定入口；多机需对象存储承载 zip。

## 12 参考

- `proto/server/v1/functions.proto:61` 服务与 `shared.v1.Empty` 复用；`internal/app/functions/semaphores.go:21` 信号量提供方；`internal/domain/functions/repo.go` 端口契约。
- `internal/infra/functions/docker.go:238` `timeoutFromExec` 与容器 `Remove` 兜底；`internal/infra/queue/redis_queue.go:44` 队列 `5s` 超时；`internal/app/functions/runtimes.go` 运行时/规格清单。
- `AGENTS.md` §开发流程（`task generate:proto/wire:all`）与 `docs/roadmap.md` §0 Agent-Native API 定位；`docs/developer/09-api-guide.md` §1 新增 RPC 全流程。
- 关联：`07-storage.md` 的分片锁 `SETNX EX 300` 与本章信号量同属 Redis 原子语义，可对照实现；进阶可读 `internal/app/functions/management.go` 的幂等清理。
- 另见 `docs/developer/05-authentication.md` 的 `RequireServerPrincipal` 在 Storage/Functions 的一致应用。
- 关联 `docs/developer/09-api-guide.md` §11 的 `OutboxService` 可作为新增 Functions RPC 的端到端参照。
