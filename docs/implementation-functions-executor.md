# Torchwood Functions 真实执行器实现方案

> 状态：待实现（接力任务）
> 目标读者：接力的实现 agent
> 关联：`docs/roadmap.md` §2.6（Functions 真实执行器）、`docs/archived/tech-decision.md`（技术选型，已归档）、`AGENTS.md`（开发约定，必读）
> 修订记录：2026-08-09 v2（评审修订：FK 级联、registry/network、同步超时、zip slip、worker 对账、输出截断、并发限制等）

---

## 1. 目标与验收标准

把 `internal/infra/functions/docker.go` 的 P0 stub 替换为真实 Docker 执行器，并补齐完整的
Functions 服务链路：proto → gRPC handler → use-case → 静态表仓库 → Docker build/run →
Redis 队列 + `cmd/worker` 异步消费 → Console 管理页面。

**验收标准（roadmap §2.6 沿用）**：

1. 上传一个 Node.js 函数代码包后，可同步调用并返回 `console.log` 输出与 `main()` 返回值。
2. 函数执行超时后返回错误（gRPC `DeadlineExceeded`，HTTP 504）并清理容器（无残留）。
3. 异步执行可在 Console 中查看 execution 状态与 stdout/stderr 日志。
4. 越权调用（无 `functions` scope 的 API Key）返回 `PermissionDenied`。
5. `go test ./...`、`task lint`、`task build` 全绿；Docker 集成测试在无 daemon 时自动跳过。

---

## 2. 架构总览

```
gRPC handler (internal/api/servergrpc/functions.go)
  → app use-case (internal/app/functions/*.go)
      ├─ FunctionRepo (bun 静态表: functions/deployments/variables/executions)
      ├─ DockerExecutor (infra: build 镜像 + run 容器)
      └─ Queue (Redis List) ──→ cmd/worker (BRPOP 消费) ──→ DockerExecutor
                                    └─ 更新 execution 状态
HTTP multipart (internal/api/serverhttp/functions_handler.go)  ← deployment 代码包上传
Console (console/src/routes/functions/) ← grpc-gateway / server API
```

关键决策：

- **同步执行**：`POST /v1/server/functions/{id}/executions`（`async=false` 默认）直接调用
  Docker executor 并等待结果；也写 execution 记录（审计）。
- **异步执行**：`async=true` 时入 Redis 队列，立即返回 `status=queued` 的 execution 记录；
  `cmd/worker` 消费后更新状态。
- **执行状态机**：`queued → building → running → completed | failed`。
- **运行时镜像**：不预构建 runtime 镜像，Dockerfile 直接 `FROM node:18-alpine` /
  `FROM python:3.11-alpine`（拉取官方镜像，自洽无额外依赖）。
- **队列**：Redis List（`BRPOP`），payload 为 execution_id JSON；**不做 ack/重试/死信**
  （worker 崩溃的任务由 worker 启动对账标记 failed，见 §5.5）。
- **持久化**：functions/deployments/variables/executions 四张静态表，bun + golang-migrate
  （与 projects/api_keys 一致，不放入 documentdb 动态层）。
- **部署假设（MVP）**：单机部署 —— server 与 worker 共享文件系统（zip 代码包存本地
  临时目录，见 §5.3）与 Docker daemon socket。
- **构建流程定案**：`CreateDeployment` **同步构建**镜像（请求内完成，构建日志写入
  deployment.error；失败则 status=failed）；不设独立构建队列（对 roadmap「构建队列」
  任务的 MVP 偏离，见 §8）。worker 消费 execution 前若 deployment 非 `ready` 先补构建。

---

## 3. 数据模型（db/migrations/ 新增）

新增 migration `XXXXXX_functions.up.sql` / `.down.sql`（编号取当前最大编号 + 1，参考
`db/migrations/` 现有命名；当前最大为 000009）：

```sql
CREATE TABLE functions (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  name TEXT NOT NULL,
  runtime TEXT NOT NULL,              -- node-18.0 / python-3.11
  entrypoint TEXT NOT NULL DEFAULT 'index.main',  -- MVP 仅占位，见 §5.3
  timeout_seconds INT NOT NULL DEFAULT 15,
  spec TEXT NOT NULL DEFAULT 'shared-1x',
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_functions_project ON functions (project_id);

CREATE TABLE function_deployments (
  id TEXT PRIMARY KEY,
  function_id TEXT NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  size BIGINT NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',   -- pending/building/ready/failed
  error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_function_deployments_function ON function_deployments (function_id);

CREATE TABLE function_variables (
  id TEXT PRIMARY KEY,
  function_id TEXT NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,                -- MVP 明文存储，见 §8 风险声明
  UNIQUE (function_id, key)
);

CREATE TABLE function_executions (
  id TEXT PRIMARY KEY,
  function_id TEXT NOT NULL REFERENCES functions(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL REFERENCES function_deployments(id) ON DELETE CASCADE,
  status TEXT NOT NULL DEFAULT 'queued',    -- queued/building/running/completed/failed
  response TEXT NOT NULL DEFAULT '',
  response_truncated BOOLEAN NOT NULL DEFAULT FALSE,
  stdout TEXT NOT NULL DEFAULT '',
  stdout_truncated BOOLEAN NOT NULL DEFAULT FALSE,
  stderr TEXT NOT NULL DEFAULT '',
  stderr_truncated BOOLEAN NOT NULL DEFAULT FALSE,
  status_code INT NOT NULL DEFAULT 0,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_function_executions_function ON function_executions (function_id, created_at DESC);
```

