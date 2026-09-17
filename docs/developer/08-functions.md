# 08 函数：执行器、并发与异步

面向后端开发者：Functions 子系统的执行模型（常驻 runner + dispatcher 分发）、构建流程、鉴权、触发器（HTTP / cron / 事件）、客户端调用面与异步 worker。所有函数执行统一经 dispatcher 分发。

> 源码锚点：`internal/domain/functions/`、`internal/infra/functions/`（分发客户端）、`dispatcher/`、`packer/`（git 打包服务）、`internal/app/functions/`、`pkg/semaphore/`、`worker/`。
> 阅读顺序建议：`06-databases.md`（三层与 outbox）→ 本章 → `09-api-guide.md`（新增 RPC）。

## 1. 架构

```
gRPC FunctionsService (proto/server/v1/functions.proto) ─→ app/functions ─┬→ FunctionRepo（bun：functions/function_deployments/function_variables/function_executions）
                                                                           ├→ Executor（DispatcherExecutor → dispatcher 内网 HTTP，internal/infra/functions/dispatcher_client.go）
                                                                           └→ Queue（Redis Stream torchwood:queue:functions-executions）─→ worker（4×XREADGROUP）
HTTP multipart FunctionsHandler（POST .../deployments/code，≤50MiB）────────┘
```

- **执行统一经 dispatcher 分发**（`dispatcher/`，独立进程、平台唯一 docker.sock 持有方）。v1 docker 执行器（每请求一容器、server/worker 进程内挂 docker.sock）已于 2026-09 移除。MVP 为单机形态，部署包落在共享文件系统 `os.TempDir()/torchwood-functions/<project>/<function>/<deployment>.zip`；多机部署需对象存储承载 zip。
- 四张表由 projectschema 迁移维护（`internal/infra/projectschema/migrations/000003_functions`）；执行身份 / 池策略 / 触发器 / 客户端调用 / 并发分别为 000013 / 000014 / 000015 / 000016 / 000017 / 000018。模型与端口在 `internal/domain/functions/`（`Execution` / `Deployment` / `Repository` / `Executor`）。

### 1.1 运行限制速查

| 限制 | 数值 | 详见 |
|---|---|---|
| invoke 上行 `data` | ≤32KB（JSON object；`data+env ≤32KB`）；触发器封套通道放宽至 ≤1MB | §4 |
| 响应大小 | response / stdout / stderr 各 **≤64KB 截断**（`maxOutputBytes`，`truncated` 标记）。dispatcher 内部 1MiB 是读封套的缓冲上限，不是对调用方的承诺 | §4、§11 |
| 执行超时 | 每函数可配 **[1,300]s，缺省 15s**；**同步调用上限 30s**（超出走异步） | §2、§4、§14 |
| `concurrency` 语义 | **单实例并发上限（1..16，默认 1）**，不是全局串行：单实例一次跑 `concurrency` 个请求；池可在无空闲实例时冷启动扩到 `max_instances` 多实例并行。真正的全局闸门是 dispatcher 池上限（`max_instances` + 有界排队 429）与每用户并发 2 | §4.3、§4.3.1、§14 |
| 部署包 | zip ≤50MiB；解压 ≤1000 条 / 单条 ≤100MiB / 总量 ≤200MiB（git 源物化包条目放宽至 ≤5000，§3.4） | §3、§3.4 |

## 2. 写方法与鉴权

`FunctionsService` 共 14 个 RPC（`ACCESS_SERVER` 默认），其中 **8 个写方法**在用例层以 `appshared.RequireServerPrincipal` 纵深防御：

| RPC | HTTP | 写语义 |
|---|---|---|
| `CreateFunction` | `POST /v1/server/functions` | `timeout_seconds∈[1,300]`，缺省 `shared-1x/15s`；可带 `declared_scopes`（§4.2） |
| `UpdateFunction` | `PATCH .../{function_id}` | `optional name/entrypoint/timeout/spec/enabled`（scopes 走独立 RPC） |
| `SetFunctionScopes` | `PUT .../{function_id}/scopes` | 全量替换 `declared_scopes`；空集 = 撤销全部平台访问 |
| `DeleteFunction` | `DELETE .../{function_id}` | 级联删部署 + RemoveImage + 删 zip（幂等） |
| `CreateDeployment` | `POST .../{function_id}/deployments` | `source` oneof：gRPC `bytes code` ≤1MiB（大包走 `POST .../deployments/code` multipart ≤50MiB）或 `git`（`GitSource`，§3.4） |
| `DeleteDeployment` | `DELETE .../{function_id}/deployments/{deployment_id}` | |
| `SetVariables` | `PUT .../{function_id}/variables` | 全量替换，明文存储（`function_variables`） |
| `CreateExecution` | `POST .../{function_id}/executions` | 同 / 异步二选一（§4） |

`RequireServerPrincipal` 允许 System / PlatformAdmin / keys；`viewer` 等细粒度由拦截器按 proto `method_auth` 的 `admin_roles` 门禁把关（Functions 写方法为 delegated_platform 档 = admin/owner，见 `authz-matrix.md`）。读方法（List* / Get* / ListRuntimes / Specifications / GetVariables）不强制写角色。

## 3. 运行时与构建

**运行时**：`node-18.0`（`index.js` 导出 `main`/`fetch`，node:18-alpine）与 `go-1.26`（Go 一期，golang:1.26-alpine 多阶段构建 + 平台生成 bootstrap，见 §3.1）。python-3.11 探测保留但构建期明确报错（python runner 未实现），历史 python zip 在构建期明确报错。规格：`shared-1x`（0.5CPU/256MB）、`shared-2x`（1CPU/512MB）。

**node runner 模板**（常驻执行模型模板，CMD = 平台 runner）：

```dockerfile
FROM node:18-alpine
WORKDIR /app
COPY . .; USER node
ENV TW_RUNNER_PORT=18080
CMD ["node",".tw-runner.js"]
```

**平台代装依赖**（node 运行时）：zip 根含 `package.json` 且 `dependencies` 非空时，构建改用经典分层模板——依赖由平台在构建期安装，用户代码包不再携带 `node_modules`：

```dockerfile
FROM node:18-alpine
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev --ignore-scripts
COPY . .
USER node
ENV TW_RUNNER_PORT=18080
CMD ["node",".tw-runner.js"]
```

规则：

- **lockfile 强制**：有 `dependencies` 但缺 `package-lock.json` → 构建失败，错误信息明示提交 lockfile。无锁安装不可复现，与"构建是平台确定性操作"不变量对齐。
- **`node_modules` 拒收**：代码包中任意条目路径第一段为 `node_modules` → 构建失败（跨平台二进制不兼容）。CLI deploy 打包时已同步排除 `node_modules` / `.git`。
- **`--ignore-scripts` 恒定**（一期不提供 opt-in）：不变量"构建期不执行用户代码 / 第三方脚本"——npm 生命周期脚本（postinstall）可执行任意代码。代价：依赖原生编译（node-gyp）或 postinstall 下载二进制的包不可用（如 esbuild / swc 安装版——函数执行时报"找不到可执行文件"即此原因，改用纯 JS 等价物或 WASM 构建）。残余风险（lockfile 为用户可控输入、npm 解析器漏洞）经 lockfile integrity hash 固定 + 构建容器 hardening 兜底；构建出网白名单后置（一期不限制，registry 拉包必需）。
- **层缓存加速**：`package.json` / lockfile 不变的重新部署直接命中 Docker 层缓存，跳过 `npm ci`，只有代码层重建。
- 探测与拒收实现在 zip 解压校验层（`extractZipWithLimits`，与 zip slip / 符号链接校验同处逐条判定）；部署源探测产出 `SourceContents`（runtime 判定 + node/go 依赖标记）；模板决策在 `runner.DockerfileFor`。

**构建流程**（`internal/app/functions/deployments.go`）：校验 zip 魔数 `PK\x03\x04` + 50MiB 限制 → 落库 `pending` → 占构建信号量 → `building` → zip base64 内联经 dispatcher `/v1/dispatch/builds` 构建（解压 ≤1000 条 / 单条 ≤100MiB / 总量 ≤200MiB，拒绝符号链接与 zip slip，拒收 node_modules；探测优先级 `index.js` > `go.mod` > `main.py`，混装按 node 不报错；**runtime 对账**：探测结果 ≠ `fn.runtime` 声明 → 构建期 InvalidArgument；go 分支平台生成 `twmain/` bootstrap，见 §3.1）→ `ready` / `failed`。镜像名 `<registry>/func-<fid>-<did>`（registry 默认 `torchwood-funcs`）。构建 ctx 与客户端断开解耦（`context.WithoutCancel` + `functions.dispatcher.build_timeout` 默认 5m 封顶），客户端断开后构建继续、状态照常落库，以 `deployment.status` 轮询兜底。

### 3.1 Go 运行时（go-1.26）

Go 一期（设计 `docs/design/functions-runtimes-and-sources.md` §1）：运行时表项 `go-1.26`（`internal/app/functions/runtimes.go`；entrypoint 字段 MVP 仅占位，升级 = 新 runtime ID、旧 ID 不日落）。构建段 `golang:1.26-alpine`（`CGO_ENABLED=0`），运行段 `alpine:3.22` + `ca-certificates`（函数 HTTPS 出访需要，基础 alpine 不自带）+ 非 root 数字 UID（65534）：

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
ENV GOFLAGS=-mod=readonly        # vendor 分支替换为 -mod=vendor 且免下一层
COPY go.mod go.sum* ./
RUN go mod download              # vendor 分支免此层
COPY . .
COPY twmain/ ./twmain/
RUN go build -trimpath -ldflags="-s -w" -o /out/tw-app ./twmain

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/tw-app /tw-app
USER 65534:65534
ENV TW_RUNNER_PORT=18080
CMD ["/tw-app"]
```

**用户契约**（纯标准库、零平台依赖）：

- **zip 根 = module 根**：go.mod 必须在 zip 根；根包为**非 main 的任意包名**（module path 取自 go.mod module 行，会被注入生成代码的 import 语句，含引号/空白等非法形态在探测期拒绝）。
- **入口双轨**（AST 探测 `gorunner.DetectEntry`，优先级同 node：Fetch 优先、Main 兜底；生成期二选一写死进 bootstrap——签名不符由编译器报错兜底，构建日志透传）：

  ```go
  // fetch 风格（优先）：HTTP 触发器封套还原为真 *http.Request，
  // 响应经 ResponseRecorder 捕获进 fetch 封套（status/headers/body 可控）。
  func Fetch(w http.ResponseWriter, r *http.Request)

  // main 风格（兜底）：data 为 TW_DATA JSON 反序列化；ctx 六键全 string。
  func Main(data map[string]any, ctx map[string]string) (any, error)
  // ctx 键：executionToken / apiBaseUrl / executionId / source /
  // invokingUserId / projectId（source 恒非空——缺省回落 "server"；
  // invokingUserId 空串 = 非用户触发，系统语义）。
  ```

  **fetch 风格身份通道**：Go fetch 风格还原的 `*http.Request` 除触发器封套 headers 外，还注入 x-tw-* 调用身份 header（`x-tw-execution-token` / `x-tw-execution-id` / `x-tw-source` / `x-tw-invoking-user-id` / `x-tw-project-id`，经 `r.Header.Get` 读取）——与 node fetch 风格的 env 参数**信息等价、通道不同**。`source` 缺省回落 `"server"`（与 main 风格 ctx / node env 同语义）；其余身份头分发侧未携带时不注入（`Get` 返回空串，等价 node env 空值）。`apiBaseUrl` 不走 header，函数直接读容器 env `TW_API_BASE_URL`（与 ctx 同源）。封套触发头与身份头撞名时身份头胜出（平台身份不得被触发方伪造）。

  两者皆无 → 构建期报错 `go function must export Fetch or Main`。探测口径与编译器同宽：文件枚举按 `go/build` MatchFile 评估（显式 `GOOS=linux`/`GOARCH=amd64` 上下文 + Cgo 关闭，`//go:build ignore` 与平台特定文件里的 Fetch 不会被误探测），显式跳过 `*_test.go`（export_test.go 暴露内部函数是 Go 社区常见写法）。

