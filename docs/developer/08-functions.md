# 08 Functions：常驻服务调用、触发器与 Webhooks（v4）

> **v4 重写（2026-10-15）**：本章按 Functions v4 终态重写——执行面（打包/构建/部署/池/运行时/变量）已整体外包，dispatcher / packer 进程与部署/变量/运行时 RPC 退役；函数 = 常驻服务（endpoint target），torchwood 收敛为纯「路由与身份层」，Webhooks 升一等公民。设计全文（裁决链/复杂度审计/迁移记录）见 **`docs/design/functions-v4.md`**；旧版执行器/池/构建章节随执行面消解删除，历史形态阅读旧设计文档（`functions-v3.md` 及此前谱系）。

面向后端开发者：Functions v4 的能力面（endpoint target / 触发器三型 / 调用面 / invocation 记账）、调用协议（RPC 轴 JSON-RPC 2.0 + 事件轴 CloudEvents 1.0）、FDK（Go + TS）指引与 Webhooks 子系统。

> 源码锚点：`internal/domain/functions/`、`internal/app/functions/`、`internal/infra/functions/`（Invoker 出站投递引擎）、`internal/app/webhooks/`、`internal/infra/webhooks/`、`internal/pkg/ssrfguard/`、`worker/`（异步调用 / cron / 事件 / webhook 投递）、`sdk/go/functions`（Go FDK）、`sdk/typescript/functions`（TS FDK）。
> 阅读顺序建议：`06-databases.md`（三层与 outbox 事件脊柱）→ 本章 → `09-api-guide.md`。

## 1. 终态能力面

```
  客户端 SDK / CLI ──▶ InvokeFunction（幂等 / 限频 / 并发闸）
  HTTP 触发器 ───────▶ 触发器调度：http / cron / event
                      身份：twx_ 执行身份铸造 + scopes + 签名注入
                      记账：function_invocations 两写 + 计量
                      事件投递：Webhooks（一等公民）+ 函数事件触发
                                │ 签名 HTTP（§3 协议）
                                ▼
                      函数 = 常驻服务（跑 FDK，任意 substrate）
                      Webhook 接收方（任意 HTTPS 端点）
                                │ 部署/构建/池/扩缩
                                ▼
                      fleetly CLI（参考部署链）/ 用户自有工具链（k8s/裸机）
```

torchwood 保留的每一块都是 BaaS 本分（路由、身份、记账、投递）；外包的每一块都是 PaaS 机器（打包、构建、镜像、池、冷启动、运行时）。**torchwood 不编排任何工作负载，不持有任何用户代码字节。**

| 能力 | v4 终态 |
|---|---|
| 打包/构建/部署 | **零**——fleetly CLI 或用户自有工具链（`functions deploy` / deployments RPC 已退役） |
| 执行底座 | **零**——目标恒为常驻服务，无唤醒无保温（dispatcher / 池策略已退役） |
| 运行时/变量/资源规格 | **零**——归 substrate（fleetly env 与副本数 / k8s HPA / 用户自管；variables RPC 已退役） |
| 触发器调度（http/cron/event） | **保留**（语义不变，载体改 endpoint 调用，§5） |
| 客户端调用面 + twx_ 执行身份 | **保留**（注入通道改签名请求头，§4.2 / §6） |
| invocation 记账 + 计量 | **保留**（两写骨架 + 幂等/限频/审计三链，§8） |
| 事件 → 外部投递 | **一等公民 Webhooks**（§7） |

一次调用统一为 **invocation**（v4 §2 重命名：`CreateExecution` → `InvokeFunction`、`Get/ListExecutions` → `Get/ListInvocations`、表 `function_executions` → `function_invocations`；状态集收敛 `queued / running / completed / failed`——`building` 态随补构建退役）。`execution` 身份词汇保留（`twx_` 前缀、`fromExecution`、执行 principal）——指代「某次执行的身份上下文」，与代码在哪跑无关。

## 2. 快速上手

### 2.1 创建函数（登记 endpoint target）

```bash
./bin/torchwood functions create \
  --id greet --name greet \
  --url https://fn.example.com/greet             # 生产强制 https
# → 响应一次性回显 target 签名密钥（twfnsec_ 前缀明文，仅此一次）
```

签名密钥服务端生成、加密落库，API 面只出指纹（`secret_fingerprint` = `twfnsec_` + SHA-256 前 8 hex）；**明文仅在创建响应出现一次**，立即配置到函数侧（FDK 环境变量或部署注入）。也可经 `--target-secret` 录入自有密钥。泄漏/换钥走 `torchwood functions rotate-secret <id>`：新钥即刻生效（v1 签名），旧钥保留一个宽限窗（v2 双签，接收方不断流），新明文同样只在轮换响应出现一次。