要点：

- **FK 全部 `ON DELETE CASCADE`**：删函数 → 部署/变量/执行记录级联删除；删部署 →
  其执行记录级联删除（评审修正：缺 cascade 时删除必然 FK violation）。
- **输出截断**：stdout/stderr/response 各截断 64KB，超限置对应 `*_truncated=true`（§5.3）。
- **保留策略**：`CreateExecution` 后顺带清理该函数超过最近 100 条的更旧记录
  （单条 `DELETE ... WHERE function_id=? AND id NOT IN (SELECT id FROM ... ORDER BY created_at DESC LIMIT 100)`，
  失败仅记日志）。

Bun 模型放 `internal/infra/bun/model/function.go`（对照 `model/project.go` 的 tag 风格：
`bun:"table:functions,alias:f"`）。

---

## 4. 端口定义

### 4.1 `internal/domain/functions/repo.go`（新建）

```go
package functions

// FunctionRepo 持久化函数/部署/变量/执行记录（bun 静态表适配）。
type FunctionRepo interface {
    CreateFunction(ctx context.Context, fn *Function) error
    GetFunction(ctx context.Context, projectID, functionID string) (*Function, error)
    ListFunctions(ctx context.Context, projectID string) ([]Function, error)
    UpdateFunction(ctx context.Context, fn *Function) error
    DeleteFunction(ctx context.Context, projectID, functionID string) error

    CreateDeployment(ctx context.Context, d *Deployment) error
    GetDeployment(ctx context.Context, functionID, deploymentID string) (*Deployment, error)
    ListDeployments(ctx context.Context, projectID, functionID string) ([]Deployment, error)
    UpdateDeployment(ctx context.Context, d *Deployment) error
    DeleteDeployment(ctx context.Context, functionID, deploymentID string) error

    SetVariables(ctx context.Context, projectID, functionID string, vars map[string]string) error
    GetVariables(ctx context.Context, projectID, functionID string) (map[string]string, error)

    CreateExecution(ctx context.Context, e *ExecutionRecord) error
    GetExecution(ctx context.Context, projectID, functionID, executionID string) (*ExecutionRecord, error)
    ListExecutions(ctx context.Context, projectID, functionID string, limit int) ([]ExecutionRecord, error)
    UpdateExecution(ctx context.Context, e *ExecutionRecord) error
    // RecoverOrphanExecutions 将停留 queued/building/running 超过 staleAfter 的记录
    // 标记为 failed（worker 启动对账，见 §5.5）。
    RecoverOrphanExecutions(ctx context.Context, staleAfter time.Duration) (int64, error)
    // PruneOldExecutions 清理超过 keepRecent 条的最新之外记录（保留策略）。
    PruneOldExecutions(ctx context.Context, functionID string, keepRecent int) error
}

type Function struct {
    ID, ProjectID, Name, Runtime, Entrypoint string
    TimeoutSeconds int
    Spec string
    Enabled bool
    CreatedAt, UpdatedAt time.Time
}

type Deployment struct {
    ID, FunctionID, ProjectID string
    Size int64
    Status string // pending/building/ready/failed
    Error string
    CreatedAt, UpdatedAt time.Time
}

type Variable struct { ID, FunctionID, Key, Value string }

// ExecutionRecord 是执行记录实体；命名区别于 executor 入参 Execution（见 §4.3）。
type ExecutionRecord struct {
    ID, FunctionID, ProjectID, DeploymentID string
    Status string
    Response, Stdout, Stderr, Error string
    ResponseTruncated, StdoutTruncated, StderrTruncated bool
    StatusCode int
    DurationMS int64
    CreatedAt, UpdatedAt time.Time
}
```

### 4.2 `internal/domain/shared/ports.go`（新建，roadmap §3.6 的 Queue 端口）