- **依赖确定性**（对齐 node「lockfile 强制」口径）：go.mod `require` 非空时须有 `go.sum` **或** `vendor/`，否则构建期报错（「检测到外部依赖但缺少 go.sum——go mod tidy 生成后提交」）。`vendor/` **受纳**（Go 官方钉版机制、源码形态无跨平台二进制问题，与拒收 node_modules 的理由本质不同）：模板切 `-mod=vendor` 且免 `go mod download` 层；无 vendor 走 `-mod=readonly` + 分层下载（go.mod/go.sum 不变命中 Docker 层缓存）。纯 stdlib 函数（require 空）合法无 go.sum——模板 `COPY go.mod go.sum* ./` 通配。
- **`CGO_ENABLED=0`**：cgo 依赖（如 mattn/go-sqlite3）不可用；`go build` 只编译不执行（Go modules 无 npm 生命周期脚本等价物），「构建期不执行用户代码」不变量强于 node。
- **`twmain/` 是平台保留目录**：构建期由平台生成（`runtime.go` = runner 协议实现 + `main.go` = `import user "<module path>"` 的编译入口），用户代码包内禁用——zip 携带路径第一段为 `twmain` 的条目构建期拒收并提示改名。
- **诚实契约差（与 node 的行为差异）**：
  - 封套 `stdout`/`stderr` **恒空串**（字段位保留）：Go 无 per-request console 捕获，函数日志直接写容器 stdout，经 `docker logs` 查看（node 的 per-request 日志分桶语义不适用）；
  - cgo 不可用（上条）；native 依赖须有纯 Go 等价物；
  - **构建画像**：Go 冷构建 = dispatcher 拉 `golang:1.26-alpine` 基础镜像（数百 MB，计入构建时间）+ 依赖下载 + 编译——全新环境的首个 Go 部署可逼近 `functions.dispatcher.build_timeout` 默认 5m，必要时调大该配置；go.mod/go.sum 不变的重新部署命中层缓存。

### 3.2 Runner 协议（契约镜像规范）

自 Go 一期起，`:18080` runner 契约升格为**公开契约**（为三期 BYO 契约镜像铺路）：Go 函数与未来的镜像源函数是同一契约的两类消费方。平台有两份同契约同版本的实现——node `runner/runner.js` 与 Go `runner/gorunner/assets/runtime.go.tmpl`（渲染进 twmain/），本节是协议规格，事实源 = 两份实现源码。

**监听与端点**（容器内 `:18080`，env `TW_RUNNER_PORT` 可配；仅 per-project 网络内可达）：

- `GET /_tw/health` → 200 `{"ok":true,"ready":true,"served":<n>,"inflight":<n>}`。ready 语义差异：node 入口运行期探测（模块加载失败**常驻 not-ready**，boot 探针超时回收）；Go 入口编译期绑定，**恒 ready**。
- `POST /` 调用双轨（同一函数包二选一生效，优先级 fetch > main）：
  - **main 风格**：请求 body = TW_DATA JSON → 200 `{"ok":true,"result":<入口返回值>,"stdout":"...","stderr":"..."}`（Go 实现的 stdout/stderr 恒空串，§3.1）；入口返回 error / 返回值不可 JSON 序列化 → 500 `{"ok":false,"error":...,"stdout","stderr"}`；body 非 JSON object → 400；body 上限 4MB（超限 413）；用户 panic 捕获为 500 错误封套（不杀常驻实例）。
  - **fetch 风格**：成功封套 `{"ok":true,"status":<函数 HTTP status>,"headers":{...},"body_base64":"...","truncated":bool,"stdout","stderr"}`——headers 过滤 hop-by-hop 与 content-length/host/date/server，其余（含 content-type）原样透传、同名多值逗号合并；body 超 64KB 截断标 `truncated`。无触发器封套时 Request = `POST http://function/`、body = TW_DATA（invoke / cron 语义）。
  - 未知路径 / 非 POST → 404 错误封套。
- **per-request 超时**：分发 header `x-tw-timeout-seconds`（缺省 30s）——到点回 500 封套（error 注明 timed out）并放弃等待；诚实声明（Lambda 同款）：用户代码可能残跑至实例回收，inflight 按请求生命周期释放。

**`x-tw-*` 分发 header 族**（dispatcher → runner，调用身份与控制面）：

| header | 语义 |
|---|---|
| `x-tw-execution-token` | 本次执行的短期平台凭证（main 风格 `ctx.executionToken`；node fetch 风格经 `env.EXECUTION_TOKEN` 参数；Go fetch 风格经还原 Request 的本 header 可达，见 §3.1 fetch 风格身份通道） |
| `x-tw-execution-id` | 平台执行 ID（日志关联） |
| `x-tw-source` | 调用来源 `client` / `http\|cron\|event:{trigger_id}` / `server`；缺省/为空回落 `server`，**恒非空** |
| `x-tw-invoking-user-id` | 触发用户 id（principal 注入不可伪造）；空串 = 非用户触发（系统语义） |
| `x-tw-project-id` | 执行所属项目 id |
| `x-tw-timeout-seconds` | per-request 超时秒数（缺省 30） |
| `x-tw-trigger-envelope` | 触发器封套元数据（base64 JSON `{method,path,raw_query,headers}`，不含 body，≤12KB）：存在时 fetch 风格还原真 `*http.Request`（`url = http://trigger{path}?{raw_query}`、headers 原样、body = 分发 body 本身）、main 风格重组 TW_DATA（与 §13.1 封套逐字段同构） |

**生命周期**：

- `TW_MAX_REQUESTS`（默认 1000；≤0 = 不限）：每响应后计数，达标主动退出（dispatcher 检测退出后补位）；
- SIGTERM → **drain**：停止接新请求（drain 中的 POST / 回 503）、等在途完成后退出；`TW_DRAIN_TIMEOUT_MS`（默认 10000）兜底强退。

**协议演进宪法（四条，必须遵守）**——契约公开后消费方分两类：平台可控（node runner / Go bootstrap，受同版本纪律约束）与平台不可控（用户 BYO 契约镜像，`RunnerTemplateVersion` 的「重建即升级」对其无强制力）。因此：

1. **只加不改不删**：既有端点 / 封套字段 / header 语义冻结；
2. **未知 `x-tw-*` header 镜像侧必须忽略**（分发侧可先于镜像侧演进）；
3. **封套新字段必须可选**（消费方防御式解析，未知/缺失字段不致命）；
4. **分发侧（dispatcher）对响应封套的解析必须防御式**（未知/缺失字段不致命）。

**双实现同版本纪律**：node runner.js 与 Go twmain runtime.go 共享单一常量 `RunnerTemplateVersion`（当前 = 5；Go 模板分支新增不改存量 node 语义，不 bump）——任一实现的语义变更必须同步另一实现并递增同一常量，禁止「node 到 v6 而 Go 停在 v5」的漂移。

**本地测试指引**：契约镜像可直接 `docker run` 本地对照 `:18080`——`docker run --rm -p 18080:18080 <image>` 后 `curl http://127.0.0.1:18080/_tw/health` 与 `curl -X POST -d '{"hello":"world"}' http://127.0.0.1:18080/` 即可自测契约符合性（Lambda RIE 等效物）；twmain 的 runtime.go 与 runner.js 源码即参考实现。三期契约基础镜像（FROM node:18-alpine + 预置 runner）交付后，BYO 用户 `FROM` 后 COPY 代码即得合规镜像。

### 3.3 部署后验证 spawn（构建质量门）

build 成功后 dispatcher 自动追加**验证 spawn**（`dispatcher/daemon.go` `spawnVerifyInstance`）：spawn 一枚验证实例 → `/_tw/health` 轮询（预算 = `functions.dispatcher.boot_timeout`，默认 60s）→ 就绪即回收。实施语义：

- **开关**：`functions.dispatcher.verify_build`（config optional bool——未设置 = 默认开启，显式 false 全局关闭）；node / go 同一机制，无按 runtime 的分档。
- **池外实例**：走 daemon 原语直连——不进 Redis 注册表、不受 `max_resident_instances` 约束、不参与 reaper 对账，用完即删。
- **env 与执行同源**：验证实例携带函数当前 variables（模块顶层 / init 依赖环境变量是常见模式，无 env 的验证会误杀合法部署）；`TW_DATA` / `TW_EXECUTION_TOKEN` 不进容器 env（构建/验证期无执行身份，常驻的是容器不是凭证）。
- **egress 与执行一致**：untrusted 函数（client_callable 或存在触发器）的验证实例挂 **internal 变体网络**——部署期不给不可信镜像开出网窗口（执行期 egress 约束不被部署期旁路）。
- **失败处置**：回收容器日志尾部（≤64KB）拼进错误 → `deployment.error`——运行期错误（panic / 协议未实现）的第一现场；编译错误在构建日志、不经此路径。
- **验证范围 = health 探针，不做 invoke 验证**：invoke 需要平台构造 TW_DATA 并**执行用户代码**（副作用不可控），违反「部署期不执行用户代码」不变量；「health 通过 ≠ invoke 语义正确」的残余风险由运行期 transport-error → 杀实例重建语义兜底（有意为之，非疏漏）。

### 3.4 部署源：git 仓库（packer）

git 部署源（二期，设计 `docs/design/functions-runtimes-and-sources.md` §2）：`CreateDeployment` 请求的 `source` oneof 二选一——`code`（zip bytes，既有通道）或 `git`（`GitSource`）。git 源由独立 **packer 服务**（§3.5）把 `url@ref[:directory]` 物化为与 zip 源**同构**的代码包，随后走同一条构建路径（写盘 → INSERT → `buildDeployment`）；zip 流向反转：packer → server 落既有 `zipPath`。`function_deployments` 的 source 投影列（projectschema 迁移 000023）：`source_type ∈ {zip, git}` + 审计四件 `source_url` / `source_ref`（钉死 commit SHA）/ `source_dir` / `context_sha256`（物化 zip 的 hex sha256）——**凭证字段在投影上不存在**。

**GitSource 字段语义**（protovalidate 声明 + app 层 `validateGitSource` 纵深复核，`internal/app/functions/deployments_git.go`）：