也可以走 REST / SDK（`proto/server/v1/functions.proto`，REST 形状以 genproto swagger 为准）：

```ts
import { Torchwood } from "@torchwoodcloud/sdk";

const tw = Torchwood.withApiKey(endpoint, projectId, apiKey);
const { function: fn, target_secret } = await tw.server.functions.create({
  id: "greet",
  name: "greet",
  timeout_seconds: 15,
  target_url: "https://fn.example.com/greet",
});
// target_secret 仅此一次，立即持久化到部署侧 secret 管理
```

### 2.2 调用（CLI / Server API）

```bash
./bin/torchwood functions invoke --input '{"ping":"world"}' greet
./bin/torchwood functions invoke --async --input '{"ping":"world"}' greet   # 立即返回 queued
./bin/torchwood functions invocations list greet --status failed
./bin/torchwood functions invocations get greet <invocation-id>
```

`InvokeFunction`（`POST /v1/server/functions/{function_id}:invoke`）校验 `data ≤32KB` 且为 JSON object；同步调用超时上限 30s（网关余量），超时场景用 `async=true` 入队走异步。

### 2.3 终端用户调用（客户端调用面）

显式开启 `client_callable`（每用户限频配额 + 窗口必配）后，Client API 登录态可同步调用：

```ts
const tw = Torchwood.withAccessToken(endpoint, projectId, accessToken);
const res = await tw.functions.invokeFunction("daily_signin", {
  data: JSON.stringify({ day: "2026-10-15" }),
  idempotency_key: "signin-u1-20261015",   // 幂等键防网络重试重复执行
});
// res.status: completed | failed | running（幂等命中且进行中）
```

平台强制每用户限频（超限 `429` + `RetryInfo` 窗口结束时刻）与每用户并发闸门（默认 8）；调用目标恒为函数当前 endpoint target。详见 §6。

### 2.4 invocation 记账

每次调用（server / client / 三型触发器）落 `function_invocations` 行：status、response（≤1MiB 截断，`response_truncated` 标记）、status_code、duration_ms、error。**torchwood 只见网关往返结果**——stdout/stderr 已随执行面外包退役，函数日志归 substrate 侧（fleetly logs / 用户服务自管）。

## 3. 调用协议摘要（D1 + D10 双轴）

协议按语义轴双轨，判定准则 =「有没有调用方在等应用层结果」：

| 轴 | 协议 | 通道 | 结果去向 |
|---|---|---|---|
| **RPC 轴**（请求-响应，有 id） | JSON-RPC 2.0 | `invoke`（client/server，含 async）、`http.request`（HTTP 触发器 sync 与 async_ack 统一） | 调用方 / invocation 行 |
| **事件轴**（通知，无应用层结果） | CloudEvents 1.0 structured mode | `cron.tick`、事件触发（document/system）、webhook 投递 | 2xx = 送达；非 2xx 按重试梯度重投 → 终态失败行 = DLQ |

FDK 单端点按 body 探测分流（有 `jsonrpc` → RPC 轴、有 `specversion` → 事件轴）；同一签名方案罩两面。

### 3.1 RPC 轴：JSON-RPC 2.0 over 签名 HTTP

请求 `POST {target.url}`，method 承接调用语义路由：

| method | params | 对应场景 |
|---|---|---|
| `invoke` | 函数载荷（原 TW_DATA JSON） | client / server 调用面（FDK Invoke handler） |
| `http.request` | `{envelope, body_b64}`（触发器封套，§5.1） | HTTP 触发器（sync 与 async_ack 同形态） |

- `id` = invocation id（请求关联白送，FDK 回显进响应）。
- 响应恒 HTTP 200（送达成功），函数成败在 body：`{"jsonrpc":"2.0","id":"…","result":<函数返回值>}`；`http.request` 的 result 为 `{"status":201,"headers":{…},"body_b64":"…"}`（fetch 语义）；函数应用错误 = `error` 对象（server error 区段 -32000..-32099，torchwood 不消费语义、只透传记录）。
- **平台错误 vs 函数结果边界**：非 2xx / 连不通 / 超时 / 验签协议不符 = 平台语义错误（`Unavailable` / `DeadlineExceeded`，HTTP 触发器应答 502/504）；2xx + JSON-RPC body = 函数结果（含 error 对象）。
- 大小上限：请求 body ≤4MB（HTTP 触发器封套双编码口径，per-trigger `body_limit_bytes` 缺省 64KB / 上限 1MB）；响应 ≤1MiB（超限 `response_truncated`）。