```go
package shared

// Queue 是异步任务队列端口（MVP：Redis List BRPOP 实现）。
type Queue interface {
    Enqueue(ctx context.Context, queue string, payload []byte) error
    // Dequeue 阻塞等待任务；timeout<=0 时阻塞直到有任务或 ctx 取消。
    Dequeue(ctx context.Context, queue string, timeout time.Duration) ([]byte, error)
}
```

### 4.3 命名冲突

`executor.Execution`（执行入参）与 repo 执行实体重名：**repo 实体统一用
`ExecutionRecord`**（§4.1 已用），executor 的沿用 `functions.Execution` 不变。

---

## 5. 分层实现规格

### 5.1 proto：`proto/server/v1/functions.proto`（新建）

service 级 `option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_API_KEY };`
参照 `apikeys.proto`：import `google/api/annotations.proto`、`shared/v1/authz.proto`、
`shared/v1/common.proto`、`google/protobuf/timestamp.proto`。

**共 16 个方法**（评审修正计数），**每个方法都必须带 `google.api.http` 注解**：

```proto
service FunctionsService {
  option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_API_KEY };

  rpc ListRuntimes(shared.v1.Empty) returns (ListRuntimesResponse) {
    option (google.api.http) = { get: "/v1/server/functions/runtimes" };
  }
  rpc ListSpecifications(shared.v1.Empty) returns (ListSpecificationsResponse) {
    option (google.api.http) = { get: "/v1/server/functions/specifications" };
  }
  rpc CreateFunction(CreateFunctionRequest) returns (Function) {
    option (google.api.http) = { post: "/v1/server/functions", body: "*" };
  }
  rpc ListFunctions(shared.v1.ListRequest) returns (ListFunctionsResponse) {
    option (google.api.http) = { get: "/v1/server/functions" };
  }
  rpc GetFunction(GetFunctionRequest) returns (Function) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}" };
  }
  rpc UpdateFunction(UpdateFunctionRequest) returns (Function) {
    option (google.api.http) = { patch: "/v1/server/functions/{function_id}", body: "*" };
  }
  rpc DeleteFunction(GetFunctionRequest) returns (shared.v1.Empty) {
    option (google.api.http) = { delete: "/v1/server/functions/{function_id}" };
  }
  rpc CreateDeployment(CreateDeploymentRequest) returns (Deployment) {
    option (google.api.http) = { post: "/v1/server/functions/{function_id}/deployments", body: "*" };
  }
  rpc ListDeployments(GetFunctionRequest) returns (ListDeploymentsResponse) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}/deployments" };
  }
  rpc GetDeployment(GetDeploymentRequest) returns (Deployment) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}/deployments/{deployment_id}" };
  }
  rpc DeleteDeployment(GetDeploymentRequest) returns (shared.v1.Empty) {
    option (google.api.http) = { delete: "/v1/server/functions/{function_id}/deployments/{deployment_id}" };
  }
  rpc SetVariables(SetVariablesRequest) returns (Variables) {
    option (google.api.http) = { put: "/v1/server/functions/{function_id}/variables", body: "*" };
  }
  rpc GetVariables(GetFunctionRequest) returns (Variables) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}/variables" };
  }
  rpc CreateExecution(CreateExecutionRequest) returns (Execution) {
    option (google.api.http) = { post: "/v1/server/functions/{function_id}/executions", body: "*" };
  }
  rpc ListExecutions(GetFunctionRequest) returns (ListExecutionsResponse) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}/executions" };
  }
  rpc GetExecution(GetExecutionRequest) returns (Execution) {
    option (google.api.http) = { get: "/v1/server/functions/{function_id}/executions/{execution_id}" };
  }
}

message Function {
  string id = 1; string project_id = 2; string name = 3; string runtime = 4;
  string entrypoint = 5; int32 timeout_seconds = 6; string spec = 7; bool enabled = 8;
  google.protobuf.Timestamp created_at = 9; google.protobuf.Timestamp updated_at = 10;
}
message RuntimeInfo { string id = 1; string name = 2; string entrypoint = 3; }
message SpecificationInfo { string id = 1; string cpu = 2; string memory = 3; }
message Deployment {
  string id = 1; string function_id = 2; int64 size = 3; string status = 4;
  string error = 5; google.protobuf.Timestamp created_at = 6; google.protobuf.Timestamp updated_at = 7;
}
message Variables { repeated Variable variables = 1; }
message Variable { string key = 1; string value = 2; }
message Execution {
  string id = 1; string function_id = 2; string deployment_id = 3; string status = 4;
  string response = 5; string stdout = 6; string stderr = 7; int32 status_code = 8;
  int64 duration_ms = 9; string error = 10;
  bool response_truncated = 11; bool stdout_truncated = 12; bool stderr_truncated = 13;
  google.protobuf.Timestamp created_at = 14; google.protobuf.Timestamp updated_at = 15;
}
message CreateFunctionRequest {
  string id = 1; string name = 2; string runtime = 3; string entrypoint = 4;
  optional int32 timeout_seconds = 5; optional string spec = 6; optional bool enabled = 7;
}
message UpdateFunctionRequest {
  string function_id = 1; optional string name = 2; optional string entrypoint = 3;
  optional int32 timeout_seconds = 4; optional string spec = 5; optional bool enabled = 6;
}
message GetFunctionRequest { string function_id = 1; }
message CreateDeploymentRequest { string function_id = 1; bytes code = 2; }  // code 为 zip，≤1MiB 走此通道
message GetDeploymentRequest { string function_id = 1; string deployment_id = 2; }
message CreateExecutionRequest {
  string function_id = 1; optional string deployment_id = 2;  // 缺省用最新 ready deployment
  string data = 3; optional bool async = 4;
}
message GetExecutionRequest { string function_id = 1; string execution_id = 2; }
message SetVariablesRequest { string function_id = 1; repeated Variable variables = 2; }  // 全量替换
message ListRuntimesResponse { repeated RuntimeInfo runtimes = 1; }
message ListSpecificationsResponse { repeated SpecificationInfo specifications = 1; }
message ListFunctionsResponse { repeated Function functions = 1; shared.v1.ListResponseMeta meta = 2; }
message ListDeploymentsResponse { repeated Deployment deployments = 1; }   // MVP 不分页
message ListExecutionsResponse { repeated Execution executions = 1; }      // MVP 不分页（返回最近 100 条）
```