| 字段 | 语义 |
|---|---|
| `url` | 仅 `https://`（`functions.packer.allow_insecure=true` 时放行 `http://`，与 packer 侧 SSRF 开关同源）；**拒绝 URL 内嵌 userinfo**——带凭证的 URL 会进错误消息/日志，凭证必须走独立字段；packer 侧另有 `file://`/裸本地路径，仅 `TORCHWOOD_ENV=development`（本地调试） |
| `ref` | 空 = HEAD；branch / tag / 40 位 hex commit 均可（字符白名单 `[A-Za-z0-9._/-]`）。解析顺序 HEAD → hex → branch → tag，全部未命中明确 404，**不静默回落 HEAD**——回落会把部署钉到非请求内容 |
| `directory` | 仓库内子目录 = 构建上下文根（`/` 分隔、相对路径；拒绝绝对路径与任何 `..` 段）；空 = 仓库根。物化 zip 的根 = 该子目录（子目录外的文件不入包） |
| `username` / `token` | 私有仓库的 Basic 凭证（PAT）；`username` 空回落字面量 `git`（GitHub/GitLab PAT 通用形态）。**一次性凭证**：仅本次请求内存送达 packer，不落库、不写日志、不回显（D8）——分支后续移动不影响已部署内容，重新部署才需要再给凭证 |

**钉死 commit**：packer 解析 ref 后返回 `CommitSHA`，落 `source_ref` 列——部署内容 = 该提交的不可变快照；`context_sha256` 使盘上 zip 可审计比对。

**失败清理分流**：pack 失败 / 写盘失败 / INSERT 失败 / 构建信号量满 → **无行无 zip**（请求级失败不留残骸）；**构建失败（已收敛为 `failed` 状态）时 git zip 保留**——与 zip 源构建失败即删不同：物化快照在盘上，worker 补构建以盘上 zip 为输入、不依赖一次性凭证（重建语义，`buildDeployment` 按 `source_type` 分流）。

**条目边界（诚实声明）**：packer 物化上限 **5000 条目**（`MaxPackEntries`——git worktree 是真实文件，宽于 zip 上传通道的 1000 防炸弹声明侧预检）；dispatcher 构建侧对 BuildImage 统一放宽到同口径（`ExtractZipRelaxed`，条目 1000 → 5000，单条 100MiB / 总量 200MiB 维持）。**>5000 条目的典型 vendor 项目仍受限**——属声明边界，超限报 ResourceExhausted；node 项目请依赖平台代装依赖（§3），Go 项目 vendor/ 想进包请自行瘦身。

**CLI**（一次性 token 走环境变量，绝不进 argv / shell history）：

```bash
export TORCHWOOD_GIT_TOKEN=ghp_xxx        # 或 --git-token-env 指向其他变量名
./bin/torchwood functions deployments create-from-git greet \
  --url https://github.com/acme/functions.git \
  --ref main --dir functions/greet        # 公开仓库也请设一个非空占位值（显式优于静默匿名拉取）
```

token 未设置即报错（不静默匿名拉取私有仓库）；服务端 `functions.packer.url` 未配置时 git 源报明确错误（`FailedPrecondition`），zip 源完全不受影响——**增量启用**。

### 3.5 packer 服务（运维）

独立进程（`cmd/packer`，`task build` 一并产出；实现包 `packer/`）专职承载**不可信 git 输入**的重资源操作：浅克隆 + worktree 核算 + 子目录物化为 zip（go-git 纯 Go 实现，无系统 git 依赖）。

- **无状态、可牺牲**：零 Redis / DB / docker 依赖；单请求内存上界 ≈ 物化 zip 预算（默认 50MiB）+ base64 膨胀（×4/3）。它挂了只有 git 部署不可用（重启即恢复），API / 函数执行 / node 构建无感；可独立重启 / 扩缩（多副本无亲和需求——zip 由 server 落构建亲和节点本地盘）。
- **并发自限**：进程内信号量（容量 = `concurrency`，默认 4），饱和**立即 429**——与 dispatcher 的构建信号量成两道独立闸、无嵌套（pack 在构建信号量之外）。
- **两级预算 + 整体封顶**：`max_repo_bytes`（worktree 磁盘侧，默认 200MiB）/ `max_zip_bytes`（物化 zip 传输侧，默认 50MiB，与构建内联通道同源）+ 5000 条目；`fetch_timeout`（默认 120s）对单次打包整体封顶（clone + 核算 + 物化共享同一预算），到点 504。物化跳过 `.git` / symlink、拒收 `node_modules`（与 zip 通道同口径）；同 commit 产出字节级一致的 zip（walk 字典序 + 固定时间戳 → checksum 稳定可审计）。
- **部署拓扑**：与 dispatcher 同级的**内网服务**（dokploy compose 同网部署，server 经 `functions.packer.url`（如 `http://packer:9071`）寻址）；HTTP 监听 `addr` 默认 `:9071`（与 dispatcher 缺省 `:9070` 错开）。server 进程只消费 `url` / `shared_token`，其余字段仅 packer 进程消费（与 dispatcher 同款分段约定）。
- **SSRF 防护**：默认 **https-only** 且拒绝私网 / 回环 / link-local 目标（云元数据端点 169.254.169.254 等）；校验实施在**拨号点**（`dialer.Control` 钩子，TCP connect 时对解析后 IP 判定）——DNS rebinding（解析后换记录）因此失效。`allow_insecure=true` 整体放行（http + 私网目标），为**自托管内网 git 服务**场景提供显式开关而非逼出危险旁路；口径启动即固定（go-git 协议表是进程级全局）。
- **API 面**（内网专用 + 可选 `x-tw-packer-token` 静态共享密钥，constant-time 比对；`GET /healthz` 豁免认证）：

  | 端点 | 入参 → 出参 | 说明 |
  |---|---|---|
  | `POST /v1/pack/git` | `{url, ref?, directory?, username?, token?}` → `{commit_sha, checksum, zip_base64}` | 请求体 ≤1MiB；响应读取上限 = max_zip_bytes 的 base64 膨胀 + 余量 |

  错误映射（与 dispatcher 同款）：并发饱和 429 → ResourceExhausted、超预算 429 → ResourceExhausted、fetch_timeout 504 → DeadlineExceeded、形状错误 400 → InvalidArgument、仓库/ref 未命中 404 → NotFound、git 凭证认证失败 400 → InvalidArgument；packer API 自身的共享密钥校验失败为 401，server 侧 `PackerClient`（`internal/infra/functions/packer_client.go`）按码还原 grpc status（401 → FailedPrecondition）。

### 3.6 部署源：镜像引用（BYO 契约镜像）

三期（设计 `docs/design/functions-runtimes-and-sources.md` §3）：`CreateDeployment` 的 `source` oneof 第三选支 `image`（`ImageSource`）——**免构建路径**：平台拉取用户引用的镜像 → digest 钉死 → retag 进平台命名 → 强制契约验证。仅 `runtime = image` 的函数接受（源/运行时互斥 D7 双向：image 函数拒收 zip/git 源，反之亦然；`ListRuntimes` 增 `{ID: "image", Name: "Bring your own image"}` 表项）。git/image 载荷极小，仅走 gRPC JSON 通道（请求体为顶层扁平 oneof 投影 `{"git":{...}}` / `{"image":{...}}`），不设 multipart 形态。

**流程**（`internal/app/functions/deployments_image.go` → dispatcher `POST /v1/dispatch/images/import`）：

1. server 侧形状校验（引用非空、≤500 字节）→ GetFunction 互斥校验 → **首次 ImportImage 在 INSERT 之前**（digest 是 `source_ref` 不可变写入的前提；失败无行，与 git pack→INSERT 同构；INSERT 失败由 `RemoveImage` 幂等清理兜底）；
2. dispatcher 编排（`dispatcher/daemon.go` `importImage`）：registry host 名称级准入（先于一切 docker 操作，本地直导同样受约束）→ 取得钉死内容（`ExpectedDigest` 命中本地平台镜像**零 pull**；引用本地已存在——本地构建/本地 tag 直导——直接用本地内容跳过 pull；否则 pull，`RegistryAuth` 内联单次转发）→ digest 钉死 → retag 成 `<registry>/func-<fid>-<did>` 并删原始引用标签 → **强制契约验证 spawn** → 返回钉死 digest；
3. INSERT 行（`source_ref` = digest、`template_version = 0`）→ `buildDeployment` 按 `source_type` 分流：image → 幂等 ImportImage 复检（spec 带预期 digest = 行内 `source_ref`；本地命中零操作，镜像被外部删除则重 pull）→ ready。

**引用语义与 digest 钉死**：引用形态 `host/repo[:tag|@sha256:...]`（无显式 host 归属 docker.io）。tag 在导入期钉死为 digest 落 `source_ref`——tag 上游漂移不影响已部署内容；引用自带 `@sha256:` 时校验与拉取结果一致（不匹配 InvalidArgument，防 tag 漂移）；本地构建/tag 直导不经 registry（无 manifest digest）回落镜像 ID（内容寻址钉死值）。`source_url` 保留原始引用（审计四件之一），删除部署走 `RemoveImage` 只删本地平台镜像、不动上游 registry。

**registry host 准入（`dispatcher/imageref.go`，名称级校验）**：pull 的网络发起方是宿主 docker daemon（IP 拨号点 guard 不可实施于 daemon），故采用**名称级校验 + 白名单 + 信任级论证**（部署者 = functions.write 特权主体，与 zip 上传同级）：

- 默认拒 **IP 字面量**（IPv4/IPv6，含端口形态）与 **`localhost` / `*.localhost`**——消灭「引用直接写 IP 打内网」的最廉价攻击形态，错误信息明示命中规则与放行通道；
- `functions.image.allowed_registries`（可选正向白名单，空 = 不设）：条目为精确域名或后缀域（`example.com` 同时命中自身与子域 `registry.example.com:5000`，不命中 `badexample.com`；含端口 host 需整体精确登记）；**白名单命中优先放行**——显式登记 = 运维明确意图（含内网 registry 直连 IP 的显式白名单场景）；白名单非空时未命中一律拒绝（正向白名单语义）；
- `functions.image.allow_insecure=true` 整体放行（自托管内网 registry 的显式开关，而非逼出危险旁路）；
- 诚实声明的残余面：公网域名解析到内网（DNS rebinding 形态）、错误回显的端口扫描侧信道——随 egress 原语后置。

**契约验证强制**：验证 spawn 与 zip/git 源同一机制（§3.3：health 探针、池外用完即删、携带函数 variables、untrusted 挂 internal 变体网络、失败回收容器日志尾进 `deployment.error`），且**无开关**——`functions.dispatcher.verify_build` 关不掉它：镜像内容不经平台构建护栏，验证是镜像源唯一的质量门。非契约镜像（无 runner 监听）在 boot 预算内判失败 → deployment 标 `failed`。

**并发降级**：image 部署 `template_version = 0`（未知模板，D12）——既有 `< MinConcurrencyTemplateVersion` 判定自动把 `concurrency > 1` 降为 1：BYO 镜像是否真支持并发无从验证，fail-safe 白拿（写 5 等于做无法验证的承诺）。

