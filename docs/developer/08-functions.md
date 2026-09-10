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

## 2 8 个写方法与鉴权

`proto/server/v1/functions.proto:61` `FunctionsService` 共 14 RPC（`ACCESS_SERVER` 默认），其中 **8 个写方法**在用例层以 `appshared.RequireServerPrincipal` 纵深防御（`internal/app/functions/*.go`）：

| RPC | HTTP | 写语义 |
|---|---|---|
| `CreateFunction` | `POST /v1/server/functions` | `timeout_seconds∈[1,300]`，缺省 `shared-1x/15s`；可带 `declared_scopes`（见 §4.2） |
| `UpdateFunction` | `PATCH .../{function_id}` | `optional name/entrypoint/timeout/spec/enabled`（无 repeated，scopes 走独立 RPC） |
| `SetFunctionScopes` | `PUT .../{function_id}/scopes` | 全量替换 `declared_scopes`（P0 执行身份；空集 = 撤销全部平台访问） |
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

### 4.2 执行身份（execution principal，P0）

设计：`docs/design/functions-execution-identity-and-triggers.md` §1/§2。函数执行获得平台注入的**短期受限凭证**，替代「开发者往 variables 塞长期 API key」。

**注入的 env（`internal/app/functions/executions.go`，同步与 worker 异步路径一致）**：

- `TW_EXECUTION_TOKEN`：`twx_` 前缀的不透明 token（32 字节 crypto/rand base64url）。服务端 Redis 键 `torchwood:exec-token:sha256hex(token)` 存身份投影 `{project_id, function_id, execution_id, scopes, invoking_user_id}`，**不存原值**；TTL = 函数超时 + 60s（仅崩溃兜底）——执行结束（成功/失败/panic）即**主动吊销**（`internal/infra/functions/execution_token_redis.go`；实现在 infra/functions 而非 infra/auth，worker 依赖图禁入后者），「执行结束即失效」是主动语义，stdout 回显 token 的残余时效为秒级。
- `TW_API_BASE_URL`：函数容器回访 Server API 的可达地址（`functions.execution.api_base_url`）。**dokploy compose 已接好**：dispatcher 按 `functions.dispatcher.callback_container`（= `torchwood-server`）把 server 容器 attach 进每个函数网络，`api_base_url` 填容器名地址 `http://torchwood-server:9080`——容器名 DNS 在 user-defined 网络（含 internal 变体）上均由内置 DNS 解析。手动部署须自行满足两个一致性：①该地址在函数网络内可达（server attach 进 `tw-func-<project>[-int]`，或经宿主桥地址）；②server/worker 两进程配置同值（都经 buildExecution 注入）。为空则不注入（函数需自行解析地址）。

**函数内用法**：`Authorization: Bearer <TW_EXECUTION_TOKEN>` 调 `TW_API_BASE_URL` 的 Server API。`twx_` 前缀在凭证解析层（`internal/domain/shared/authn.go`）即判定为独立凭证族，构造 execution principal（`ActorKind=execution`，`internal/infra/auth/validator.go`）。

**declared_scopes（最小特权，默认空 = 无任何平台访问）**：

- 管理经 `CreateFunction.declared_scopes` / `SetFunctionScopes`（全量替换）；每项必须形如 `<resource>:<op>`，resource 白名单 = **assets / databases / users / groups / storage / subscriptions / payments**（`functions/projects/billing/outbox/oauthproviders` 等平台与编排面资源一律拒绝——含 `functions:*` 自我复制的递归放大面）；op ∈ {read, write}；自动去重（词表实现：`internal/domain/functions/scopes.go`）。
- **scope 门与 API key 完全同构**：declared scopes 运行时投影为 API key 同款权限串（`assets:write` → `assets.write`），经同一 `PolicySet.AllowsAPIKeyTargets` 求值（含 databases/storage 的实例寻址语义）。Redis 不可用 = 校验拒绝（fail-closed）。
- **数据面语义（重要）**：scope 门只是第一层。文档/存储的可见性基于 principal 角色（`Roles: [keys, key:function:<function_id>]`，复用 B14 per-key 数据隔离模型）——**使用 `databases:*` / `storage:*` scope 前，必须把目标集合/桶的 ACL 授予 `key:function:<function_id>` 角色（DocRole keys 族）**，否则 scope 过门但数据不可见（错误形态是空结果，最难排查）。Console 的集合/桶权限编辑处像授权 API key 一样授予该角色。
- **限流维度**：函数回访流量按 `api:execution:<project_id>:<function_id>` 独立计数（默认 6000/min，可配 `security.rate_limit.functions_execution`，`internal/api/interceptor/ratelimit.go`）——既不落 per-IP（全部容器经 bridge NAT 同一出口 IP 会互相击穿），也不落 per-user（每次执行独立桶 = 实质不限流）。
- **审计**：经执行身份发起的资产写，账本 `operator` 记录 `{kind:"execution", actor_id:<function_id>, user_id:<触发用户?>}`（`internal/app/assets/assets.go` operatorFrom），对标 PlayFab currentPlayerId 语义。