### 3.2 事件轴：CloudEvents 1.0 structured mode

投递引擎 → 消费方 endpoint，body 为 structured mode（全部属性在 body 内，整体落在签名保护圈）：

```json
{
  "specversion": "1.0",
  "id": "<事件投递 ID>",
  "source": "projects/{p}/triggers/{t} | projects/{p}",
  "type": "cron.tick | documents.created | auth.users.created | …",
  "time": "<RFC3339>",
  "datacontenttype": "application/json",
  "data": { "…CronTick / 事件投影…" },
  "twproject": "<project id>",
  "twseq": 42
}
```

- 响应语义：**2xx = 送达**（FDK 回 204，无应用层结果）；非 2xx / 超时 / 连不通 = 失败，按重试梯度重投（webhook 与函数事件触发同一管道语义）。
- 事件轴投递行不记录函数返回值（响应 body 尾部截断留档作审计替代）。
- data 投影 ≤32KB 与函数事件触发器同口径：文档事件只带 ID + 摘要，全量按 `document_id` 用 `databases:read` 回读；系统事件 data 携带脱敏 `attrs` 本体。

### 3.3 签名与身份头（两轴同一方案）

| header | 语义 |
|---|---|
| `X-Tw-Signature` | `t=<unix秒>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>`——Stripe 风格；±5 分钟时间窗防重放；验签方常数时间比较；轮换宽限窗内旧钥以 `v2=` 双签并存 |
| `X-Tw-Protocol-Version` | `1`——管 method 词表 / 头语义演进（JSON-RPC 本体自版本化） |
| `X-Tw-Execution-Token` | `twx_…` 执行身份（TTL = 超时 + 60s，执行结束即吊销；§4.2） |
| `X-Tw-Source` / `X-Tw-Invoking-User-Id` / `X-Tw-Project-Id` | 身份投影：source ∈ client / server / `http\|cron\|event:{trigger_id}`（恒非空）；invoking_user_id 空 = 系统语义；op 级鉴权以此为准 |

## 4. FDK 指引（Go + TS）

FDK 是协议参考实现：Go **`github.com/torchwoodcloud/torchwood/sdk/go/functions`**（独立 module、仅标准库），TS **`@torchwood/functions`**（`sdk/typescript/functions`，零运行时依赖）。v4 双轴统一入口 = **验签中间件 + method 分发**。

### 4.1 双轴入口（验签 + 分发）

Go——`NewServer` 组装「验签 → body 探测 → 分发」的 http.Handler，可直接挂任意 `http.Server` / 云平台入口：

```go
package main

import (
    "context"
    "encoding/json"
    "net/http"
    "github.com/torchwoodcloud/torchwood/sdk/go/functions"
)

func main() {
    mux := functions.NewMux()
    mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) {
        // RPC 轴 invoke：函数载荷已解出（原 TW_DATA JSON）；身份读 functions.FromContext(ctx)
        return map[string]any{"pong": string(data)}, nil
    })
    mux.Cron(func(ctx context.Context, tick functions.CronTick) error { ... })       // 事件轴 cron.tick
    mux.Event(func(ctx context.Context, ch functions.DocumentChange) error { ... })  // 事件轴文档/系统事件
    mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... })) // RPC 轴 http.request
    // 验签 + 双轴分流一体：secrets 为函数侧密钥集（轮换窗口内新旧并存）
    http.ListenAndServe(":8080", functions.NewServer(mux, secret))
}
```

`functions.Verify(secrets...)` 可单独作中间件使用（校验 `X-Tw-Signature`：时间窗 + 常数时间比较 + 多密钥任一命中，失败 401）。统一入口内置可选探针 `GET /_tw/health`（不走验签，部署平台探活用）与 4MB body 上限，handler panic 捕获为 error 对象（RPC 轴）/ 400（事件轴），不杀进程。单源便捷封装 `StartInvoke / StartCron / StartEvent / StartHTTP(handler, secrets...)` 与阻塞形态 `Listen(mux, secrets...)` 全部构建在 `NewServer` 之上（同协议、验签必经）：`Listen` 绑定 `TW_RUNNER_PORT`（缺省 18080，v4 语义 = 常驻服务监听端口），SIGTERM/SIGINT → 优雅排水（停止接新 + 等在途完成 + `TW_DRAIN_TIMEOUT_MS` 兜底强退）。

TS——`newServer(mux, ...secrets)` 同构：