**一次性 registry 凭证**：`registry_username` / `registry_token` 构造 base64 RegistryAuth 单次转发 daemon pull，不落库、不写日志、不回显（D8，与 git 凭证同款）。凭证不落库的代价（声明边界）：私有镜像的 worker 补拉无凭证可用——公共镜像可补，私有补拉失败标 `failed`（重新部署时再给凭证）。

**契约基础镜像**：`docker/functions-runtime-node/` 随仓交付（FROM node:18-alpine + 预置 `.tw-runner.js` = Runner 协议 node 参考实现，与平台 bootstrap 共享单一 `RunnerTemplateVersion` 纪律）。用户 `FROM` 后 `COPY index.js /app/index.js` 即得合规镜像，无需自行实现协议；构建、推送与部署前本地自测（`docker run --rm -p 18080:18080 <image>` 后 curl `/_tw/health` 与 `POST /`，Lambda RIE 等效物）见 `docker/functions-runtime-node/README.md`。执行面（封套、`x-tw-*` header、协议演进宪法）与 §3.2 完全一致——retag 后镜像源函数与平台构建函数全链路零改动。

**CLI**（阶段 3）：

```bash
./bin/torchwood functions create --id greet --name greet --runtime image
./bin/torchwood functions deployments create-from-image greet \
  --image <registry>/my-function:1
```

## 4. 执行（同步 / 异步）

`CreateExecution` 校验 `data ≤32KB` 且为 JSON object（数组 / 标量 / null 拒绝）、`data+env ≤32KB`；缺省取最新 `ready` 部署。

- **同步**（`async=false`）：`timeout_seconds>30` 拒绝（网关 WriteTimeout 余量）；`executor.Execute` 经 dispatcher 分发，写回 `stdout/stderr/response`（各 ≤64KB 截断）→ `completed/failed`。
- **异步**：`status=queued` → Redis Stream 入队，payload `{execution_id, function_id, project_id, data, attempt?}`；首次无 `attempt`，重试 +1 持久化于消息体。
- 状态机 `queued → building（补构建）→ running → completed|failed`；`failed` 聚合 `error`（stderr / timed out / build failed）；`duration_ms` / `status_code` 落库；每函数保留最近 100 条（`PruneOldExecutions`）。

**安全基线**（`dispatcher/daemon.go SpawnInstance`）：`CapDrop ALL`、`no-new-privileges`、只读根文件系统 + `/tmp` tmpfs、memory / cpu / pids(512) 按 spec。网络默认 per-project 隔离 bridge `tw-func-<project.id>`（项目间函数容器互不可达）；显式配置 `functions.docker.network` 时 opt-in 全局网络——跨项目容器同网互通有横向访问风险（`configs/config.yaml.template` 有警告）。`TW_DATA` 由分发请求体承载，超时强制回收实例。

### 4.1 事务边界

函数代码运行在**外部 Docker 容器**（进程隔离）：输入经分发请求体注入、输出从 HTTP 封套收集，与 server 不共享 context 或数据库事务连接（容器内无 SDK 注入、无回环网络通道）。因此"事务上下文注入"前提不成立，多写原子性降级为以下形态：

- 函数内的**多写原子性**统一由 `DatabasesService/ExecuteTransactions`（`documents:execute-tx`，见 `06-databases.md` §8.1）提供——函数代码通过 API/SDK（scoped API Key）调用即可，ATOMIC 批成功全提交、任一失败整批回滚，批内事件序 = op 序。
- 单条文档写本身的原子性（数据行 + `_acl` + outbox 事件同事务）由服务端保证，与调用方是否为函数无关。
- 跨进程事务协调（两阶段提交 / 补偿协调器）不在范围内；如未来函数改宿主进程内运行时，可再评估。

### 4.2 执行身份（execution principal）

设计见 `docs/design/functions-execution-identity-and-triggers.md`。函数执行获得平台注入的**短期受限凭证**，替代"开发者往 variables 塞长期 API key"。

**注入的 env**（同步与 worker 异步路径一致，`internal/app/functions/executions.go`）：

- **`TW_EXECUTION_TOKEN`**：`twx_` 前缀的不透明 token（32 字节 crypto/rand base64url）。服务端 Redis 键 `torchwood:exec-token:sha256hex(token)` 存身份投影 `{project_id, function_id, execution_id, scopes, invoking_user_id}`，**不存原值**；TTL = 函数超时 + 60s（仅崩溃兜底）——执行结束（成功 / 失败 / panic）即**主动吊销**。"执行结束即失效"是主动语义，stdout 回显 token 的残余时效为秒级。
- **`TW_API_BASE_URL`**：函数容器回访 Server API 的可达地址（`functions.execution.api_base_url`）。dokploy compose 已接好：dispatcher 按 `functions.dispatcher.callback_container`（= `torchwood-server`）把 server 容器 attach 进每个函数网络，`api_base_url` 填容器名地址 `http://torchwood-server:9080`。手动部署须自行满足两个一致性：① 该地址在函数网络内可达；② server / worker 两进程配置同值。为空则不注入。

**函数内用法**：`Authorization: Bearer <TW_EXECUTION_TOKEN>` 调 `TW_API_BASE_URL` 的 Server API。`twx_` 前缀在凭证解析层即判定为独立凭证族，构造 execution principal（`ActorKind=execution`）。

**declared_scopes（最小特权，默认空 = 无任何平台访问）**：

- 管理经 `CreateFunction.declared_scopes` / `SetFunctionScopes`（全量替换）。每项必须形如 `<resource>:<op>`；resource 白名单 = **assets / databases / users / groups / storage / subscriptions / payments**（functions / projects / billing / outbox / oauthproviders 等平台与编排面资源一律拒绝——含 `functions:*` 自我复制的递归放大面）；op ∈ {read, write}；自动去重。
- **scope 门与 API key 完全同构**：declared scopes 运行时投影为 API key 同款权限串（`assets:write` → `assets.write`），经同一 `PolicySet.AllowsAPIKeyTargets` 求值（含 databases / storage 的实例寻址语义）。Redis 不可用 = 校验拒绝（fail-closed）。
- **数据面语义（重要）**：scope 门只是第一层。文档 / 存储的可见性基于 principal 角色（`Roles: [keys, key:function:<function_id>]`，复用 per-key 数据隔离模型）——**使用 `databases:*` / `storage:*` scope 前，必须把目标集合 / 桶的 ACL 授予 `key:function:<function_id>` 角色**，否则 scope 过门但数据不可见（错误形态是空结果，最难排查）。Console 的集合 / 桶权限编辑处像授权 API key 一样授予该角色。
- **限流维度**：函数回访流量按 `api:execution:<project_id>:<function_id>` 独立计数（默认 6000/min，可配 `security.rate_limit.functions_execution`）——既不落 per-IP（全部容器经 bridge NAT 同一出口 IP 会互相击穿），也不落 per-user（每次执行独立桶 = 实质不限流）。
- **审计**：经执行身份发起的资产写，账本 `operator` 记录 `{kind:"execution", actor_id:<function_id>, user_id:<触发用户?>}`，对标 PlayFab currentPlayerId 语义。

**variables kind 与旧指引弃用**：`function_variables` 有 `kind TEXT CHECK (kind IN ('text','secret'))` 列（projectschema 迁移 000013）。**不要再往 variables 存平台 API key**——那是明文列 + stdout 截断回存的双重泄漏面；平台能力一律用执行身份。variables 只放第三方密钥（如微信 SSV 的 AES key）；一期同表明文存储，`GetVariables` 掩码回显。

### 4.3 执行器：常驻 runner（唯一执行模型）

CGI 形态（每请求一容器）为设计缺陷，常驻 runner 是唯一执行路径（v1 docker 执行器已移除）。

**模型**：函数镜像 CMD 为平台 runner（node：构建期 COPY `.tw-runner.js` 作 CMD；go：平台编译产物 `/tw-app` 内嵌同契约 runtime，§3.1——`internal/infra/functions/runner/` 与 `runner/gorunner/`）。模板版本常量 `RunnerTemplateVersion` 落 `function_deployments.template_version`（当前 = 5），语义变更必须递增（node/Go 双实现同版本纪律见 §3.2）。runner 启动即加载用户入口（node：约定 `index.js` 导出 `main`/`fetch`，加载完成前 `/_tw/health` 返回 not-ready；go：入口编译期绑定恒 ready），加载后监听容器内 `:18080`（仅 per-project 桥网络可达）：`POST /` body = TW_DATA JSON + header `x-tw-execution-token`，响应 200 `{"ok":true,"result":...}` / 500 `{"ok":false,"error":...}`（附加 stdout / stderr 尾部环缓冲）；达 `TW_MAX_REQUESTS` 自退出、SIGTERM 排空在途后退出（协议全貌见 §3.2）。

**分发拓扑**：独立 `dispatcher` 进程专职持有 docker.sock（compose 唯一挂载点；dokploy 编排下 dispatcher 以 `user: root` 运行——镜像缺省用户读不了宿主 `root:docker` 的 sock）。dispatcher 按需 join `tw-func-<project>` 网络（容器 NetworkConnect 自 attach；宿主进程模式跳过——注意 Docker Desktop for Windows/macOS 的 VM 拓扑下容器 bridge IP 对宿主不可路由，分发通路要求 Linux / dokploy compose 拓扑）。server/worker 经 HTTP API 分发（适配 Executor 端口），零 daemon 依赖；zip 构建以 base64 内联传输（无共享文件系统假设）。API 面（内网专用 + 可选 `x-tw-dispatcher-token` 静态共享密钥）：

| 端点 | 入参 → 出参 | 说明 |
|---|---|---|
| `POST /v1/dispatch/builds` | `{project_id, function_id, deployment_id, zip_base64, function_timeout_seconds}` → `{error?}` | runner 模板构建；成功后旧 deployment 池 drain（宽限 ≤ 函数超时） |
| `POST /v1/dispatch/executions` | 执行规格 `{image, project_id, function_id, deployment_id, runtime, spec, timeout_seconds, env, execution_token, execution_id, source, invoking_user_id, data, pool}` → `{status, response, stdout_tail, stderr_tail, duration_ms, status_code, error}` | 池管理热路径；接受调用方 ctx 超时；`source` / `invoking_user_id` 经分发 header 进 runner ctx（§4.3.3） |
| `POST /v1/dispatch/images/remove` | `{function_id, deployment_id}` | 幂等 |

错误映射：排队超限 429 → ResourceExhausted、执行超时 504 → DeadlineExceeded、缺参 400。`TW_EXECUTION_TOKEN` 经分发 header 传递——**mint → 注入 → defer revoke 链路不变，常驻的是容器不是凭证**。

**池策略**（平台默认 + per-function 列覆盖，projectschema 迁移 000014）：`min_instances`（默认 0 = 纯 scale-from-zero；≥1 保温）、`max_instances`（默认 2）、`idle_ttl_seconds`（默认 300）、`max_requests_per_instance`（默认 1000，Lambda 同款防泄漏回收）+ 平台级常驻总量上限（每 daemon 默认 8，`functions.dispatcher.max_resident_instances`）。实现语义（`dispatcher/pool.go`，Redis 注册表 `torchwood:fninst:{project}:{function}`）：