**variables kind 与旧指引弃用**：`function_variables` 新增 `kind TEXT CHECK (kind IN ('text','secret'))`（迁移 000013）。**不要再往 variables 存平台 API key**——那是明文列 + stdout 截断回存的双重泄漏面；平台能力一律用执行身份。variables 只放第三方密钥（如微信 SSV 的 AES key）；`kind` 的 API 面与 secret 注入解析随后续阶段开放（一期同表明文存储，`GetVariables` 掩码回显不变）。

## 4.3 执行器 v2：常驻 runner 为默认执行模型（P0.5）

设计：`docs/design/functions-execution-identity-and-triggers.md` §6（本节唯一事实源）；owner 裁决：CGI 形态（每请求一容器）为设计缺陷，常驻 runner 升级为默认执行模型，冷启动 = 池 0→1 扩容的单一代码路径。

**模型**：函数镜像 CMD 换为平台 runner（`internal/infra/functions/runner/`，node 先行；模板版本常量 `RunnerTemplateVersion=2` 落 `function_deployments.template_version`，构建期不执行用户代码的不变量不变）。runner 启动即加载用户模块（约定 `index.js` 导出 `main(TW_DATA)`），加载完成前 `/_tw/health` 返回 not-ready；加载后监听容器内 HTTP `:18080`（仅 per-project 桥网络可达）：`POST /` body = TW_DATA JSON + header `x-tw-execution-token`（每请求写入 `process.env.TW_EXECUTION_TOKEN` 再调 main——一期串行执行 1 并发/实例使逐请求覆盖安全），响应 200 `{"ok":true,"result":...}` / 500 `{"ok":false,"error":...}`（附加 stdout/stderr 字段为 console.* 尾部环缓冲）；达 `TW_MAX_REQUESTS` 自退出、SIGTERM 排空在途后退出。

**分发拓扑（owner 拍板方案③）**：独立 `functions-dispatcher` 进程（`cmd/functions-dispatcher` + `cmd/functions-dispatcher/internal/functionsdispatcher/`）专职持有 docker.sock（compose 唯一挂载点；dokploy 编排下 dispatcher 以 `user: root` 运行——镜像缺省 torchwood 用户读不了宿主 `root:docker` 的 sock，server/worker 不挂 sock 不受影响），按需 join `tw-func-<project>` 网络（自身容器 NetworkConnect 自 attach；宿主进程模式跳过——Linux 桥 IP 宿主可直达，Docker Desktop for Windows/macOS 的 VM 拓扑下容器 bridge IP 对宿主不可路由，v2 分发通路要求 Linux/dokploy compose 拓扑）。server/worker 经 HTTP API 分发（`internal/infra/functions/dispatcher_client.go` 适配 Executor 端口），零 daemon 依赖；zip 构建以 base64 内联传输（无共享文件系统假设）。API 面（内网专用 + 可选 `x-tw-dispatcher-token` 静态共享密钥）：

| 端点 | 入参 → 出参 | 说明 |
|---|---|---|
| `POST /v1/dispatch/builds` | `{project_id, function_id, deployment_id, zip_base64, function_timeout_seconds}` → `{error?}` | v2 模板构建；成功后旧 deployment 池 drain（宽限 ≤ 函数超时） |
| `POST /v1/dispatch/executions` | 执行规格 `{image, project_id, function_id, deployment_id, runtime, spec, timeout_seconds, env, execution_token, data, pool}` → `{status, response, stdout_tail, stderr_tail, duration_ms, status_code, error}` | 池管理热路径；接受调用方 ctx 超时 |
| `POST /v1/dispatch/images/remove` | `{function_id, deployment_id}` | 幂等 |

错误映射：排队超限 429 → `ResourceExhausted`、执行超时 504 → `DeadlineExceeded`、缺参 400。`TW_DATA` 由请求体承载、`TW_EXECUTION_TOKEN` 经分发 header 传递——**P0 的 mint→注入→defer revoke 链路不变，常驻的是容器不是凭证**，只换注入通道。