生成：`task generate-proto`（buf 自动纳入，无需改 buf.yaml/buf.gen.yaml）。
生成后必须把 `serverv1.File_server_v1_functions_proto` 加入
`internal/infra/server/grpc.go` 的 `collectMethodsByAccess` 调用（约 :50-64），
否则 `assertRegisteredMethodsHaveAuthz` 启动失败。

### 5.2 gRPC handler：`internal/api/servergrpc/functions.go`（新建）

参照 `apikeys.go` 模式。注入 `*appfunctions.Functions`。所有方法先取 `projectID`
（`contexts.Principal`，缺失返回 `Unauthenticated`），再调 use-case；错误直接透传。
读写方法均需 `contexts.WithAuditResource`（风格 `"functions/{id}/executions/{eid}"`）。

**同步执行超时（评审定案）**：

- **限制同步执行 `timeout_seconds` ≤ 30s**：`CreateFunction/UpdateFunction` 校验
  `timeout_seconds` 1..300；但同步执行（async=false）时若 `timeout_seconds > 30` 返回
  `InvalidArgument`（"timeout_seconds exceeds 30s for synchronous execution, use async"）。
  原因：grpc-gateway 挂载的 HTTP server 有 `WriteTimeout`（config `server.http.timeout`，
  模板默认 60s），超过会被网关截断；限制 30s + 构建余量可保证 gateway 路径可行。
- handler 层兜底：`ctx, cancel := context.WithTimeout(ctx, fn.TimeoutSeconds+60s)`（gRPC
  直连路径需要），超时映射 `DeadlineExceeded`（HTTP 504）。
- `CreateDeployment` 的 `bytes code` 仅支持 **≤ 1MiB** 的小代码包（gRPC 默认
  max recv 4MiB，gateway base64 膨胀 1.33x）；大包走 multipart（§5.6）。
  同时在 `NewGRPCServer` 增加 `grpc.MaxRecvMsgSize(8 << 20)`（lynx
  `WithServerOptions` 透传），覆盖 1MiB 源 + base64 膨胀。

### 5.3 app use-case：`internal/app/functions/`（扩展）

现有 `functions.go`（`Functions` + `Execute` + `RuntimeImage`）保留，新增：

- `internal/app/functions/management.go`：
  - `CreateFunction/ListFunctions/GetFunction/UpdateFunction/DeleteFunction`。
  - ID 校验复用 `pkg/idgen`；runtime 必须存在于 `runtimes` 静态表（§5.8）；
    `timeout_seconds` 1..300（同步执行另限 ≤30s，见 §5.2）；`spec` 必须存在于
    specifications。
  - `entrypoint` **MVP 仅占位**：字段保留并落库，但执行入口固定为 `index.js` 的
    `main`（node）/ `main.py` 的 `main`（python）。文档注明（§8）。