- **spawn 收敛**：同函数并发 spawn 经 `torchwood:fnspawn:*` SETNX 锁收敛为一次，其余请求等注册表（防 daemon 重启后全量冷启动风暴）；冷启动成本由触发 spawn 的请求支付但不独占实例。
- **有界排队**：池满时排队（深度上限 `queue_depth` 默认 32 + 队首超时 `queue_head_timeout` 默认 10s）→ 超限 429 ResourceExhausted，同步调用方不无界等在 30s ctx 上。
- **判活**：dispatch 认领续租（lease）+ busy 标记；busy 实例不因租约过期被回收（不误杀执行中实例）；租约过期超 10min 才强杀。请求超时 / handler 崩溃 → 杀整个实例（实例级隔离粒度，业界 FaaS 同款取舍；同实例跨请求共享模块级状态，仅同函数同租户）。
- **idle reaper**：15s 周期 docker inspect 与注册表 diff 幽灵对账；idle > idle_ttl 且实例数 > min_instances 回收；draining 实例兜底清理；`function_resident_uptime_ms` 按实例存活累计（保温成本显式计费口径）。

**执行路径与 SLA**：池上限 / 排队由 dispatcher 内部管控（无全局 run 信号量——双重限流会互相饿死）。**v1→dispatcher 升级要求：重新部署存量函数**——以 v1 模板构建的镜像（CMD 跑完即退）在常驻健康探针处必然失败，升级后须对全部函数重新 `CreateDeployment`。SLA 口径：**热路径同步分发简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）**；冷启动与异步队列路径显式不在 SLA 内。

### 4.3.1 执行器 v3：实例内多路复用（runner v3）

设计见 `docs/design/functions-v3.md` §1。v2 的"1 并发/实例串行"是吞吐天花板；v3 不推翻 v2 底座，打开单实例并发复用——**concurrency=1 时与 v2 行为逐步等价**。

- **并发模型与可重入契约（红线）**：单 Node 事件循环内多请求交错（非多进程 / worker_threads）——**函数作者必须保证 `main` 可重入：模块级可变全局状态在并发下有竞态**，与 Lambda / Cloud Run 同款契约。平台责任 = 默认 `concurrency=1`（不 opt-in 即无暴露，fail-closed）+ 文档明示 + max_requests / 崩溃重建兜底。迁移 000017 落 `functions.concurrency`（默认 1，CHECK 1..16——上限 16 = 8 实例 × 16 = 128 并发对单机拓扑够用）。池语义变化仅认领条件一处：`ClaimIdle` 从"实例空闲"变为 `inflight < concurrency`（释放路径 Lua 原子化防并发交错丢更新）；背压顺序不变（认领 → trySpawn → 有界排队 → 超限 429）。
- **`main(data, ctx)` 第二参数**：runner 以 `AsyncLocalStorage` 圈住每次调用，`ctx = { executionToken, apiBaseUrl, executionId }`（v5 起追加调用身份三件，见 §4.3.3）。**含 await 的 main 必须读 `ctx.executionToken`**：`process.env.TW_EXECUTION_TOKEN` 仍设置（同步 main 与模块顶层读取兼容），但 async 函数在 await 恢复后 env 可能已被并发请求覆盖（凭证串号——A 以 B 的身份干活）；ctx 是并发下唯一安全通道。执行 ID 经分发 header `x-tw-execution-id` 透传进 `ctx.executionId`（日志关联）。
- **per-request 日志分桶**：console 捕获按请求环缓冲——执行记录的 `stdout` / `stderr` 语义从"实例级混流尾部"变为"**本请求** console 输出尾部"（审计口径更准，排障改善）。
- **超时语义变更（有意变更）**：超时 / 调用方取消只失败该请求、**不再杀实例**——并发下一个慢请求不得误杀同实例健康在途请求（Cloud Run 同款）；传输层错误（连接拒绝 / reset）仍杀实例（容器崩溃判定）。配套**超时熔断**堵住"超时不杀"打开的僵尸负载通道：实例累计超时达阈值（默认 5，可配 `functions.dispatcher.timeout_budget`）→ 杀实例重建 + 熔断指标。诚实声明（Lambda 同款）：超时后用户 main 可能仍在事件循环里跑至实例回收，inflight 按请求生命周期释放、不追踪用户代码生命周期。
- **降级保护（fail-safe 不 fail-closed）**：函数 `concurrency > 1` 而执行所用 deployment 的 `template_version < 3` 时**静默按并发 1 执行** + 降级计数指标——存量函数不因新列拒绝执行；重新 `CreateDeployment` 获 v3 模板后自然生效。
- **生效时机**：concurrency 在 spawn 时固化进实例记录——**调大后存量实例按旧值服务至 idle 回收 / 部署更替**，不热生效。

### 4.3.2 Web 标准 fetch 入口（runner v4）

runner 加载用户模块时按固定优先级探测导出：`mod.fetch` 为 function → fetch 风格；否则 `mod.main` → main 风格（既有路径，零改动继续可跑）；两者皆无 → 加载错误 `index.js must export main or fetch`（加载失败常驻 not-ready）。

**CJS 约定**：runner 以 `require` 消费用户模块（纯 CJS）——fetch 风格写法是 `module.exports = { fetch }` 或 `exports.fetch = async (request, env) => {...}`。ESM 示例（`export default { fetch }`）需经打包 / 互操作落成 CJS 导出后才能被识别。

```js
// index.js —— fetch 风格（Node 18+ 原生全局 Request/Response，undici）
exports.fetch = async (request, env) => {
  const { userId } = await request.json();
  return Response.json({ ok: true, userId });
};

// main 风格（v1 起既有）：main(data, ctx)，双轨并存
exports.main = async (data, ctx) => ({ ok: true });
```

**env 三件**：fetch 风格的 `env` 是每次调用的**参数**（非 process.env），恒为 `{ EXECUTION_TOKEN, API_BASE_URL, EXECUTION_ID }`（v5 起追加调用身份三件 `SOURCE / INVOKING_USER_ID / PROJECT_ID`，与 ctx 同源）——token 经参数传递，并发下串号问题在该风格下结构性不存在。functionVariables 仍固化在容器 `process.env`（函数级非请求级，第三方库读 env 照常工作）；`env` 参数与 data 同计 32KB 预算。

**触发器 sync 完整透传**（HTTP 触发器 + fetch 风格 = 标准 Web 处理器）：

```js
// 微信 SSV 验签（fetch 风格重写）：
exports.fetch = async (request, env) => {
  const url = new URL(request.url);              // query 回到该在的位置
  const signature = url.searchParams.get("signature");
  const body = await request.text();              // 原始 body 无需解析封套
  // …sha256(sort(query)+body+app_secret) 验签、AES 解密…
  return Response.json({ is_valid: true });       // status/headers/body 全透传
};
```

sync 模式下函数返回的 `Response` 完整透传给调用方：HTTP status（≥100 即函数 HTTP status）、headers（runner 侧过滤 hop-by-hop 与 date/server/host 等平台头；handler 侧第二层白名单过滤）、body（>64KB 截断）。自定义状态码、二进制（`body_base64` 无损）、302 重定向从此可达。main 风格封套照旧进 TW_DATA（双轨并存到 main 退役）。一期限制：invoke 路径（server / client）不回传 headers、body 全缓冲不流式；`waitUntil` 后台任务不做。Go 运行时的 fetch 双轨对应 `Fetch(w http.ResponseWriter, r *http.Request)`（§3.1）——请求级身份经还原 Request 的 x-tw-* header 可达（信息等价、通道不同，见 §3.1 fetch 风格身份通道）。

### 4.3.3 ctx 调用身份三件（runner v5）

平台经**可信通道**把调用身份注入函数运行时，handler 据此做 op 级鉴权与审计——"身份由平台注入"原则在 handler 侧的补全（此前 `invoking_user_id` 只存在于执行行与 execution token 的服务端投影，函数内做 op 级鉴权没有可信输入）。

**注入链路**：执行记录 `trigger_source` / `invoking_user_id` / 所属项目经 `buildExecution` 进执行规格 → dispatcher 客户端进分发请求体 → dispatcher 经分发 header `x-tw-source` / `x-tw-invoking-user-id` / `x-tw-project-id` 注入 runner → main 风格读 `ctx`、fetch 风格读 `env` 参数，两风格同源。**并发安全性与 `ctx.executionToken` 同节律**：值进 AsyncLocalStorage 圈住的请求级 store（不落 `process.env`、无跨请求覆盖面），多路复用下并发请求各自拿到自身身份。

| 字段 | 类型 | 取值 | 何时为空 |
|---|---|---|---|
| `ctx.source` | string | `client`（客户端调用面 §14）/ `http:{trigger_id}` / `cron:{trigger_id}` / `event:{trigger_id}` / `server`（Server 面 CreateExecution） | **恒非空**——空 `trigger_source` 由平台映射为 `server`；header 缺省时 runner 回落 `server` |
| `ctx.invokingUserId` | string | 客户端调用面的调用用户 id（principal 注入，非请求体自报，不可伪造） | 空串 = 非用户触发（server 面 / 触发器路径，**系统语义**）——函数不得把空值当匿名调用者放行 |
| `ctx.projectId` | string | 执行所属项目 id | 恒非空（分发请求必带 project）；防御性容忍空串 |

```js
// op 级鉴权示例（rpc 封套配合，见 §14.7）：
exports.main = async (data, ctx) => {
  if (data.type === "rpc" && data.op === "admin") {
    if (ctx.source !== "client" || !ctx.invokingUserId) {
      return { error: "forbidden" };        // 非用户触发不进管理操作
    }
    // …按 ctx.invokingUserId 鉴权/审计…
  }
};
```

**生效条件**：字段随 runner 模板 v5 起注入——存量 deployment 需重新 `CreateDeployment` 获 v5 模板；旧模板实例不注入，函数侧不应假设字段必然存在（防御性读法同 `ctx.executionToken`）。

### 4.4 同步快路径两写预占记账

同步执行跳过 queued / building 中间态：分发前 `INSERT (status='running', timeout_seconds 快照)` 直接预占——预占行即刻成为审计 / 限频计数依据（"先占位后执行"），执行中崩溃行留在 running、由周期孤儿恢复按 `staleAfter = timeout_seconds + 120s` 宽限判 failed（timeout 快照为 NULL 的存量行回退 1h；扫描范围 queued / building / running——修掉 v1"同步崩溃留 queued 永不入队"的洞）。结束后 `UPDATE` 终态（completed / failed + outputs + duration_ms）。孤儿恢复为 worker 每分钟周期 ticker；Prune（保留最近 100 条）移出同步热路径，由 worker 10min 低频 ticker 承接。异步路径状态机 `queued → building → running` 原样保留。

## 5. 构建信号量

`pkg/semaphore` 提供 `RedisSemaphore` + `InMemorySemaphore` 回退（`internal/app/functions/semaphores.go`），接口 `TryAcquire(ctx)(bool, func(), error)`：

| 信号量 | max | TTL | key 前缀 |
|---|---|---|---|
| `Build` | 4 | 360s | `torchwood:sem:build:slot:<idx>` |

（原 `Run` 全局执行信号量随 v1 docker 执行器移除——执行并发由 dispatcher 池上限与有界排队管控；`torchwood:sem:run:*` 残留键 TTL 过期自动消亡。）