```ts
import { newServer, Mux } from "@torchwood/functions";

const mux = new Mux();
mux.invoke(async (data, ctx) => ({ pong: data }));
mux.cron(async (tick, ctx) => { ... });
mux.event(async (change, ctx) => { ... });

export default newServer(mux, process.env.TW_FUNCTION_SECRET!);
```

TS FDK 同时导出 `verify` / `computeSignature` / `verifySignatureHeader`（自定义接入面）与 `RPC_METHOD_INVOKE / RPC_METHOD_HTTP_REQUEST` 常量。dogfood 函数是 JS——**TS FDK 是一期硬依赖**（桥接全靠它）。

签名口径逐字节跨仓钉死：载荷串 `"<t>.<body>"`、HMAC-SHA256 hex、头格式 `t=/v1=/v2=`、±5 分钟窗——平台 `internal/domain/functions/protocol.go` 与两侧 FDK 共享基准向量测试，任一侧漂移即红。

### 4.2 执行身份（execution principal）

每次调用平台铸造短期 `twx_` token 经 `X-Tw-Execution-Token` 头注入（Go 读 `functions.FromContext(ctx)`、TS 读 handler `ctx.identity`）。服务端 Redis 投影 `{project_id, function_id, invocation_id, scopes, invoking_user_id}`，TTL = 函数超时 + 60s，执行结束即吊销。FDK 身份六件 = 四个身份头 + `executionId`（RPC 轴 = JSON-RPC `id`、事件轴 = CloudEvent `id`，日志关联 / 幂等键推荐来源）+ `apiBaseUrl`（部署侧注入的 `TW_API_BASE_URL`，见 §4.3）。

**declared_scopes（最小特权，默认空 = 无任何平台访问）**：

- 每项形如 `<resource>:<op>`；resource 白名单 = **assets / databases / users / groups / storage / subscriptions / payments**（平台与编排面资源一律拒绝）；op ∈ {read, write}。管理经 `CreateFunction.declared_scopes` / `SetFunctionScopes`（全量替换）；Console 详情页「执行身份 Scopes」卡片同源。
- scope 门与 API key 完全同构（`PolicySet.AllowsAPIKeyTargets`，含实例寻址语义）；Redis 不可用 = 拒绝（fail-closed）。
- **数据面语义**：scope 门只是第一层——文档/存储可见性基于角色 `key:function:<function_id>`，使用 `databases:*` / `storage:*` scope 前必须把目标集合/桶 ACL 授予该角色，否则 scope 过门但数据不可见（空结果最难排查）。
- 函数内平台回访：`functions.NewClient(ctx)`（Go，fail-closed 构造）/ `new Client(ctx.identity)`（TS），`Authorization: Bearer twx_…` 调 Server API；base 取身份投影的 `apiBaseUrl`——**由部署侧注入的 env `TW_API_BASE_URL` 供给**（v4 下 env 归部署侧下发，如 fleetly app env / compose；未注入时 Go 构造即报错、TS 请求期抛错，均指明部署需注入该 env）。限流维度 `api:execution:<project>:<function>`（默认 6000/min）。

### 4.3 部署：fleetly CLI 参考路径 / 任意 substrate

v4 彻底 substrate-blind——函数常驻服务就是一个 URL 后面的进程，部署链完全归用户：

- **参考路径（fleetly 托管）**：FDK 常驻服务打成 fleetly app（`fleetly` CLI deploy / compose / build-from-upload），密钥与平台 API base 经部署 env 下发（`TW_FUNCTION_SECRET` + `TW_API_BASE_URL`），把 fleetly 给出的稳定服务 URL 登记为 torchwood 函数的 `target_url`。dogfood 函数即按此路径部署为常驻 app。私网寻址（fleetly 任务网内网服务名解析为 RFC1918 私址）需运营方显式开启 `functions.invoke.allow_private_targets`（平台级信任决策，见 §9）。
- **任意 substrate**：k8s Deployment + Service、裸机 systemd、Cloud Run——同一条签名 HTTP 协议，torchwood 零感知。
- 池策略 / 副本数 / 扩缩 / 运行时版本 / 资源规格在**真正拥有它们的地方**配置：fleetly app 副本数、k8s HPA、用户自管。torchwood 不代理任何基础设施旋钮。
- 常驻即无唤醒：torchwood 不做 0→1 唤醒、不保温、不排队；超时 = 函数记录的 timeout 透传为请求超时，目标过载/失联 = 快速失败。

### 4.4 本地开发