- `internal/app/functions/deployments.go`：
  - `CreateDeployment`：接收 zip 字节流 → 大小上限 50 MiB（multipart 路径限流）→
    落库 `status=pending` → **同步构建**（调用 executor.Build，见 §5.4）→
    成功 `ready` / 失败 `failed`（构建日志截断 64KB 写入 error）→ 返回。
    - zip 存本地临时目录 `os.TempDir()/torchwood-functions/{projectID}/{functionID}/{deploymentID}.zip`
      （**单机部署假设**，§2）。
    - 构建失败时清理本地 zip 与可能残留的镜像（幂等）。
  - `ListDeployments/GetDeployment/DeleteDeployment`：删除顺序
    **先 DB 级联删除 → 再 `docker image rm` → 最后删本地 zip**（全部幂等，
    失败仅记日志），避免进行中构建/执行读到半删除状态。
- `internal/app/functions/executions.go`：
  - `CreateExecution`：
    1. 取 function（enabled=false 拒绝执行）；选定 deployment（显式且 `ready`，
       或最新 `ready`；无 ready 部署 → `FailedPrecondition` "no ready deployment"）。
    2. 校验 `data` ≤ 64KB 且为合法 JSON（`InvalidArgument`）；合并 env vars
       总量 ≤ 32KB。
    3. 创建 `ExecutionRecord{status: queued}` **先落库**。
    4. `async=false` → 同步执行（占执行信号量 → executor → 写回
       completed/failed + duration + 输出截断）。
    5. `async=true` → 入队 `torchwood:queue:functions-executions`
       （payload = `{"execution_id":...}`）；**入队失败 → 记录标记 failed**
       （error="enqueue failed"）并返回错误。
    6. 写回后 `PruneOldExecutions(functionID, 100)`。
  - `GetExecution/ListExecutions`（返回最近 100 条）。
  - **执行信号量（评审补充）**：同步执行与 worker 共用 `Semaphore{build:4, run:16}`
    （包级变量或 config 注入）；超限返回 `ResourceExhausted`。
  - **输出截断**：stdout/stderr/response 落库前各截断 64KB，超限置对应
    `*_truncated=true`（§3 字段）。

### 5.4 infra：真实 Docker 执行器

替换 `internal/infra/functions/docker.go` 的 stub 为真实实现（改造同文件，删除 stub
内容）。新增依赖：`github.com/docker/docker`（`go get` 后 `go mod tidy`）。

**环境准备（评审修正）**：

- Docker host 从 `cfg.Functions.Docker.Host`（模板默认 unix socket）；
  `client.NewClientWithOpts(client.WithHost(host))`。
- **镜像 registry 必须小写**：模板 `configs/config.yaml.template` 的
  `functions.docker.registry` 改为 `torchwood-funcs`（现为 `Torchwood-funcs`，大写
  会导致 `docker build -t` 失败）；`RuntimeImage` 兜底默认值同样改小写
  （`internal/app/functions/functions.go:70` 的 `"Torchwood"` → `"torchwood-funcs"`）。
  registry 为空时镜像不带前缀（仅本地）。
- **网络**：`docker run --network` 使用 `cfg.Functions.Docker.Network`；执行器启动时
  `docker network inspect` 不存在则 `create`（名称为配置值，driver bridge）；
  模板默认值改 `torchwood-functions`（小写）。compose 无需预建网络。

**Build**（`Build(ctx, functionID, deploymentID, zipPath)`）：

1. 解压 zip（`archive/zip`，防 zip 炸弹与路径穿越）：
   - 条目数 ≤ 1000、单条 ≤ 100 MiB、总解压 ≤ 200 MiB（超限 `InvalidArgument`）。
   - **zip slip 防护**：每个条目 `filepath.Clean` 后必须仍位于解压根目录内，否则拒绝；
     拒绝 symlink 条目。
   - 校验入口文件存在：`index.js`（node）/ `main.py`（python）。
2. 生成 Dockerfile（**data 经环境变量传递，禁止拼接进命令**）：
   - node：`FROM node:18-alpine\nWORKDIR /app\nCOPY . .\nCMD ["node","-e","const {main}=require('./index');Promise.resolve(main(JSON.parse(process.env.TW_DATA||'{}'))).then(r=>console.log(JSON.stringify(r))).catch(e=>{console.error(e);process.exit(1)})"]`
   - python：`FROM python:3.11-alpine\nWORKDIR /app\nCOPY . .\nCMD ["python","-c","import json,os,main;r=main.main(json.loads(os.environ.get('TW_DATA','{}')));print(json.dumps(r))"]`
3. `docker build -t {registry}/func-{functionID}-{deploymentID} {ctx}`；
   构建日志截断 64KB，失败时写入 deployment.error。

**Run**（`Run(ctx, exec domainfunctions.Execution)`）：