实现：依次 `SETNX key token EX ttl` 抢槽位，命中即成功，返回 `release` 闭包——Lua 脚本比对 token 后 DEL（防误删过期后被他人占用的槽位；以 `context.Background()` 释放）。TTL 覆盖最长持有（360s > worker 补构建超时 5m）。`client==nil` 时回退进程内 channel 实现；`NoopSemaphore` 供测试。

```go
ok, release, err := semaphores.Build.TryAcquire(ctx)
if err != nil { return status.Error(codes.Internal, ...) }
if !ok { return status.Error(codes.ResourceExhausted, "too many builds") }
defer release()
```

## 6. Worker 与 Stream Trim

`worker/`（独立进程，无 `api` 层）中的 Functions 相关职责：

- **消费**：4 goroutine `XREADGROUP`（Redis Stream，至少一次；Block 1s 配合退出）→ `ProcessExecutionPayload`。
- **领取**：`TransitionExecutionStatus(queued→building)` CAS 防重复投递，重复消息静默跳过（at-least-once 收敛）。
- **补构建**：非 `ready` 时以 5m 超时同步 `buildDeployment`，失败归还 `building→queued` 并 requeue。
- **重试**：`attempt` 持久化于 payload，瞬时失败 requeue 时 +1；超过 `maxProcessAttempts=3` 则标记 `failed`；非法 payload 丢弃不重试（无死信队列）。
- **孤儿恢复**：每分钟周期任务，stale 判定 = 行内 `timeout_seconds + 120s`（NULL 回退 1h），将超时的 queued / building / running 标 failed。
- **cron 调度循环**：每分钟 `DispatchDueCronTriggers(now, 100)` 领取到期 cron 触发器并入队异步执行（§13.2）。
- **事件触发器消费循环**：`functions-triggers` 消费组 XREADGROUP `torchwood:events` + 订阅匹配器 15s 快照 + 停机补投（§13.5）。
- **优雅退出**：`Stop` 取消消费上下文（XREADGROUP Block 1s 内返回）。

`StreamTrimmer`：每 10min `XTRIM APPROX ... MAXLEN 100000`（XADD 侧不设 MaxLen 保未投递消息，裁剪低频异步化）。

## 7. per-statement 超时（Functions 侧）

bunrepo 每方法入口 `context.WithTimeout`（读 5s、写 10s，`WithoutCancel` 不受上游取消牵连）；队列 Enqueue / Dequeue / Trim 均 5s；worker 补构建单独 5m。

## 8. 事件脊柱与 outbox 超时

Functions 的异步执行投递失败亦通过统一 `outbox` 事件对外可见（`shared.EventPublisher` 在写事务内 INSERT outbox）。`internal/infra/events/outbox_worker.go` 的超时与重试语义：

| 语句 | 超时 | 说明 |
|---|---|---|
| `SELECT COUNT(*) pending` 指标 | 5s | Gauge，每轮先刷新 |
| claim（`FOR UPDATE SKIP LOCKED` + `UPDATE dispatched_at`） | 10s（事务 2 倍语句超时） | `LIMIT 32`，行锁防多副本重复 XADD |
| XADD 失败退避 UPDATE | 5s | 指数 `1<<attempts` 秒，上限 60s，快速重试 |
| 死信迁入 `INSERT...SELECT → DELETE` | 10s | attempts≥10 入 dead 表 |
| 清理 published>24h / dead>30d | 5s ×3 | 启动即执行，随后 10m 周期 |

轮询 200ms、batch=32；`pending/dead` Gauges 与 `publish_lag` Histogram（Prometheus）。本地复现：

```bash
go test ./pkg/semaphore -run TestRedisSemaphore -count=1
go test ./dispatcher -run TestIntegration_Dispatcher -count=1
```

（dispatcher 集成测试探测本机 docker daemon：可达即跑、不可达自动 skip；非默认 daemon 用 `TORCHWOOD_FUNCTIONS_DOCKER_HOST` 指向。）

## 9. 配置

`internal/pkg/config/config.proto` 中 Functions 相关键（环境变量映射见 `03-configuration.md`）：

| 键 | 说明 |
|---|---|
| `functions.dispatcher.url` | **必填**（缺失时 server/worker 启动失败），dispatcher 服务地址 |
| `functions.dispatcher.shared_token` | 内网可选认证（`x-tw-dispatcher-token`） |
| `functions.dispatcher.max_resident_instances` | 每 daemon 常驻总量上限，默认 8 |
| `functions.dispatcher.queue_depth` / `queue_head_timeout` | 有界排队深度 32 / 队首超时 10s |
| `functions.dispatcher.boot_timeout` | 实例启动超时 60s（验证 spawn 的探针预算同源） |
| `functions.dispatcher.build_timeout` | 构建整体超时（构建 ctx 与请求 ctx 解耦后的独立预算），默认 `5m`——Go 冷构建含基础镜像拉取，全新环境首个 Go 部署必要时调大（§3.1） |
| `functions.dispatcher.verify_build` | 构建后验证 spawn 开关（optional bool：未设置 = 默认开启，显式 false 关闭；§3.3） |
| `functions.dispatcher.timeout_budget` | 超时熔断阈值，默认 5 |
| `functions.dispatcher.addr` | dispatcher HTTP 监听地址，默认 `:9070` |
| `functions.dispatcher.callback_container` | dokploy 场景随函数网络 attach 的 server 容器名 |
| `functions.docker.host` | 默认 `unix:///var/run/docker.sock`，仅 dispatcher 进程消费 |
| `functions.docker.network` | 默认留空 = per-project 网络（不存在时自动创建 bridge）；显式配置为 opt-in 全局网络 |
| `functions.docker.registry` | 小写，默认 `torchwood-funcs` |
| `functions.execution.api_base_url` | 函数容器可达的 Server API 地址，注入 `TW_API_BASE_URL`；空 = 不注入 |
| `functions.trigger.http_ip_per_minute` | HTTP 触发器每 IP 限频，默认 3000 |
| `functions.client_invoke.per_user_concurrency` | 每用户并发闸门，默认 2 |
| `functions.client_invoke.queue_head_timeout` | 并发闸门排队队首超时，默认 5s |
| `functions.packer.url` | packer 内网 HTTP 基址（server 侧消费），如 `http://packer:9071`；**空 = git 部署源未启用**（zip 源不受影响，§3.4/§3.5） |
| `functions.packer.shared_token` | 内网可选认证（`x-tw-packer-token`），空 = 不校验（仅限可信内网） |
| `functions.packer.fetch_timeout` | 单次 fetch+物化整体超时，默认 `120s`（仅 packer 进程消费） |
| `functions.packer.max_repo_bytes` | 克隆 worktree 磁盘侧预算，默认 200MiB（仅 packer 进程消费） |
| `functions.packer.max_zip_bytes` | 物化 zip 传输侧预算，默认 50MiB（仅 packer 进程消费） |
| `functions.packer.concurrency` | 并发打包上限，默认 4；饱和立即 429（仅 packer 进程消费） |
| `functions.packer.allow_insecure` | 放行 http:// 与私网/回环目标（SSRF guard 整体放行，自托管内网 git 场景），默认 false |
| `functions.packer.addr` | packer HTTP 监听地址，默认 `:9071`（仅 packer 进程消费） |
| `functions.image.allowed_registries` | registry host 正向白名单（空 = 不设白名单）：条目为精确域名或后缀域（`example.com` 命中自身与子域，含端口需整体精确登记）；命中优先放行，非空时未命中一律拒绝（仅 dispatcher 进程消费，§3.6） |
| `functions.image.allow_insecure` | 放行 IP 字面量与 `localhost`/`*.localhost` 形态的 registry host（自托管内网 registry 显式开关），默认 false（仅 dispatcher 进程消费，§3.6） |

`functions.executor` 键已删除（reserved；残留配置键被静默忽略）。`task build` 同时产出 server / worker / torchwood / dispatcher / packer 五个二进制。

## 10. 变量与保留策略

- 变量 `SetVariables` 全量替换；`sanitizeEnv` 丢弃键含 `\n\r\0` 的变量；执行时 `envSize(vars)+len(data) ≤32KB`（`TW_EXECUTION_TOKEN` / `TW_API_BASE_URL` 注入亦计入该预算，约 200B 量级）；`GetVariables` 返回掩码 `******`，真实值仅在 Set 请求可见一次；`kind`（text / secret）见 §4.2。
- **执行记录保留分级**：server 来源条数式每函数保留最近 100 条；client / http / cron 来源按时间窗保留 **48h**（≥2× 最长限频窗口 day=24h——这些行是限频 DB 降级的窗口内计数依据，条数裁剪会少计超发）。两条删除路径互不误伤，均只清终态行。`DeleteFunction` 级联清 deployments / variables / executions + 逐镜像 RemoveImage + 删 zip。
- `XTRIM` 不在入队侧做，由 StreamTrimmer 以 10m 周期异步 Trim（`MAXLEN≈100k`，APPROX），水位远高于正常积压。

## 11. 超时与可观测性

- 同步执行外层由拦截器设超时，内层 `executor.Execute` 以 `fn.TimeoutSeconds` 为 ctx 超时，handler 额外 +60s 余量覆盖镜像清理。
- **核心指标**（Prometheus，包内自注册）：`torchwood_functions_execution_duration_seconds{project,function,source,status}`、`torchwood_functions_executions_total`（source = server|http|cron|client|event）、`torchwood_functions_queue_wait_seconds`（异步 queued→running）、`torchwood_functions_invoke_total{project,function,source,result}`（触发器 + 客户端入口计数）。
- **dispatcher 侧指标**：`torchwood_functions_cold_starts_total`（池 0→1 计数）、`torchwood_functions_init_duration_seconds`（对标 Lambda initializationDuration）、池水位 `torchwood_functions_pool_{ready,booting,draining}`、`torchwood_function_resident_uptime_ms`（保温成本口径）、`torchwood_functions_dispatch_duration_seconds` / `torchwood_functions_dispatch_queue_wait_seconds` / `torchwood_functions_dispatch_queue_{dropped,timeouts}_total`。
- SLA burn 告警锚点：热路径端到端 P99 > 100ms 或平台开销 > 25ms。
- 其他：`torchwood_functions_egress_class_total{project,class=trusted|untrusted}`（egress 分类计数）；`torchwood_functions_concurrency_downgraded_total`（旧模板并发降级）、`torchwood_functions_instance_timeout_fuse_total`（超时熔断）、`torchwood_functions_instance_inflight`（在途水位）、`torchwood_functions_event_deliveries_total` / `torchwood_functions_event_backfill_total`（事件触发器投递 / 补投）。
- 日志：容器侧 stdout/stderr 缓冲 1MiB；`error` 列截断 64KB。

## 12. 测试与已知边界