暂无 `functions dev` 命令（CLI 的 dev 通道随 v4 部署面退役未补）。本地联调的真实路径：本地起 NewServer（Go `go run .` + `functions.Listen(mux, secret)` / TS 裸 `node:http` 挂 `newServer(mux, secret)`），注入部署 env（`TW_FUNCTION_SECRET` + `TW_API_BASE_URL`），再用 `BuildSignatureHeader`（Go）/ `buildSignatureHeader`（TS）对请求自签调用（载荷串 `"<t>.<body>"`，±5 分钟窗）——SDK 测试套件即按此口径全链自测，可直接照抄。探针 `GET /_tw/health` 不走验签，可作就绪检查；`TW_RUNNER_PORT` 语义 = 常驻服务监听端口（缺省 18080），`TW_DRAIN_TIMEOUT_MS` 控制排水窗。

## 5. 触发器（http / cron / event）

实体 `function_triggers`：`type ∈ {http, cron, event}`，config JSONB 分型存储；http 的 token 为独立列（`UNIQUE(token)`）。管理面四 RPC：`CreateFunctionTrigger` / `ListFunctionTriggers` / `DeleteFunctionTrigger` / `RotateFunctionTriggerToken`（token 轮换仅 http，旧 token 立即失效）；函数删除 FK 级联清理。三型触发的执行统一走 endpoint 调用：http 走 RPC 轴 `http.request`，cron/event 走事件轴 CloudEvents（§3），投递重试由统一出站引擎承接（非 2xx 按梯度重投 → 终态失败行可查）。

### 5.1 HTTP 触发器（公开 URL）

- 路由 `/f/{project_id}/{trigger_token}`：token 128bit 随机、不可猜即鉴权；URL 含 token 会被代理/访问日志记录，疑似泄漏即轮换。
- **只路由不验签**：请求以封套透传进 `http.request` params（FDK fetch handler 还原真 `*http.Request`）：

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

  **raw_query 必须透传**——微信 SSV 的验签参数在 query。headers 白名单（统一小写键）：全部 `x-*` + `content-type` + `wechatpay-*` 前缀，其余剥除；body 双通道（`body` best-effort UTF-8 + `body_base64` 恒在无损）。平台不解析 body、日志不落原文。
- **双响应模式**（创建时必选 `response_mode`）：`sync` 同步执行（≤30s，handler 预算 35s）并完整透传函数响应（fetch 风格 status/headers/body 全透传；failed → 502、超时 → 504）；`async_ack` 先入队成功后写 200（顺序红线，入队失败一律 503 让回调方重试），200 body = `ack_body`（≤1KB）——**async_ack 的 200 是「已受理」而非「验证通过」**，函数内验签失败无法追回响应，需配合幂等。
- **GET 握手**：`handshake: "echo"` 时平台直接回 `{"echostr": <query.echostr>}`（不 invoke）；非 echo 模式 GET → 405。
- **body 上限**：缺省 64KB、per-trigger 可配至 1MB（`body_limit_bytes`），超限 413。
- **per-IP 限频**：Redis 固定窗口，默认 3000/min（`functions.trigger.http_ip_per_minute`），超限 429 + Retry-After，Redis 故障 fail-closed；限频先于 token 查找。
- 封套经签名 HTTP 送达 target 后，**传输层验签归平台注入的 `X-Tw-Signature`**；回调方自身的业务验签（如微信 sha256 比对）在函数内用封套原文完成。

### 5.2 cron 触发器

- 表达式：5 字段（分 时 日 月 周）**UTC**；支持 `* , - /` 与数字；周 0-7 且 7≡0；越界解析报错（fail-closed）。解析器 `internal/domain/functions/cronexpr.go`（零依赖，DOM 与 DOW 均受限取并集）。
- 调度：worker 每分钟 ticker → 按项目轮转扫描（全局预算 100/轮）→ `ClaimDueCron` 原子领取；**先 CAS 后入队**（多实例只有赢家）；misfire 默认 `catch_up_once`（宕机 N 周期只补跑 1 次，`scheduled_for` 为原计划时刻；`skip` 只推进不补跑）。
- 入队事件 `type=cron.tick`，data 携带 `{trigger_id, scheduled_for}`——`scheduled_for` 是幂等键推荐来源。

### 5.3 事件触发器（文档写事件 + 系统行为事件）

链路：写事务内 outbox 行 → OutboxWorker XADD `torchwood:events` → `functions-triggers` 消费组（worker）→ 订阅匹配 → 命中即按事件轴投递到 endpoint（at-least-once，与 Webhooks 同一投递管道语义，§7.2）。