- 容器安全基线（评审补全）：
  `docker run --rm --network {network|none} --memory {spec.memory} --cpus {spec.cpu}
   --cap-drop ALL --security-opt no-new-privileges --pids-limit 512 --read-only
   --tmpfs /tmp --stop-timeout 5 -e TW_DATA='{data}' [-e {user vars...}]
   {image}`
  - network 为空 → `--network none`。
  - Dockerfile 加 `USER node`（node 镜像自带 `node` 用户）/ `USER 1000`（python，
    需 `adduser` 或直接数值 uid）。
- 收集 stdout/stderr：`ContainerAttach`（或 Logs）。
- 超时：`context.WithTimeout(fn.TimeoutSeconds)`；超时后 `ContainerStop` +
  `ContainerRemove`，返回 `DeadlineExceeded`（HTTP 504）。
- 返回 `domainfunctions.ExecutionResult{StatusCode, Stdout, Stderr, Response,
  DurationMS}`（Response = stdout 末行若为合法 JSON 则原样，否则空）。
- 镜像清理：DeleteDeployment/DeleteFunction 时 `docker image rm`（幂等，失败记日志）。

**并发**：executor 不做内部限流；由 app 层信号量（§5.3）控制 build ≤4 / run ≤16。

**可测性**：`NewDockerExecutor(cfg)`；`Build`/`Run` 为独立方法，便于单测注入。

### 5.5 队列 + cmd/worker

- `internal/infra/queue/redis_queue.go`（新建）：实现 `shared.Queue`。
  `Enqueue` → `LPUSH`；`Dequeue` → `BRPOP(queue, timeout)`。依赖 `*redis.Client`
  （`internal/infra/clients/database.go` 的 `NewRedis`）。队列名常量
  `"torchwood:queue:functions-executions"`。
- `cmd/worker/`（新建 `main.go` + `provides.go` + `wire.go`，参照 `cmd/server/`）：
  - 启动：godotenv → 加载 config → 连 Redis + Postgres（bun repo）→ 构造
    `appfunctions.Functions`（executor + repo + queue）。
  - **启动对账（评审补充）**：先 `repo.RecoverOrphanExecutions(ctx, 1h)` —— 将停留
    `queued/building/running` 超过 1h 的记录标 `failed`（error="worker restarted"）。
    兜底：Redis 重启丢任务、worker 崩溃孤儿。
  - 消费循环：`Queue.Dequeue(ctx, queue, 1s)` 轮询（配合 `signal.NotifyContext`
    优雅退出）；**单进程 N=4 goroutine 并发消费**（任务间无顺序依赖，天然并行；
    BRPOP 多消费者互斥由 Redis 保证）。
  - 每任务：解析 `{execution_id}` → 加载 execution/function/deployment →
    deployment 非 `ready` 先补 `Build`（§2）→ 状态 `building`→`running` →
    `executor.Execute`（超时=fn.TimeoutSeconds）→ 写回 completed/failed
    （stdout/stderr/response/duration/截断标志）。
  - 写回 UPDATE 影响 0 行（记录已被删）静默忽略。
- Taskfile 新增：`worker`（`go run ./cmd/worker`）、`wire-worker`。
- 镜像产出：`task build` 追加 `go build -o ./bin/ ./cmd/worker`（同一二进制产物目录）。

### 5.6 HTTP multipart 上传：`internal/api/serverhttp/functions_handler.go`（新建）

参照 `file_handler.go` 的 `Register` 模式（grpc-gateway `runtime.ServeMux`）：

- `POST /v1/server/functions/{functionId}/deployments/code`：multipart 字段 `code`
  （zip 文件，`MaxBytesReader` 50 MiB）→ 校验 zip 魔数 `PK\x03\x04` →
  `CreateDeployment` → 返回 deployment JSON。
- 鉴权复用 `auth.Validator` + API key scope 检查（`FunctionsService/CreateDeployment`
  → `functions.write`），参照 `file_handler.go` 的 `authorize`。
- **结构化访问日志**（`logOp` 风格，参照 `file_handler.go:69-91`）：op=
  deployment-upload，含 function_id/deployment_id/ip/actor。
- 在 `internal/infra/server/grpc_gateway.go` 的 gateway mux 上 `Register`。

### 5.7 静态表仓库：`internal/infra/bun/bunrepo/function_repo.go`（新建）

bun 实现 `domain/functions.FunctionRepo`：CRUD + project_id 过滤 + deployments/
variables/executions 关联操作。`SetVariables` 用「DELETE WHERE function_id + 批量
INSERT」（单事务）；`RecoverOrphanExecutions`/`PruneOldExecutions` 为原生 SQL。
注册：`internal/infra/provides.go` 添加
`wire.Bind(new(functions.FunctionRepo), new(*bunrepo.FunctionRepo))`（参照现有
bind 范式 :32-39）。