- 单元：`internal/app/functions/`（并发上限、截断、队列 payload 校验、鉴权分支）；`internal/infra/queue/redis_queue_test.go`。
- 安全：`security_test.go`（zip slip / 符号链接 / size 上限）；`authz_test.go`（写方法鉴权）；`semaphore_test.go`（SETNX + Lua 互斥）。
- 集成：`dispatcher/daemon_integration_test.go`（门控 `TORCHWOOD_FUNCTIONS_DOCKER_HOST`，CI 预拉 node:18-alpine / golang:1.26-alpine；含 python zip 构建期拒绝用例）；Go 端到端（`TestIntegration_GoMainFunctionE2E` / `GoFetchFunctionE2E` 双风格执行断言、`NodeVerifyStillGreenE2E` 护栏、`GoVerifyFailureCapturesLogTail` 验证失败日志尾回收、`GoMissingEntryRejected` 缺入口拒收）；git 源 docker 集成（`dispatcher/git_source_integration_test.go`：`GitSourceGoFunctionFullChainE2E` git fixture → PackGit → BuildImage(verify) → 池执行全链 + `GitWideEntryZipBuild` >1000 条目放宽预算构建回归）；**镜像源 docker 集成**（`dispatcher/image_source_integration_test.go`：`ImageSourceFullChainE2E` 旗舰全链——BuildImage 产物即契约镜像 tag 带域名引用 → ImportImage 本地直导（host 准入拒 IP 字面量 / digest 钉死 = Image ID / retag 进平台命名 / 原始引用删除 / 强制验证）→ 池执行封套断言；`ImageSourceNonContractRejected` 非契约镜像验证拒收 + 残留断言；`ImageSourceDigestPinIdempotentResummon` ExpectedDigest 命中零 pull（不可解析 `.invalid` 域名实证——实现若违规 pull 必炸）+ 本地 miss 重拉真实触网报错）；worker 的消费 / requeue 测试（attempt 持久化、Transition CAS）；app 层 git 源进程内冒烟（`deployments_git_e2e_test.go`，真实 DB + fake packer/executor）；app 层镜像源分支（`deployments_image_test.go` 互斥校验/形状校验/降级 + `deployments_image_dispatch_e2e_test.go` 真实 DB + fake executor）。
- **未落地清单**：独立构建队列（CreateDeployment 同步构建，worker 消费前补构建兜底）；重试无死信队列（超限直接标 failed）；变量明文；`entrypoint` 固定入口；多机需对象存储承载 zip；池策略管理 API 面（min/max_instances、idle_ttl、max_requests、concurrency 的 UpdateFunction 字段 + Console 卡片未落地）——**concurrency 现无管理入口，恒为列默认 1**，行为与 v2 串行等价。

## 13. 触发器（HTTP + cron + 事件）

实体 `function_triggers`（projectschema 迁移 000015 + 000018；`internal/domain/functions/triggers.go`）：`type ∈ {http, cron, event}`，config JSONB 存分类型配置；http 的 token 为独立列（`UNIQUE(token)` 支撑查找）。管理面走 Server RPC（functions.write + admin/owner）：`CreateFunctionTrigger` / `ListFunctionTriggers` / `DeleteFunctionTrigger` / `RotateFunctionTriggerToken`（token 轮换仅 http）。函数删除经 FK `ON DELETE CASCADE` 级联清理触发器（dispatcher 不需通知，靠 idle TTL 收敛）。

### 13.1 HTTP 触发器（公开 URL）

- **路由** `/f/{project_id}/{trigger_token}`。token 128bit 随机（base64url 22 字符），**不可猜即鉴权**；URL 含 token 会被代理 / 访问日志记录，疑似泄漏即调 `RotateFunctionTriggerToken`（旧 token 立即失效）。
- **只路由不验签**：请求以封套透传进 `TW_DATA`（函数内 `JSON.parse`）：

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

  **raw_query 必须透传**——微信 SSV 的验签参数在 query 而非 body。headers 白名单（统一小写键）：全部 `x-*` + `content-type` + `wechatpay-*` 前缀，其余剥除（Authorization 等凭证不透传）。body 双通道：`body` 为 best-effort UTF-8 字符串，`body_base64` 恒在（无损，二进制 webhook 用）。平台不解析 body 内容、日志不落 body 原文。
- **双响应模式**（创建时必选 `response_mode`）：
  - `sync`：同步执行（≤30s）并透传函数响应——completed → `200 + response`；failed → `502`；超时 `504`。适合非时限场景。
  - `async_ack`：**先入队成功、后写 200（顺序红线）**——入队失败一律 503 让回调方重试（先 200 后入队的抖动窗口 = 事件永久丢失且平台无痕迹）。200 body = 创建时配置的 `ack_body`（≤1KB，防公开端点带宽放大）。**诚实语义：async_ack 的 200 是「已受理」而非「验证通过」**——函数内验签失败无法追回响应，需配合幂等（§13.6）。
- **GET 握手**：`handshake: "echo"` 时平台对 GET 直接回 `{"echostr": <query.echostr>}`（200，`Cache-Control: no-store`），不 invoke；非 echo 模式 GET → 405。
- **body 上限**：缺省 64KB、per-trigger 可配至 1MB（`body_limit_bytes`）；超限 413。
- **per-IP 限频**：Redis 固定窗口（键 `torchwood:ftrig:ip:{ip}`），独立默认 **3000/min**（微信回调出口 IP 段集中，沿用全局 300/min 会在发奖风暴下 429 → 重试耗尽 → 事件丢失），可配 `functions.trigger.http_ip_per_minute`；超限 `429 + Retry-After`；Redis 故障 fail-closed（503 让回调方重试）。
- **安全与审计**：该路由不经 gRPC 拦截器链——`trigger_source = http:{trigger_id}`、`source_ip`（经 trusted_proxies 解析）即审计载体；执行 principal 的 scope / 角色语义与 §4.2 完全一致。
- **指标**：`torchwood_functions_invoke_total{source="http", result=ok|quota|echo|not_found|error|timeout}`（echo 单列——探测行为可观测）。

### 13.2 cron 触发器

- 表达式：5 字段（分 时 日 月 周），**UTC**；支持 `* , - /` 与数字；周 0-7 且 7≡0；越界一律解析报错（fail-closed）。解析器自实现于 `internal/domain/functions/cronexpr.go`（零第三方依赖），语义对齐 vixie cron：DOM 与 DOW 均受限时取并集。
- 调度：worker 每分钟 ticker → 按.active 项目轮转扫描 → `ClaimDueCron` 原子领取。
- **先 CAS 后入队（红线）**：候选无锁扫描后逐条 `UPDATE ... SET next_run_at=新 WHERE id=$1 AND next_run_at=旧` 判 rows=1——多实例并发只有赢家（先入队后 CAS 在多实例下双入队）。推进目标恒 `> now`（否则下一轮立即重复领取）。
- **misfire 语义**（默认 `catch_up_once`）：错过（到期早于 now-90s 宽限）时 `next_run_at` 直接推进到 now 之后的下一计划时刻——宕机 N 个周期只补跑 1 次；`skip` 同样推进但不补跑。入队失败把 `next_run_at` 回滚到原到期值（best-effort），下轮按 catch_up 语义重领。
- 入队 data 为 `{"type":"cron","trigger_id":"...","scheduled_for":"<RFC3339>"}`——`scheduled_for` 是函数做幂等键的推荐来源。
- `next_run_at` 在 List 响应可见。

### 13.3 事件触发器（v3 切片 D）

数据库文档写事件（create / update / delete）触发函数异步执行。链路：文档写事务内 outbox 行（既有事件脊柱，零改动）→ OutboxWorker XADD `torchwood:events` → **`functions-triggers` 消费组**（worker 进程）→ 进程内订阅匹配器 → 命中触发器逐条异步入队（与 cron 同一通道，`trigger_source = event:{trigger_id}`）。outbox 主投递路径（WS 实时扇出）零侵入。

- **订阅串格式**：`databases.{database_id}.collections.{collection_id}.documents.{op}`，op ∈ {create, update, delete}。一期通配语义：collection 段与 op 段可为 `*`；**database 段必须精确**。解析 / 校验 / 匹配实现于 `internal/domain/functions/eventmatch.go`（创建期与匹配器构建共用同一解析——存储侧无第二套宽松口径）。一个触发器可带多条订阅串（≤64 条）；同一事件命中同一触发器的多条订阅串会**多次投递**，函数幂等吸收。
- **存在性 best-effort**：创建时不强制校验 database / collection 存在——订阅不存在的集合合法（事件永不命中，静默无投递）。
- **data 投影与回读（32KB 预算的关键设计）**：事件信封可达 1MiB 而执行 data 上限 32KB——data 只带投影：

  ```json
  {
    "type": "event",
    "event": "databases.documents.update",
    "event_id": "01J…", "seq": 42,
    "database_id": "app", "collection_id": "notes",
    "document_id": "doc_1", "version": 7,
    "envelope_truncated": false,
    "data": { "id": "doc_1", "data": {…}, "permissions": […], "created_at": "…", "updated_at": "…", "version": 7 }
  }
  ```

  文档投影"尽力塞入剩余预算"，超限剥掉 data 并标 `data_truncated: true`；`envelope_truncated` 是信封自身在 outbox 序列化期的 1MiB 预算截断（**两级截断分名，语义不混**）。函数按 `document_id` 用 `databases:read` scope 回读全量——"ID + 摘要进 data，全量靠回读"与 HTTP 触发器封套同一哲学。delete 事件无 data 键。
- **权限不豁免（重要）**：回读走 execution principal + RLS 完整链路——函数需 `databases:read` declared scope，**且目标集合 ACL 授予 `key:function:<id>` 角色**，否则 scope 过门但回读为空。
- **投递保证与幂等键**：at-least-once——重叠投递由函数幂等吸收；**幂等键推荐来源 = data 的 `event_id`**（seq 是集合内分配序、有空洞，仅当游标与去重辅助用，勿当全局提交序）。消费组 XACK 在入队成功后；XACK 前崩溃的在途条目由 XAUTOCLAIM（1min idle）重投。
- **停机补投**：`XTRIM`（~100k 水位）不理会消费组进度——worker 停机超过 Stream 裁剪窗口后，消费组会静默跳到现存最老条目。worker 启动时以 Redis 自管水位键 `torchwood:fnevent:lastseq` 与 Stream 现存首条的信封 seq 比较，`first_seq > lastseq+1` 即存在裁剪缺口 → 从 outbox 表按 seq 区间分批（500/批）补投，走同一匹配 + 投递路径（与恢复后的正常消费重叠投递一次，幂等吸收）。**诚实边界：outbox 行 24h 清理窗口之外的极端停机（>24h）才真正丢失**（与 WS 订阅 `EVENTS.RESUME_EXPIRED` 同一口径）。
- **风暴兜底**：一期不做精确限流——既有机制兜底（dispatcher 有界排队 429 → enqueue_error + 补投分批推进）。
- **自环警告（无硬防护）**：函数订阅自己写入的集合 → 写 → 事件 → 再触发循环会无限放大。平台一期不做递归硬防护（文档警告）——Console 订阅编辑处展示警告文案；链式调用多个函数成环同样危险。观测兜底：`torchwood_functions_event_deliveries_total` 只记命中的触发器（no_match 不记）——本指标速率即风暴 / 自环告警锚点；`torchwood_functions_event_backfill_total` 补投计数非零即告警锚点（停机窗口可视）。
- **触发器生效延迟**：订阅匹配器是 15s 周期全量快照（任一项目扫描失败保留旧快照）——创建 / 启停触发器到生效有一个刷新周期的传播窗口，窗口内的事件对新触发器不补投；重启即全量重建。