**池策略（平台默认 + per-function 列覆盖，迁移 000014）**：`min_instances`（默认 0 = 纯 scale-from-zero；≥1 保温）、`max_instances`（默认 2，突发上限）、`idle_ttl_seconds`（默认 300）、`max_requests_per_instance`（默认 1000，Lambda 同款防泄漏回收）+ 平台级常驻总量上限（每 daemon 默认 8，`functions.dispatcher.max_resident_instances`）。实现语义（`cmd/functions-dispatcher/internal/functionsdispatcher/pool.go`，Redis 注册表 `torchwood:fninst:{project}:{function}`）：

- **spawn 收敛**：同函数并发 spawn 经 `torchwood:fnspawn:*` SETNX 锁收敛为一次，其余请求等注册表（防 daemon 重启后全量冷启动风暴）；冷启动成本由触发 spawn 的请求支付但不独占实例；
- **有界排队**：池满时排队（深度上限 `queue_depth` 默认 32 + 队首超时 `queue_head_timeout` 默认 10s）→ 超限 429 `ResourceExhausted`，同步调用方不无界等在 30s ctx 上；
- **判活**：dispatch 认领续租（lease）+ busy 标记；busy 实例不因租约过期被回收（不误杀执行中实例），租约过期超 10min（请求方残留）才强杀；请求超时/handler 崩溃 → 杀整个实例（实例级隔离粒度，业界 FaaS 同款取舍；同实例跨请求共享模块级状态，仅同函数同租户）；
- **idle reaper**：15s 周期 docker inspect 与注册表 diff 幽灵对账；idle > idle_ttl 且实例数 > min_instances 回收；draining（max_requests 到期/部署更替）实例兜底清理；`function_resident_uptime_ms` 按实例存活累计（保温成本显式计费口径）。

**执行路径切换与回退**：`functions.executor` 二选一——`"docker"`（默认，v1 回退：每请求一容器、进程内执行、保留 run 信号量）或 `"dispatcher"`（v2：常驻 runner，**v2 路径跳过全局 run 信号量**——池上限/排队由 dispatcher 内部管控，双重限流会互相饿死）。不同时启用；python 函数 v2 暂不支持（构建期明确报错），对 python 保持 v1。**切换前须重新部署存量函数**：存量 deployment 的镜像是 v1 模板（CMD 跑完即退、`template_version` 为空），v2 分发会在健康探针处失败（boot timeout）——重新 `CreateDeployment` 即获得 runner 模板镜像（`template_version=2`）。SLA 口径：**热路径同步分发简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）**；冷启动与异步队列路径显式不在 SLA 内。配套清账：同步快路径两写预占记账（见 §4.4）、principal 短 TTL 缓存、`latest_ready_deployment_id` 热路径指针、function/variables 30s 缓存、Prune 移出热路径。

## 4.4 同步快路径两写预占记账（§6 约束①，K9）

同步执行跳过 queued/building 中间态：分发前 `INSERT (status='running', timeout_seconds 快照)` 直接预占——预占行即刻成为审计/限频计数依据（「先占位后执行」），执行中崩溃行留在 running、由周期孤儿恢复按 `staleAfter = timeout_seconds + 120s` 宽限判 failed（timeout 快照为 NULL 的存量行回退 1h；扫描范围 queued/building/running——修掉 v1「同步崩溃留 queued 永不入队」的洞）；结束后 `UPDATE` 终态（completed/failed + outputs + duration_ms）。孤儿恢复从「worker 启动跑一次」改为 **worker 周期 ticker（1min）**；Prune（保留最近 100 条）移出同步热路径，由 worker 10min 低频 ticker 承接。异步路径状态机 `queued→building→running` 原样保留。

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
- cron 调度循环（P1，`worker.go cronLoop`）：每分钟 `DispatchDueCronTriggers(now, 100)` 领取到期 cron 触发器并入队异步执行（见 §12.5）。
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

`pkg/config/config.proto`：`functions.executor=docker`、`functions.docker.host`（`TORCHWOOD_FUNCTIONS_DOCKER_HOST`，默认 `unix:///var/run/docker.sock`，构造失败延迟到首次调用）、`functions.docker.network`（默认留空 = per-project 网络 `tw-func-<project_id>`，执行器不存在时自动创建 bridge；显式配置为 opt-in 全局网络，见 §4 安全基线与 `configs/config.yaml.template` 警告）、`functions.docker.registry`（小写，默认 `torchwood-funcs`）、`functions.execution.api_base_url`（`TORCHWOOD_FUNCTIONS_EXECUTION_API_BASE_URL`，P0 执行身份：函数容器可达的 Server API 地址，注入 `TW_API_BASE_URL`；空 = 不注入，见 §4.2）、`functions.client_invoke.per_user_concurrency`（P2 每用户并发闸门，默认 2）、`functions.client_invoke.queue_head_timeout`（P2 并发闸门排队队首超时，默认 `5s`，见 §13）。`Taskfile.yml` `task dev:worker` 跑 `go run ./cmd/worker`，`task build` 同时产出 `server/worker/torchwood`。