### 5.8 静态资源：runtimes / specifications

`internal/app/functions/runtimes.go`（新建）：

```go
// node-18.0 → node:18-alpine，入口 index.js；python-3.11 → python:3.11-alpine，入口 main.py
var runtimes = []functions.RuntimeInfo{...}
var specifications = []functions.SpecificationInfo{
    {ID: "shared-1x", CPU: "0.5", Memory: "256m"},
    {ID: "shared-2x", CPU: "1", Memory: "512m"},
}
```

### 5.9 gRPC 挂载与 scope

- `internal/infra/server/grpc.go`：`NewGRPCServer` 增加 `*servergrpc.FunctionsService`
  形参（:28-46），`collectMethodsByAccess` 追加 `serverv1.File_server_v1_functions_proto`
  （:50-64），注册 `serverv1.RegisterFunctionsServiceServer`（:92-104）；
  `WithServerOptions` 增加 `grpc.MaxRecvMsgSize(8 << 20)`。
- `internal/api/provides.go`：新增 `servergrpc.NewFunctionsService` provider。
- `internal/app/provides.go`：新增 use-case 构造函数（Functions 需要 repo/executor/queue）。
- `pkg/grpc/interceptor/apikey_scope.go`：登记 **16 个方法**（§5.1 清单）：
  - read：ListRuntimes、ListSpecifications、ListFunctions、GetFunction、
    ListDeployments、GetDeployment、GetVariables、ListExecutions、GetExecution；
  - write：CreateFunction、UpdateFunction、DeleteFunction、CreateDeployment、
    DeleteDeployment、SetVariables、CreateExecution。
- `cmd/server/wire_gen.go`：`task wire-all` 重新生成。

### 5.10 Console

- `console/src/api/functions.ts`（新建）：类型 + 全套 CRUD client（参照 `api/storage.ts`）。
- `console/src/routes/functions/pages.tsx`（新建，参照 `routes/storage/pages.tsx`）：
  - `FunctionsListPage`：函数列表（名称/runtime/状态），新建/删除。
  - `FunctionDetailPage`：基本信息编辑（runtime/entrypoint/timeout/spec/enabled）、
    variables 编辑（key/value 行编辑 + 全量保存）、deployments 列表 + zip 上传
    （`<Input type="file" accept=".zip">`）、执行区（data JSON 输入 +
    「异步执行」按钮——**默认异步**，规避 gateway WriteTimeout）、执行历史列表
    （状态徽标 + 点击查看 stdout/stderr/response，截断标志展示提示）。
- 路由注册 `console/src/App.tsx`（`/console/functions`、`/console/functions/:functionId`）；
  侧边栏 `console/src/components/Layout.tsx` Develop 分组加 Functions 项；
  `console/src/components/PageHeader.tsx` 标题映射。

---

## 6. 实现顺序（建议）

| 步骤 | 内容 | 验证 |
|------|------|------|
| 1 | proto + generate + grpc.go 挂载（含 MaxRecvMsgSize）+ apikey_scope 登记 | `task generate-proto`、`go build ./...` |
| 2 | migration + bun model + bunrepo + domain 端口 + shared.Queue | `go vet ./...` |
| 3 | app use-case（management/deployments/executions/runtimes，含信号量/截断/保留策略） | `go test ./internal/app/functions/...` |
| 4 | infra：docker executor（真实）+ queue + wire | `go test ./internal/infra/functions/...` |
| 5 | gRPC handler + provides + wire-all + multipart handler | `go build ./...`、`task wire-all` |
| 6 | cmd/worker + Taskfile（含 build 产物） | `go build ./cmd/worker` |
| 7 | Console | `task console-build` |
| 8 | 测试补齐 + 全量验证 | 见 §7 |

每步完成跑 `gofmt -l .`（必须空）+ `go vet ./...`。注意先修改
`configs/config.yaml.template` 的 registry/network 默认值（§5.4）与
`internal/app/functions/functions.go` 的默认 registry 兜底。

---

## 7. 测试与验证

- **app 层单元测试**（mock `FunctionRepo` + mock `Executor` + mock `Queue`）：
  - CreateFunction 校验（runtime/spec 非法 → InvalidArgument；timeout>30 同步拒绝）；
  - CreateExecution 同步路径（executor 结果写回 + 截断）与异步路径（先落库后入队；
    入队失败标 failed）；无 ready 部署 → FailedPrecondition；data>64KB → InvalidArgument；
  - 超时（mock executor 返回 DeadlineExceeded）写回 failed；
  - 信号量超限 → ResourceExhausted。