- **订阅串双形态**（`internal/domain/functions/eventmatch.go`，与 Webhooks 订阅同语法）：
  - 文档写事件 `databases.{database_id}.collections.{collection_id}.documents.{op}`（op ∈ create|update|delete|*；collection 与 op 段可 `*`，**database 段一期必须精确**）；
  - 系统行为事件 `{domain}.* | {domain}.{resource}.* | {domain}.{resource}.{op}`，域词表 = 平台系统事件目录（`internal/domain/events/catalog.go`：auth / payments / economy / subscriptions）；`*` 仅允许尾段；目录内无命中的前缀直接拒绝（拼错立刻 400）。
- 一个触发器 ≤64 条订阅串（单条 ≤200B）；同一事件命中多条订阅串会多次投递，函数幂等吸收。幂等键推荐来源 = data 的 `event_id`（seq 是集合内分配序、有空洞）。
- **权限不豁免**：文档回读走 execution principal + RLS 完整链路——需 `databases:read` scope **且**目标集合 ACL 授予 `key:function:<id>` 角色。
- **自环警告（无硬防护）**：订阅本函数自身写入的集合 → 写 → 事件 → 再触发循环无限放大；Console 编辑处文案警告，`torchwood_functions_event_deliveries_total{source=event}` 速率即风暴告警锚点。
- **停机补投**：worker 以水位键 `torchwood:fnevent:lastseq` 与 Stream 现存首条比较，裁剪缺口从 outbox 按 seq 区间补投（>24h outbox 窗口的极端停机才真正丢失）。

## 6. 客户端调用面（END_USER）

`POST /v1/functions/{function_id}:invoke`（`proto/client/v1/functions.proto`，`ACCESS_END_USER`）：终端用户（Bearer 登录态）可调用显式开启 `client_callable` 的函数，身份取自 Principal、请求体不携带身份。函数执行失败是结果而非传输错误（HTTP 200 + `status=failed`）。

- **策略列**（函数级，管理经 CreateFunction / UpdateFunction）：`client_callable`（默认 false，fail-closed）、`client_per_user_limit`（≥1 才可开启）、`client_limit_window ∈ {minute, hour, day}`（day 按 UTC 日期）。
- **每用户限频**：Redis 固定窗口 `torchwood:fnq:{project}:{function}:{user}:{bucket}`，先限频后执行；超限 `ResourceExhausted` + `ErrorInfo.Reason = FUNCTIONS.INVOKE_QUOTA_EXCEEDED` + `RetryInfo`（HTTP 429）。故障降级 fail-closed：Redis 不可用查 `function_invocations` 窗口内计数（partial 索引），DB 亦不可用 = 拒绝。匿名 `client_anonymous_allowed` 字段保留、一期禁用（置 true 显式报错）。
- **幂等键**：`idempotency_key`（≤128 字符）预占 INSERT 携带，partial 唯一索引 `(project, function, user, key) WHERE <> ''` 冲突即回读既有行原样返回（running 返回 running）。网络超时/5xx 后**同 key 重试安全**；不同业务操作必须换 key；重复请求也消耗限频配额（先限频后执行）。「调用即发资产」的函数以 `invocation_id` 派生资产幂等键双保险。
- **每用户并发闸门**：进程内 keyed 信号量（默认 8，`functions.client_invoke.per_user_concurrency`），队首超时 10s → `ResourceExhausted`；多实例部署下为近似全局（诚实声明）。
- **SLA**：热路径同步调用简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）；目标服务冷启动不在 SLA 内（常驻保温归 substrate）。

## 7. Webhooks（一等公民，D5）

事件反应类需求从此**无需在平台跑代码**：订阅 outbox 事件脊柱，按 CloudEvents 1.0 structured mode 投递到用户配置的 HTTPS endpoint。proto：`proto/server/v1/webhooks.proto`（九 RPC，scope 资源复用 functions）。

### 7.1 订阅模型与语法

- `Webhook` = URL + 订阅串集 + 签名密钥 + 状态（enabled / paused）。**url 强制 https**；订阅串复用 eventmatch 内核（与函数事件触发器同款语法，§5.3：文档写事件 + 系统行为事件双形态，1..64 条 × ≤200B，创建期逐条格式校验 fail-closed）。一期为路由级过滤（负载级过滤不做）。
- per-project webhook 数上限默认 32（`functions.webhooks.max_per_project`）。
- 管理面：`CreateWebhook / GetWebhook / ListWebhooks / UpdateWebhook / DeleteWebhook / RotateWebhookSecret / ListDeliveries / GetDelivery / RedeliverDelivery`。密钥服务端生成（`whsec_` 前缀），明文仅在**创建 / 轮换响应**一次性出现（之后任何接口只出 `secret_fingerprint`）。Console「Webhooks」页提供列表 / 创建编辑（url/订阅串/启停）/ 轮换 / 投递记录与 Redeliver；CLI 同构：