## 9 变量与裁剪

- 变量 `SetVariables` 全量替换，`sanitizeEnv` 丢弃键含 `\n\r\0`，执行时 `envSize(vars)+len(data)≤32KB`（`executions.go:82`；`TW_EXECUTION_TOKEN`/`TW_API_BASE_URL` 注入亦计入该预算，约 200B 量级）；`GetVariables` 返回掩码 `******`（`variables.go:secretMask`），真实值仅在 `Set` 请求可见一次；`kind`（text/secret，迁移 000013）见 §4.2。
- **保留分级（P2 Q7）**：`PruneOldExecutionsInProject`（server 面 `trigger_source=''`）条数式每函数保留最近 100 条；`PruneTriggerExecutionsInProject`（client/http/cron 来源）时间窗保留 **48h**（≥2× 最长限频窗口 day=24h——这些行是限频 DB 降级的窗口内计数依据，条数裁剪会少计超发，§13.2）。两条删除路径互不误伤，均只清终态行。`DeleteFunction` 级联清 `deployments/variables/executions` + 逐镜像 `RemoveImage` + 删 `os.TempDir()/torchwood-functions/<pid>/<fid>/<did>.zip`（失败仅日志）。
- `XTRIM` 不在 `Enqueue` 侧做，Worker `StreamTrimmer` 以 `10m` 周期间隔异步 `Trim`（`internal/domain/shared/ports.go:QueueFunctionsExecutions="torchwood:queue:functions-executions"`），`MAXLEN≈100k`，`APPROX` 单次 `O(被裁剪部分)`，水位远高于正常积压。

## 10 超时与可观测性

- 同步执行 `runExecution` 外层 `grpc/interceptor` 已设 `lynxgrpc.WithTimeout`，内层 `executor.Execute` 以 `fn.TimeoutSeconds` 为 `context.WithTimeout`，`servergrpc/functions.go:304` 额外 `+60s` 余量覆盖镜像清理。
- 指标 `torchwood_outbox_*`（`outbox_worker.go`）与 `torchwood_function_duration_ms`（`meterDuration` 以 `200ms` `WithoutCancel` 异步 `Incr` 到 `usage` 表）均 best-effort。
- **P0.5 观测补全（Prometheus，包内自注册）**：`torchwood_functions_execution_duration_seconds{project,function,source,status}` 与 `torchwood_functions_executions_total{...,source,status}`（`internal/app/functions/metrics.go`；source=server|http|cron|client）、`torchwood_functions_queue_wait_seconds`（异步 queued→running）、`torchwood_functions_invoke_total{project,function,source,result}`（P1 触发器 + P2 客户端入口计数，§12.1/§13.2）。执行器 v2 侧（`cmd/functions-dispatcher/internal/functionsdispatcher/metrics.go`）：`torchwood_functions_cold_starts_total`（池 0→1 计数）、`torchwood_functions_init_duration_seconds`（对标 Lambda initializationDuration）、池水位 `torchwood_functions_pool_{ready,booting,draining}`、`torchwood_function_resident_uptime_ms`（保温成本计费口径）、`torchwood_functions_dispatch_duration_seconds` / `torchwood_functions_dispatch_queue_wait_seconds` / `torchwood_functions_dispatch_queue_{dropped,timeouts}_total`。SLA burn 告警锚点：热路径端到端 P99 > 100ms 或平台开销 > 25ms（狗粮期实测，§4.3 SLA 口径）。P2 新增：`torchwood_functions_egress_class_total{project,class=trusted|untrusted}`（容器创建 egress 分类计数，§13.5）。
- 日志 `stdout/stderr` 容器侧缓冲 `1MiB`，`executionErrorMessage` 优先取 `status.Message`，`error` 列截断 `64KB`。

## 11 测试与边界

- 单元：`internal/app/functions/functions_test.go`/`executions_test.go`/`mocks_test.go`（`maxConcurrentBuilds/Runs`、截断、队列 payload 校验、`RequireServerPrincipal` 分支）；`internal/infra/queue/redis_queue_test.go`（`LPUSH/BRPOP`、`Trim`）。
- 安全：`security_test.go` 校验代码包 `zip slip`/符号链接/size 上限；`authz_test.go` 校验写方法鉴权；`semaphore_test.go` 校验 `SETNX+Lua` 互斥。
- 集成：`internal/infra/functions/docker_integration_test.go`（`TORCHWOOD_RUN_DOCKER_TESTS=1`，CI 预拉 `node:18-alpine`/`python:3.11-alpine`）；`cmd/worker/consume_test.go` / `requeue_test.go`（`attempt` 持久化、死信未落、`Transition` CAS）。
- 未落地：独立构建队列（`CreateDeployment` 同步构建，Worker 消费前补构建兜底）；重试无死信队列（超限 `FailExecutionIfActive`）；变量明文；`entrypoint` 固定入口；多机需对象存储承载 zip。