- **infra queue 测试**：miniredis（参照 `ratelimit_redis_test.go`）：Enqueue/Dequeue 往返。
- **docker executor 集成测试**（`internal/infra/functions/docker_integration_test.go`）：
  - 前置：`TORCHWOOD_RUN_DOCKER_TESTS=1` 且 `docker info` 成功，否则 `t.Skip`；
  - 覆盖：zip 包 build+run（node hello 函数，断言 stdout 含输出）、超时清理
    （timeout=1s sleep 函数 → 报错且 `docker ps -a` 无该镜像残留）、zip slip 拒绝、
    非法 zip 拒绝。
- **gRPC handler 测试**：`internal/api/servergrpc/functions_test.go`（mock use-case，
  projectID 校验与错误透传）。
- **CI（`.github/workflows/ci.yml`）**：backend job 直接设置
  `TORCHWOOD_RUN_DOCKER_TESTS=1`（ubuntu runner 自带 daemon），并在测试前
  `docker pull node:18-alpine python:3.11-alpine` 预拉镜像控制时长。
- **全量验证**：`go test ./...`（本地 .env 提供 TORCHWOOD_TEST_DATABASE_SOURCE）、
  `task lint`、`task build`（产出 server + worker）、`task console-build`。

---

## 8. 范围外与显式声明（明确不做）

- 执行任务重试 / 死信队列 / ack 语义（MVP 失败即 failed；worker 崩溃由启动对账兜底，
  §5.5）。
- 构建与执行的持久化队列（Redis 重启丢任务；execution 记录仍在 DB，靠对账标记 failed）。
- Runtime 镜像预构建与离线缓存（直接拉官方基础镜像）。
- VCS 集成、自动部署、Webhooks 事件触发（P2）。
- 断点续传/分片上传（代码包 ≤ 50 MiB）。
- **独立构建队列**（对 roadmap「构建队列」的 MVP 偏离：`CreateDeployment` 同步构建，
  见 §2 定案）。
- **`DeleteExecution`**（roadmap 仅列 POST/GET；执行历史由保留策略裁剪，§3）。
- **ListExecutions/ListDeployments 分页**（MVP 返回最近 100 条 / 全部）。
- **`entrypoint` 自定义**（MVP 固定 `index.js`/`main.py` 的 `main`，字段仅占位）。
- **Variables 加密**：MVP 明文存储且 `functions.read` 可读；风险已声明
  （env 常含密钥，后续可做 AES 加密）。容器内 env 可见属主机管理员权限，可接受。
- 容器资源配额的多租户隔离调优（--memory/--cpus/--cap-drop ALL/no-new-privileges/
  --pids-limit/--read-only/--tmpfs 为基线）。

---

## 9. 关键坑（实现时必须注意）

1. **authz fail-closed**：16 个新方法无注解或未登记 scope → 启动失败/403。
   生成 proto 后立即改 `grpc.go` 与 `apikey_scope.go`。
2. **Wire 剪枝**：`NewFunctions` 需要 handler 消费才会进 wire_gen；新增 handler +
   `task wire-all` 后检查 `cmd/server/wire_gen.go` 出现 functions 标识符。
3. **executor.Execution 与 repo 实体重名**：repo 用 `ExecutionRecord`。
4. **同步执行超时**：`timeout_seconds > 30` 禁止同步（§5.2）；handler
   `WithTimeout(fn+60s)`；容器 `--stop-timeout 5`；超时 → `DeadlineExceeded`/504。
5. **zip 炸弹 + zip slip**：条目数/单条/总大小上限 + `filepath.Clean` 前缀校验 +
   拒绝 symlink（§5.4）。
6. **Dockerfile 注入**：data 经 `TW_DATA` 环境变量传递，禁止拼接进命令；
   `data` ≤ 64KB（execve 单变量硬上限），env 总量 ≤ 32KB。
7. **registry/network 大小写与存在性**：registry 必须小写；network 不存在时自动
   create（§5.4）；先改 `configs/config.yaml.template` 与 functions.go 兜底值。
8. **FK 级联**：executions.deployment_id 与各表均 `ON DELETE CASCADE`（§3）。
9. **输出截断**：stdout/stderr/response 落库前 64KB 截断 + 标志（§5.3）。
10. **并发控制**：app 层信号量 build≤4/run≤16 → 超限 `ResourceExhausted`（§5.3）。
11. **worker 对账**：启动时 `RecoverOrphanExecutions(1h)`（§5.5）。
12. **Windows 本地开发**：daemon 测试自动跳过；Docker 路径在 Linux（CI/容器）验证。
13. **Console 改动后**：先 `task console-build` 再 `task build`（Go embed）。
14. **gRPC 消息上限**：`MaxRecvMsgSize(8MiB)`；deployment `code` ≤1MiB 走 gRPC，
    大包走 multipart（§5.2/§5.6）。