```bash
torchwood webhooks create --url https://hooks.example.com/cb \
  --events auth.users.created,databases.db.collections.col.documents.created
torchwood webhooks list | get <wh-id> | update [--url|--events|--status] <wh-id> | delete <wh-id>
torchwood webhooks rotate-secret <wh-id>          # 新明文一次性回显，旧钥宽限窗双签 v2
torchwood webhooks deliveries list [--status failed] <wh-id>   # DLQ 浏览
torchwood webhooks deliveries get|redeliver <wh-id> <delivery-id>
```

### 7.2 投递语义

- 消费 outbox 事件脊柱的新消费组 `webhooks`（与 `functions-triggers` 同模式：XREADGROUP、at-least-once、停机补投经 outbox 按 seq 区间回放，水位键 `torchwood:wh:lastseq`）。
- 载荷：CloudEvents 1.0 structured mode（§3.2，`type` = 事件目录名，`twproject` / `twseq` 扩展属性携带项目与全序）；签名同 §3.3（`X-Tw-Signature`，per-webhook 密钥）。
- **重试梯度（D5 拍板）**：即时首投 → 10s → 1m → 5m → 30m → 2h → 8h ×…（`functions.webhooks.retry_backoff` 可配），**约 15 次封顶 24h**（`max_attempts` 缺省 15、`retry_window` 缺省 24h，二者任一到达即终态 failed）；单次投递 HTTP 超时缺省 10s。
- 顺序：per-webhook at-least-once、尽力保序（不承诺严格序）——接收方以信封 `event_id` / `twseq` 做幂等。
- 配额与计量：投递量/时延一期仅进 metrics、不进 billing（D5）。

### 7.3 DLQ 与 Redeliver

- 终态失败行进 **DLQ**（`webhook_deliveries` 表 `status=failed`），`ListDeliveries` 可浏览：`status` / `attempts` / `last_response_code`（0 = 未达 HTTP 层——连不通/超时）/ `last_error` / `next_attempt_at`。投递行保留窗缺省 7 天（`functions.webhooks.retention`）。
- `RedeliverDelivery` 手动重投：终态行（delivered/failed）重置为立即到期（status=pending、attempts=0），语义 = 全新一轮投递。

### 7.4 签名验证（接收方指引）

投递头 `X-Tw-Signature: t=<unix秒>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>`（轮换宽限窗内旧钥 `v2=` 并存，宽限缺省 24h）。接收方：取原始 body → 计算 HMAC-SHA256 → 与 v1（及窗口内 v2）常数时间比对 → 校验 `t` 在 ±5 分钟内。`@torchwood/functions` 的 `verify` / `verifySignatureHeader` 与 Go FDK `functions.Verify` 可直接复用（多密钥任一命中）。

## 8. invocation 记账与保留

**两写预占骨架**（`INSERT running 预占 + 终态 UPDATE`）挂三条链：

1. **幂等**：客户端调用幂等 = 唯一索引 `function_invocations_client_idem_key` INSERT 冲突检测 + 按键回读（§6）。无 Redis 兜底——不写行幂等即破。
2. **限频降级**：Redis 限频器失效时走 DB 窗口内计数 `CountClientInvocations`（partial 索引）。
3. **审计**：`ClientInvoke` 在 AuditInterceptor 跳过清单内，invocation 行（含 invoking_user_id）就是审计载体。

同步调用跳过 queued 中间态直接预占 running；异步 `queued → running → completed|failed`；孤儿恢复（worker 每分钟，stale = timeout + 120s）收敛中间态遗留行。**保留策略**：client/http/cron 来源行按时间窗保留 48h（限频 DB 降级的窗口内计数依据）；server 来源每函数保留最近 100 条；`DeleteFunction` 级联清 invocations / 触发器。计量 `function_duration_ms` 语义 = **网关往返时长**（Invoker 侧计时）。

## 9. 配置

`internal/pkg/config/config.proto` 中 Functions 相关键（环境变量映射见 `03-configuration.md`）。**退役段**（reserved，残留键被忽略）：`functions.execution / docker / dispatcher / image / packer / storage / fleetly / driver`——`mise run build` 产出 **server / worker / torchwood CLI 三（+console 嵌入）**，无 dispatcher/packer。