## 12 触发器（P1：HTTP + cron，设计 `functions-execution-identity-and-triggers.md` §3）

实体 `function_triggers`（迁移 000015；`internal/domain/functions/triggers.go` 端口 + `bunrepo/function_trigger_repo.go`）：`type ∈ {http, cron}`，config JSONB 存分类型配置；http 的 token 提为独立列（`UNIQUE(token)` 支撑查找，cron 为 NULL——UNIQUE 不去重 NULL）。管理面走 Server RPC（镜像 SetFunctionScopes 的 method_auth：functions.write + admin/owner）：`CreateFunctionTrigger` / `ListFunctionTriggers` / `DeleteFunctionTrigger` / `RotateFunctionTriggerToken`；函数删除经 FK `ON DELETE CASCADE` 级联清理触发器（dispatcher 不需通知，靠 idle TTL 收敛——P0.5 既定）。

### 12.1 HTTP 触发器（公开 URL）

- **路由** `/f/{project_id}/{trigger_token}`（`internal/api/serverhttp/function_triggers_handler.go`，HandlePath 挂网关同 mux，payments 先例）。token 128bit 随机（base64url 22 字符），**不可猜即鉴权**；URL 含 token 会被代理/访问日志记录，疑似泄漏即调 `RotateFunctionTriggerToken`（旧 token 立即失效）。
- **只路由不验签（K4）**：请求以封套透传进 `TW_DATA`（函数内 `JSON.parse`）：

  ```json
  {
    "method": "POST",
    "path": "/f/p1/tok",
    "raw_query": "signature=...&timestamp=...&nonce=...&encrypt=...",
    "headers": { "content-type": ["application/xml"], "x-wx-signature": ["..."] },
    "body": "<xml>...</xml>",
    "body_base64": "PHhtbD4..."
  }
  ```

  **raw_query 必须透传**——微信 SSV 的验签参数在 query 而非 body。headers 白名单（统一小写键）：全部 `x-*`（大小写不敏感）+ `content-type` + `wechatpay-*` 前缀，其余剥除（Authorization 等凭证不透传）。body 双通道：`body` 为 best-effort UTF-8 字符串，`body_base64` 恒在（无损，二进制 webhook 用）。平台不解析 body 内容、日志不落 body 原文。
- **双响应模式**（创建时必选 `response_mode`）：
  - `sync`：同步执行（≤30s）并透传函数响应——completed → `200 + rec.Response`；failed → `502 + rec.Response`；超时 `504`；入队/执行错误按 gRPC code 映射。适合非时限场景。
  - `async_ack`：**先入队成功、后写 200（顺序红线）**——入队失败一律 `503` 让微信重试（先 200 后入队的抖动窗口 = 事件永久丢失且平台无痕迹）。200 body = 创建时配置的 `ack_body`（≤1KB，防公开端点带宽放大）。**诚实语义：async_ack 的 `{"is_valid":true}` 是「已受理」而非「验证通过」**——函数内验签失败无法追回响应，需配合幂等（见 §12.6）。
- **GET 握手**：`handshake: "echo"` 时平台对 GET 直接回 `{"echostr": <query.echostr>}`（`200`，`application/json`，`Cache-Control: no-store`），不 invoke；非 echo 模式 GET → `405`。echostr 回显不授予任何能力。
- **body 上限**：缺省 64KB、per-trigger 可配至 1MB（`body_limit_bytes`）；超限 `413`。生效上限取配置值与执行器通道能力的小者——v2（dispatcher）走 body 通道全额生效；v1 回退模式 data 经 env 注入受 32KB 预算约束，有效上限 24KB。
- **per-IP 限频**：Redis 固定窗口（`internal/infra/functions/trigger_ip_ratelimit.go`，键 `torchwood:ftrig:ip:{ip}`），独立默认 **3000/min**（微信回调出口 IP 段集中，沿用全局 300/min 会在发奖风暴下 429 → 重试耗尽 → 事件丢失），可配 `functions.trigger.http_ip_per_minute`；超限 `429 + Retry-After`；Redis 故障 fail-closed（503 让回调方重试）。
- **安全与审计**：该路由不经 gRPC 拦截器链（无 AuditInterceptor）——`function_executions.trigger_source = http:{trigger_id}`、`source_ip`（经 `security.trusted_proxies` 解析的来源 IP 摘要）即审计载体；执行 principal 的 scope/角色语义与 §4.2 完全一致。
- **指标**：`torchwood_functions_invoke_total{project, function, source="http", result=ok|quota|echo|not_found|error|timeout}`（echo 单列——探测行为可观测）。