### 13.4 微信 SSV 接入配方（狗粮验收锚点）

微信小程序广告激励视频服务端回调（SSV）：用户看完广告 → 微信回调开发者 URL（1s 超时 × 重试 3 次）→ 验签通过回复 `{"is_valid":true}`。端到端链路：

1. **建触发器**：`POST /v1/server/functions/{function_id}/triggers`，`type=http`，`response_mode=async_ack`（1s 窗内同步执行大概率全超时致事件丢失），`ack_body={"is_valid":true}`，`handshake=echo`。得到 `invoke_path=/f/{project}/{token}`。
2. **微信侧配置**该 URL（GET 握手由平台 echo 回显完成，函数不参与）。
3. **函数内处理**（`TW_DATA` 即封套）：验签——`raw_query` 取 `signature/timestamp/nonce`，`body` 取加密报文，按微信广告 SSV 规范做 `sha256(sort(query)+body+app_secret)` 比对（app_secret 放 `function_variables`，不要硬编码）；去重——以报文内 `transaction_id` 幂等（可用 `databases` scope 写一条 `{_id: transaction_id}` 去重表，重复回调插入冲突即跳过）；AES 解密拿 `user_id`，再以 execution principal（`assets:write`）调 Server API `assets grant` 给用户发奖；账本 `operator` 记录自动溯源到 function + user。
4. **注意**：async_ack 的 200 是"已受理"语义——验签失败的回调无法在响应中拒绝，兜底是函数幂等 + 微信"前端先发奖、SSV 对账补偿"的推荐策略。

### 13.5 cron 验收锚点

每日 / 赛季重置建 `type=cron` 触发器（如 `0 3 * * *`），验证：宕机 2 天后 worker 恢复，`catch_up_once` 恰补跑 1 次（`scheduled_for` 为原计划时刻）且 `next_run_at` 直接指向下一日 03:00；`skip` 模式只推进不补跑。

## 14. 客户端调用（END_USER 按函数策略同步调用）

客户端调用面是带独立策略门的**新入口**，不是 Server 面 `CreateExecution` 的放开：终端用户（Client API Bearer 登录态）可调用显式开启 `client_callable` 的函数，身份（project / user）取自 Principal、请求体不携带身份。执行走既有参数化核心路径（`internal/app/functions/clientinvoke.go` → `createExecution`），执行身份铸造、两写预占记账、账本 operator 溯源全部自动生效。审计载体 = `function_executions` 行（`trigger_source='client'` + `invoking_user_id`），该 RPC 在 AuditInterceptor 跳过清单内。

### 14.1 API 与 SLA

- `POST /v1/functions/{function_id}:invoke`（`proto/client/v1/functions.proto`，`ACCESS_END_USER`，gRPC 与 gateway 双注册）。请求：`function_id`（必填）、`data`（JSON object 字符串，≤32KB——**不放宽**，触发器的封套通道放宽不适用不可信调用方）、`deployment_id`（可选，须 ready）、`idempotency_key`（可选，≤128 字符）。
- 响应：`execution_id` / `status`（`completed | failed | running`——running 只在幂等命中且执行仍在进行时出现）/ `response`（stdout 末行 JSON）。**函数执行失败是结果而非传输错误**（HTTP 200 + `status=failed`）；容器 exit code 对客户端无语义，不暴露。
- **SLA**：热路径同步分发简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）；**冷启动与实例排队显式不在 SLA 内**——需要 sub-100ms 的函数设 `min_instances ≥ 1` 保温。超时沿用同步路径 ≤30s。

### 14.2 每用户限频（平台强制）

- 策略列（projectschema 迁移 000016，函数级）：`client_callable`（存量全 FALSE ⇒ fail-closed）、`client_per_user_limit`（≥1 才可开启 callable）、`client_limit_window ∈ {minute, hour, day}`（day 按 UTC 日期）。管理经 Server 面 CreateFunction / UpdateFunction；Console 函数详情页"客户端调用"卡片同源。
- **窗口语义**：Redis 固定窗口 `torchwood:fnq:{project}:{function}:{user}:{bucket}`（minute=`YYYYMMDDHHMM`、hour=`YYYYMMDDHH`、day=UTC `YYYYMMDD`）；**先限频后执行**。
- **超限错误形态**：`ResourceExhausted` + `ErrorInfo.Reason = FUNCTIONS.INVOKE_QUOTA_EXCEEDED` + `RetryInfo`（窗口结束时刻）——配额超额**不是响应字段**。HTTP 侧映射 429。
- **故障降级（fail-closed）**：Redis 不可用时改查 `function_executions` 窗口内计数（partial 索引支撑）；**DB 亦不可用 = 拒绝（Unavailable）**——经济语义的限频不做 fail-open 熔断。依赖保留分级（§10：client 行 48h 时间窗保留）保证计数行不被条数 prune 裁掉。
- 匿名：`client_anonymous_allowed` 字段保留、**一期禁用**（管理面遇 true 显式报错）——匿名会话可无限新造，per-user 限频对匿名形同虚设；后续放开时需叠加按 IP 计数兜底。

### 14.3 幂等键语义与重试指引

- `idempotency_key` 提供时，预占 INSERT 携带 `client_idempotency_key`——partial 唯一索引 `(project_id, function_id, invoking_user_id, client_idempotency_key) WHERE <> ''` 在预占时刻生效，并发同键第二请求 INSERT 冲突即**回读既有行原样返回**（running 返回 running，completed 返回含 response 的终态；SDK 侧有幂等命中标记）。未提供 key 行为不变。
- **重试指引**：网络超时 / 5xx 后携带**同一 key** 重试是安全的（不会重复执行）；不同业务操作必须换 key。**平台级去重之外，函数侧幂等仍是双保险**——"调用即发资产"的函数应以 `execution_id` 派生资产幂等键，防把同一 key 复用到不同业务动作。
- 注意：重复请求（同键）也会消耗一次限频配额（先限频后执行的既定次序）——高频重试方应退避重试而非立即重放。

### 14.4 每用户并发闸门

- dispatcher 池上限 / 有界排队之外的第三道门：每用户并发上限（默认 2，`functions.client_invoke.per_user_concurrency`），进程内 keyed 信号量（channel），排队队首超时（默认 5s）→ ResourceExhausted。
- **诚实声明：per-process 语义**——多实例部署下为"近似全局"（全局上限 = 上限 × 实例数）。跨进程精确闸门需要 Redis 分布式信号量，但其排队 + 队首超时语义在热路径上不划算，本期取舍为进程内实现 + 文档明示。

### 14.5 egress 策略与部署要求（不可信函数默认 deny）

- **分类**：不可信 = `client_callable == true` **或** 存在 http / cron / event 触发器（含禁用——按行存在性分类更保守；事件源是终端用户写入，同属不可信触发面）；可信 = 其余（仅 server key 触发）。分类是**函数属性**，对该函数的所有触发来源一致生效，在 app 层完成（30s 缓存摊薄触发器查询），随执行规格传给 executor。
- **实现**：不可信函数容器 attach **internal 变体网络** `tw-func-<project>-int`（docker `internal: true`——阻断外网出口、网内互通保留）；分类与选网在 dispatcher 侧执行。指标 `torchwood_functions_egress_class_total`。
- **一期语义（诚实偏离）**：per-function 域名级白名单需要 egress 代理原语，一期不实现——语义退化为 **trusted / untrusted 二分类**：不可信函数出网**全 deny**（含第三方 API），可信函数保持放开。需要外呼第三方（如支付网关）的函数不要开启 client_callable / 触发器。
- **部署要求（dokploy compose 已接好；手动部署必读）**：`functions.execution.api_base_url` 必须填**函数网络内可达**的 Server API 地址——不可信函数在 internal 网络上无 NAT 出口，外部域名 / IP 均不可达。compose 接线：dispatcher 配置 `TORCHWOOD_FUNCTIONS_DISPATCHER_CALLBACK_CONTAINER=torchwood-server`，join 每个函数网络时把 server 容器一并 attach，`api_base_url = http://torchwood-server:9080`。attach 失败仅告警不阻断执行，但不可信函数的平台调用将连接失败——部署后用一条带 declared_scopes 的函数实跑验证回访连通性。

### 14.6 客户端接入示例

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
// 429 = 超限：读 Retry-After，窗口结束后重试（可保留同幂等键）
```

Go SDK：`client.New(...).Functions.InvokeString(ctx, "daily_signin", data, idempotencyKey, "")`。

### 14.7 rpc 调用封套 = JSON-RPC 2.0 请求对象

多操作函数的入口分发约定：调用方调用函数时，`data` **直接采用 JSON-RPC 2.0 规范的请求对象**：

```json
{
  "jsonrpc": "2.0",
  "method": "sum",
  "params": { "a": 1, "b": 2 },
  "id": "<请求id>"
}
```

- **`jsonrpc` 成员兼任触发判别**：事件投影（§13.3）带 `type:"event"` 而非 `jsonrpc`，函数入口据此一分到底，无需额外路由字段。
- 不使用规范中的 notification（无 `id` 即无应答）——调用方传输层严格请求 / 应答，`id` 恒存在（字符串形式）；`params` 恒为命名（对象）形式。
- 多操作函数（或多触发器同居）在入口统一判别分发——一个函数镜像（一个暖实例池）暴露多个 method，避免一 method 一函数的构建与保温成本：

```js
// index.js —— event + rpc 同居分发的参考形态
exports.main = async (data, ctx) => {
  if (data && data.jsonrpc === "2.0") {
    const h = handlers[data.method];
    return h ? h(data.params, ctx) : { error: { code: -32601, message: "method not found" } };
  }
  switch (data && data.type) {
    case "event": return handleEvent(data, ctx);       // 事件投影（§13.3）
    default:      throw new Error("unknown invocation");
  }
};
const handlers = {
  sum: (params) => ({ sum: params.a + params.b }),
};
```

- **应答约定（result / error 二分）**：函数返回值即响应的 `result` 成员；返回 `{"error":{"code":<int>,"message":<string>,"data"?:<any>}}` 形状即 `error` 成员（恰含其一，永不共存）。authored 错误码建议用保留区 `-32000..-32099`；分发级失败复用规范自带码（`-32601` method not found、`-32602` invalid params）。合法结果恰为该形状时须嵌套一层。
- **身份不入请求对象**：客户端可绕过调用方直调本面伪造任意 `data`，封套是路由约定不是信任边界——method 级鉴权与审计读 `ctx.invokingUserId` / `ctx.source`（§4.3.3，平台可信注入）。

## 相关文档

- `06-databases.md` §8.1 — execute-tx 事务内核（函数多写原子性的承载）
- `05-authentication.md` — `RequireServerPrincipal` 与 execution principal 凭证族
- `07-storage.md` — Redis 原子语义对照（分片锁）
- `docs/design/functions-execution-identity-and-triggers.md` / `docs/design/functions-v3.md` / `docs/design/functions-runtimes-and-sources.md`（§2 git 部署源）— 设计文档