| 键 | 说明 |
|---|---|
| `functions.trigger.http_ip_per_minute` | HTTP 触发器每 IP 限频，默认 3000 |
| `functions.client_invoke.per_user_concurrency` | 每用户并发闸门，默认 8 |
| `functions.client_invoke.queue_head_timeout` | 并发闸门排队队首超时，默认 10s |
| `functions.webhooks.retry_backoff` | webhook 重试梯度，缺省 `10s,1m,5m,30m,2h,8h`（末值循环） |
| `functions.webhooks.max_attempts` | 最大投递尝试次数（含首投），缺省 15 |
| `functions.webhooks.retry_window` | 重试窗（首投起算终态封顶），缺省 24h |
| `functions.webhooks.delivery_concurrency` | 单轮到期扫描投递并发，缺省 4 |
| `functions.webhooks.delivery_timeout` | 单次投递 HTTP 超时，缺省 10s |
| `functions.webhooks.retention` | 投递行保留窗，缺省 168h（7 天） |
| `functions.webhooks.secret_grace_period` | 签名密钥轮换宽限窗（v2 双签），缺省 24h |
| `functions.webhooks.max_per_project` | per-project webhook 数上限，缺省 32 |
| `functions.invoke.allow_private_targets` | 放行私网/回环 target（ssrfguard 拨号点守卫的显式开关）：仅自托管形态函数与 server 同处私网（fleetly 任务网 / k8s Service / 裸机内网）时由运营方开启；托管形态恒 false（默认） |

**SSRF 姿态**：server（sync invoke）与 worker（async/webhook 投递）是出站源；`internal/pkg/ssrfguard` 在拨号点强制（`GuardedHTTPClient`：拒 loopback / private / link-local / unspecified，覆盖云元数据端点；Control 钩子对 connect 时真实 IP 判定——DNS rebinding 天然失效，重定向后拨号同样受控）。用户配置的任意 URL 不放行私网，例外 = 上表 `allow_private_targets` 运营者显式决策。

## 10. 可观测性

- `torchwood_functions_execution_duration_seconds{project,function,source,status}` / `torchwood_functions_executions_total`（source = server|http|cron|client|event）、`torchwood_functions_invoke_total{result}`（触发器 + 客户端入口计数，result 词表 ok|quota|echo|not_found|error|timeout）、`torchwood_functions_queue_wait_seconds`（异步 queued→running）。
- 出站投递引擎：per-target-kind（function / webhook）时延 / 成功率 / 在途计数指标；webhook 投递/重投与事件触发器投递/补投各有 total 计数。
- outbox 四件（`torchwood_outbox_pending` / `_dead` / `_publish_total{result}` / `_publish_lag_seconds`）见 `06-databases.md`。
- SLA burn 告警锚点：热路径端到端 P99 > 100ms 或平台开销 > 25ms（v4 热路径少两跳——server → endpoint 直连）。

## 11. 声明边界（v4 不做什么）

- **不构建、不部署、不持有代码**：部署链 = fleetly CLI 或用户自有工具链；torchwood 零用户代码字节、零容器编排（dispatcher/packer/zipstore/runner 全部退役）。
- **不代理基础设施旋钮**：池策略 / 副本数 / 扩缩 / 运行时版本 / 资源规格归 substrate；scale-to-zero 若要做，是 substrate 在同一 URL 后面自己做的事。
- **不管理函数环境变量**：env 注入点 = 部署侧（fleetly 下发 / 用户自管）；平台能力访问用执行身份 scopes（§4.2），不要再把长期 API key 放进任何 env。
- **不做唤醒与保温**：常驻即无唤醒；目标过载/失联快速失败，不排队不熔断重试（webhook/事件轴投递重试除外，§7.2）。
- 事件订阅 database 段通配后置（当前必须精确 database）；Webhooks 一期为路由级过滤（负载级过滤不做）。
- 客户端匿名调用未开放（`client_anonymous_allowed` 保留、置 true 显式报错——放开时需叠加按 IP 计数兜底）。
- 调用面响应不流式（body 全缓冲 ≤1MiB）；`waitUntil` 后台任务不做。

## 相关文档

- `docs/design/functions-v4.md` — v4 设计全文（裁决链 D1–D10 / 复杂度审计 / 迁移记录）
- `06-databases.md` §8.1 — execute-tx 事务内核（函数多写原子性的承载）与 outbox 事件脊柱
- `05-authentication.md` — `RequireServerPrincipal` 与 execution principal 凭证族
- `12-sdk.md` — Go/TS SDK 与 FDK 包全景
- `09-api-guide.md` — RPC 面与 OpenAPI 建模
- `authz-matrix.md` — Functions / Webhooks 写方法的角色门禁