### 12.2 cron 触发器

- 表达式：5 字段（分 时 日 月 周），**UTC**（K10；触发器级时区后置）；支持 `* , - /` 与数字；周 0-7 且 7≡0（周日）；越界一律解析报错（fail-closed）。解析器自实现于 `internal/domain/functions/cronexpr.go`（零第三方依赖），语义对齐 vixie cron：DOM 与 DOW 均受限时取并集。
- 调度：worker 每分钟 ticker（`cmd/worker/worker.go cronLoop`）→ `DispatchDueCronTriggers(now, 100)`：按 active 项目轮转扫描（镜像孤儿恢复模式）→ `ClaimDueCron` 原子领取。
- **先 CAS 后入队（红线）**：候选无锁扫描后逐条 `UPDATE ... SET next_run_at=新 WHERE id=$1 AND next_run_at=旧` 判 rows=1——多实例并发只有赢家（先入队后 CAS 在多实例下双入队；执行行 queued→building 的 CAS 防不了两条不同 execution）。推进目标恒 `> now`（否则下一轮扫描立即重复领取）。
- **misfire 语义**（默认 `catch_up_once`）：错过（到期早于 now-90s 宽限）时 `next_run_at` 直接推进到 now 之后的下一计划时刻——宕机 N 个周期只补跑 1 次（风暴由异步通道 + 信号量兜底，**异步路径无队列深度上限，run 信号量兜底**）；`skip` 同样推进但不补跑。入队失败把 `next_run_at` 回滚到原到期值（best-effort），下轮扫描按 catch_up 语义重领。
- 入队 data 为 `{"type":"cron","trigger_id":"...","scheduled_for":"<RFC3339>"}`——`scheduled_for` 是函数做幂等键的推荐来源。
- 管理 RPC 同 §12；`next_run_at` 在 List 响应可见。

### 12.3 微信 SSV 接入配方（P1 狗粮验收锚点）

微信小程序广告激励视频服务端回调（SSV）：用户看完广告 → 微信回调开发者 URL（1s 超时 × 重试 3 次）→ 验签通过回复 `{"is_valid":true}`。端到端链路：

1. **建触发器**：`POST /v1/server/functions/{function_id}/triggers`，`type=http`，`response_mode=async_ack`（1s 窗内同步执行大概率全超时致事件丢失），`ack_body={"is_valid":true}`，`handshake=echo`。得到 `invoke_path=/f/{project}/{token}`。
2. **微信侧配置**该 URL（GET 握手由平台 echo 回显完成，函数不参与）。
3. **函数内处理**（`TW_DATA` 即封套）：
   - 验签：`raw_query` 取 `signature/timestamp/nonce`，`body` 取加密报文，按微信广告 SSV 规范做 `sha256(sort(query)+body+app_secret)` 比对——app_secret 放 `function_variables` `kind=secret`（§4.2），不要硬编码。
   - 去重：以报文内 `transaction_id` 幂等——可用 `databases` scope 写一条 `{_id: transaction_id}` 去重表（重复回调插入冲突即跳过）。
   - AES 解密拿 `user_id`，再以 execution principal（`assets:write`，§4.2）调 Server API `assets grant` 给用户发复活券；ledger `operator` 记录自动溯源到 function + user。
4. **注意**：async_ack 的 200 是「已受理」语义——验签失败的回调无法在响应中拒绝（微信侧只看 200），兜底是函数幂等 + 微信「前端先发奖、SSV 对账补偿」的推荐策略。

### 12.4 cron 验收锚点

每日/赛季重置建 `type=cron` 触发器（如 `0 3 * * *`），验证：宕机 2 天后 worker 恢复，`catch_up_once` 恰补跑 1 次（`scheduled_for` 为原计划时刻）且 `next_run_at` 直接指向下一日 03:00；`skip` 模式只推进不补跑。

## 13 客户端调用（P2：END_USER 按 per-function 策略同步调用，设计 `functions-execution-identity-and-triggers.md` §4/§5）

客户端调用面是带独立策略门的**新入口**，不是 Server 面 `CreateExecution` 的放开：终端用户（Client API Bearer 登录态）可调用显式开启 `client_callable` 的函数，身份（project/user）取自 Principal、请求体不携带身份。执行走既有参数化核心路径（`internal/app/functions/clientinvoke.go` → `createExecution`），执行身份铸造（§4.2）、两写预占记账、账本 operator 溯源（function+user，§4.2）全部自动生效。审计载体 = `function_executions` 行（`trigger_source='client'` + `invoking_user_id`，设计 §6 约束③），该 RPC 在 `AuditInterceptor` 跳过清单内（`internal/api/interceptor/audit.go`）。

### 13.1 API 与 SLA 口径

- `POST /v1/functions/{function_id}:invoke`（`proto/client/v1/functions.proto`，`service_auth = ACCESS_END_USER`，gRPC 与 grpc-gateway 双注册）。请求：`function_id`（必填）、`data`（JSON object 字符串，≤32KB——**不放宽**，触发器的封套通道放宽不适用不可信调用方）、`deployment_id`（可选，须 ready）、`idempotency_key`（可选，≤128 字符）。
- 响应：`execution_id` / `status`（`completed | failed | running`——running 只在幂等命中且执行仍在进行时出现）/ `response`（stdout 末行 JSON）。函数执行失败是结果而非传输错误（HTTP 200 + `status=failed`）；容器 exit code 对客户端无语义，不暴露。
- **SLA 口径（K9，诚实承诺）**：热路径同步分发 = 简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）；**冷启动（池 0→1）与实例排队显式不在 SLA 内**——需要 sub-100ms 的函数设 `min_instances ≥ 1` 保温（idle TTL 内连接保活，见 §4.3）。超时沿用同步路径 ≤30s。

### 13.2 每用户限频（可配窗口，平台强制）

- 策略列（迁移 000016，函数级）：`client_callable`（存量全 FALSE ⇒ fail-closed）、`client_per_user_limit`（≥1 才可开启 callable）、`client_limit_window ∈ {minute, hour, day}`（day 按 UTC 日期）。管理经 Server 面 `CreateFunction`/`UpdateFunction` 的 proto3 optional 字段；Console 函数详情页「客户端调用」卡片同源。
- **窗口语义**：Redis 固定窗口 `torchwood:fnq:{project}:{function}:{user}:{bucket}`，bucket 按窗口粒度——minute=`YYYYMMDDHHMM`、hour=`YYYYMMDDHH`、day=UTC `YYYYMMDD`；**先限频后执行**（先 INCR 后预占 INSERT，与通用限流同原子性）。
- **超限错误形态**：`ResourceExhausted` + `ErrorInfo.Reason = FUNCTIONS.INVOKE_QUOTA_EXCEEDED` + `RetryInfo`（窗口结束时刻）——配额超额**不是响应字段**。HTTP 侧映射 429。
- **故障降级（fail-closed 收口）**：Redis 不可用时改查 `function_executions` 窗口内计数（`trigger_source='client' AND invoking_user_id AND created_at >= 窗口起点`，partial 索引 `function_executions_client_quota` 支撑）；**DB 亦不可用 = 拒绝（Unavailable）**——经济语义的限频不做通用限流式 fail-open 熔断。依赖保留分级（§9：client/http/cron 行 48h 时间窗保留 ≥ 2× 最长窗口）保证计数行不被条数 prune 裁掉。
- 指标：`torchwood_functions_invoke_total{project, function, source="client", result=ok|quota|error|timeout|not_found}`。
- 匿名：`client_anonymous_allowed` 字段保留、**一期禁用**（管理面遇 true 显式报错「一期未开放」，Q4）——匿名会话可无限新造，per-user 限频对匿名形同虚设；后续放开时需叠加按 IP 计数兜底。

### 13.3 幂等键语义与重试指引

- `idempotency_key` 提供时，预占 INSERT 携带 `client_idempotency_key`——partial 唯一索引 `(project_id, function_id, invoking_user_id, client_idempotency_key) WHERE client_idempotency_key <> ''` 在预占时刻生效，并发同键第二请求 INSERT 冲突即**回读既有行原样返回**（running 返回 running，completed 返回含 response 的终态；响应含幂等命中语义，SDK 侧 `Reused`/`idempotent` 标记）。未提供 key 行为不变。
- **重试指引**：网络超时/5xx 后携带**同一 key** 重试是安全的（不会重复执行）；不同业务操作必须换 key。**平台级去重之外，函数侧幂等仍是双保险**——「调用即发资产」的函数应以 `execution_id` 派生资产幂等键（grant 的 `idempotency_key`），防把同一 key 复用到不同业务动作。
- 注意：重复请求（同键）也会消耗一次限频配额（先限频后执行的既定次序）——高频重试方应以退避重试而非立即重放。

### 13.4 每用户并发闸门

- 全局 run 信号量（16）/池上限之外的第三道门：每用户并发上限（默认 2，`functions.client_invoke.per_user_concurrency`），进程内 keyed 信号量（channel），排队队首超时（默认 `5s`，`queue_head_timeout`）→ `ResourceExhausted`。
- **诚实声明：per-process 语义**——多实例部署下为「近似全局」（全局上限 = 上限 × 实例数）。跨进程精确闸门需要 Redis 分布式信号量，但其排队+队首超时语义在热路径上不划算（§6 预算），本期取舍为进程内实现 + 本文档明示。

### 13.5 egress 策略与部署要求（不可信函数默认 deny）

- **分类**：不可信 = `client_callable == true` **或** 存在 http/cron 触发器（含禁用——可随时重新启用，按行存在性分类更保守）；可信 = 其余（仅 server key 触发）。分类是**函数属性**，对该函数的所有触发来源一致生效，在 app 层完成（30s 缓存摊薄触发器查询），随执行规格传给 executor。
- **实现**：不可信函数容器 attach **internal 变体网络** `tw-func-<project>-int`（docker `internal: true`——阻断外网出口、网内互通保留）而非常规网络；v1 执行器与 v2 dispatcher 同分类（`internal/infra/functions/docker.go ResolveInternalNetworkName`、`functionsdispatcher` `EnsureProjectNetwork(…, untrusted)`）。dispatcher 随执行分布逐渐 join 两类网络（自身容器 NetworkConnect，幂等）。指标 `torchwood_functions_egress_class_total{project, class=trusted|untrusted}`。
- **一期语义（诚实偏离）**：per-function 域名级白名单需要 egress 代理原语，一期不实现——语义退化为 **trusted/untrusted 二分类**：不可信函数出网**全 deny**（含第三方 API），可信函数保持放开。需要外呼第三方（如支付网关）的函数不要开启 client_callable/触发器，或等待 egress 代理立项。
- **部署要求（dokploy compose 已接好；手动部署必读）**：`functions.execution.api_base_url` 必须填**函数网络内可达**的 Server API 地址——不可信函数在 internal 网络上无 NAT 出口，外部域名/IP 均不可达。compose 的接线：dispatcher 配置 `TORCHWOOD_FUNCTIONS_DISPATCHER_CALLBACK_CONTAINER=torchwood-server`，在 join 每个函数网络时把 server 容器（`container_name: torchwood-server`）一并 attach，`api_base_url = http://torchwood-server:9080`（容器名 DNS 内网解析）。attach 失败仅告警不阻断执行（函数可能无需回访），但不可信函数的平台调用（assets grant 等）将连接失败——部署后用一条带 declared_scopes 的函数实跑验证回访连通性。

### 13.6 客户端接入示例

```ts
import { Torchwood } from "@torchwoodcloud/sdk";

const tw = Torchwood.withAccessToken(endpoint, projectId, accessToken);
// 签到（day 窗口限频由平台强制；幂等键防网络重试重复发奖）
const res = await tw.functions.invokeFunction("daily_signin", {
  data: JSON.stringify({ day: "2026-09-09" }),
  idempotency_key: "signin-u1-20260909",
});
if (res.status === "completed") {
  const payload = JSON.parse(res.response ?? "{}");
}
// 429 = 超限：读 Retry-After（RetryInfo），窗口结束后重试（换新请求、可保留同幂等键）
```

Go SDK：`client.New(...).Functions.InvokeString(ctx, "daily_signin", data, idempotencyKey, "")`。

## 14 参考

- `proto/server/v1/functions.proto:61` 服务与 `shared.v1.Empty` 复用；`internal/app/functions/semaphores.go:21` 信号量提供方；`internal/domain/functions/repo.go` 端口契约。
- `internal/infra/functions/docker.go:238` `timeoutFromExec` 与容器 `Remove` 兜底；`internal/infra/queue/redis_queue.go:44` 队列 `5s` 超时；`internal/app/functions/runtimes.go` 运行时/规格清单。
- `AGENTS.md` §开发流程（`task generate:proto/wire:all`）与 `docs/roadmap.md` §0 Agent-Native API 定位；`docs/developer/09-api-guide.md` §1 新增 RPC 全流程。
- 关联：`07-storage.md` 的分片锁 `SETNX EX 300` 与本章信号量同属 Redis 原子语义，可对照实现；进阶可读 `internal/app/functions/management.go` 的幂等清理。
- 另见 `docs/developer/05-authentication.md` 的 `RequireServerPrincipal` 在 Storage/Functions 的一致应用。
- 关联 `docs/developer/09-api-guide.md` §11 的 `OutboxService` 可作为新增 Functions RPC 的端到端参照。
