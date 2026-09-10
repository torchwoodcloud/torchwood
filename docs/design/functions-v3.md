# Functions v3：并发复用、接口现代化与开发者体验

> 状态：**已批准（2026-09-10 owner 拍板：D1–D15 全部通过、OQ1–OQ9 全部收口；按 Rollout 顺序进入实施——A 切片一先行、B/DX 并行、C 接口待 A 落地、D/E 独立并行；C/D 合入前各过一轮安全自查）**
> 拍板记录（2026-09-10 owner）：concurrency CHECK 上限 **16**；池策略管理 API 面**随 v3 立项小切片**（UpdateFunction optional ×5 + Console 池策略卡片）；构建安全档位 **ignore-scripts 恒定 + 出网不限制**（allow_scripts 与 registry 白名单随 egress 原语后置）；实时日志流**后置到对外开放注册（多租户）前**；模板 **v3/v4 分立**；fetch invoke 路径 **headers/流式一期不做**；事件 per-trigger 并发限流**一期不做、指标先行**；stdout 混流旁路**不做**（per-request tail + docker logs 全量覆盖）
> 定位：Functions 子系统第三代——v1「每请求一容器」（CGI 形态，已裁决为设计缺陷）→ v2「常驻 runner + dispatcher 池」（已落地）→ **v3 = 实例内并发复用 + Web 标准接口 + 平台代装依赖 + 数据库事件触发器 + 开发者体验闭环**
> 依据：2026-09-10 竞品调研（通用 FaaS / BaaS 函数 / 自托管尸检三路）+ 同日深度复查（优先级与夸大项修正）。核心结论：执行底座（常驻容器池）是业界验证过的存活形态**不推翻**；「1 并发/实例串行」「自定义 JSON 协议」「无代装依赖」「无 DB 事件触发」「函数内裸 fetch / 无本地开发闭环」五项是与业界默认形态的真实差距
> 文档关系：本稿**吸收并取代**同日创建的 `functions-runner-concurrency.md`（§1 为其全文并入，单一事实源，该文档删除）；前置设计 `functions-execution-identity-and-triggers.md`（P0–P2 已落地，本稿是其 §6 执行器 v2 的继任演进 + Q12 重开收口）
> 范围裁决：node 运行时先行——python v2 尚未支持，其 runner 落地时直接实现本稿全部语义（并发 + fetch 双轨），模板版本同源
> 产品哲学不变（K6/K7）：本稿全部交付物均为**水平原语**；SLA 口径不变（热路径同步分发 P99 ≤ 100ms、平台开销 ≤ 25ms——多路复用改善吞吐与排队，不承诺单请求延迟）
> 对抗审查（2026-09-10，13 条攻击路径）：修复 2×致命——事件消费组停机静默丢失（XTRIM 不管消费进度，outbox 补投升为一期必做，§4.2/D12）、超时不杀实例的僵尸负载通道（per-instance 超时熔断，§1.4/D3，进切片一）；连带修正切片一"严格等价"的错误验收措辞、封套传递改 header、fetch env 表述、release Lua 固化字段、truncated 撞名（各节内联标注"对抗审查修正"）

---

## Overview

五大支柱，各有独立价值、独立切片、可独立灰度：

```
┌────────────────────────────────────────────────────────────────────┐
│                       Functions v3                                  │
├──────────────┬──────────────┬──────────────┬───────────┬──────────┤
│ A 实例内多路复用│ B Web标准接口 │ C 平台代装依赖 │ D DB 事件  │ E 开发者  │
│  (吞吐)       │  (表达力/生态)│  (构建体验)   │  触发器    │  体验     │
│  runner v3   │  runner v4   │  dockerfile  │  消费组    │  SDK/dev/ │
│  inflight 化 │  fetch 探测   │  分层+scripts│  订阅匹配  │  deploy  │
└──────────────┴──────────────┴──────────────┴───────────┴──────────┘
      ▲______________▲ 共用 per-request 基建（分桶日志/超时/env）
```

| 支柱 | 解决的问题 | 业界对标 |
|---|---|---|
| A 多路复用 | 1 并发/实例串行是吞吐天花板（8 实例 × 256MB 常驻内存下唯一低成本放大器） | Cloud Run 默认 80 并发/实例、Vercel Fluid、Lambda 2024-25 打破单环境串行 |
| B Web 标准接口 | `main(TW_DATA)` JSON 进出无法返回状态码/二进制/自定义头，npm web 生态（hono/zod/中间件）不可复用，curl 不可直调 | Supabase/Deno/Fluid 的 `fetch(Request)→Response`；Appwrite 结构化 context |
| C 代装依赖 | 用户自己打 zip 带 node_modules（跨平台不兼容、包体大） | Appwrite build command（明确禁止提交 node_modules）、Firebase Cloud Build、Supabase CLI 打包 |
| D DB 事件触发 | 触发器缺行业四件套（HTTP/cron/**DB 事件**/webhook）的第三件 | Appwrite 事件订阅（`databases.*.documents.*` 通配）、Hasura/Nhost DB 变更投递 |
| E 开发者体验 | 函数内裸 fetch 拼 Bearer；改一行代码要 zip→上传→构建→部署的分钟级循环 | wx-server-sdk / Appwrite context 对象；`supabase functions serve` / `wrangler dev` |

**复查后的明确不做**（调研死亡区 / 工程量收益失衡，详见 Alternatives）：isolate 路线（自托管雷区 + 弃 Node 生态）、Firecracker/VM 池、WASM 底座、K8s 生态任何组件、每实例多进程、实时日志流（一期，per-request tail + docker logs 够用）。

## Background & Motivation

### 差距从哪来（调研结论精简）

执行器 v2 的地基是对的（常驻容器 + 池 + dispatcher = Lambda/Cloud Run/Nuclio/faasd 共同的存活形态，v1 每请求容器的 CGI 形态已被本项目亲手裁掉）。但四项与业界默认形态的差距在 v2 落地时被「水平原语优先」的排期掩盖：

1. **串行**：`InstanceRecord.Busy` 布尔互斥（`functionsdispatcher/registry.go` claimIdleLua），并发 = 池大小 ≤ 8。I/O bound 函数（验签/回访平台/发奖——狗粮全部画像）的事件循环在 await 期空闲，这正是 Fluid Compute 论证的并发收益区。
2. **协议**：`main(TW_DATA)` 是自定义 JSON 协议——调研判据下处于死亡区边缘（无标准工具可调试、无生态可复用；复查后从「死亡区」修正为「生态/表达力缺口」：server/client invoke 本就是平台 RPC，真实伤害是响应表达力与 npm web 生态）。
3. **构建**：平台不装依赖 = 用户 zip 塞 node_modules（Appwrite 明确禁止的形态）或零依赖裸写。
4. **触发面**：outbox 事件脊柱已就绪（seq/notify/Stream 投递），但函数订阅事件的后半程缺失。
5. **DX**（复查补充，日常体感的最大来源）：函数内调平台 = 裸 fetch + 手拼 Bearer + 拼 URL（`TW_EXECUTION_TOKEN` 在两个 SDK 中零引用）；本地开发无 dev loop，CLI 16 个方法全是管理面 CRUD。

### 为什么是这五项、这个顺序

复查修正过两处夸大（多路复用从「立即升格」修正为「设计就绪即做」；接口从「死亡区」降为「生态缺口」），并按**狗粮阶段负载 × 设计就绪度 × 单位工作量收益**排序：A 设计已完备（本稿 §1）先行；E 便宜（SDK 纯包装、dev/deploy 纯 CLI）且直接命中「感觉局限大」的体感；B 依赖 A 的 per-request 基建；C/D 独立可并行。所有支柱都不推翻 v2 底座，concurrency=1 + main 导出 + 用户自带依赖 + 无事件订阅 + 无 SDK 的存量函数**零改动继续可跑**。

## Goals & Non-Goals

**Goals**

- 单实例并发执行同函数多请求（per-function `concurrency`，默认 1、上限 16、显式 opt-in）；
- 函数可选 Web 标准 `fetch(request, env)` 入口（双轨共存，导出探测），HTTP 触发器封套还原为真 Request、sync 模式透传完整 HTTP 响应；
- 平台代装依赖（`npm ci --omit=dev --ignore-scripts`，lockfile 强制，分层缓存）；
- DB 文档事件触发函数（独立消费组、订阅匹配、data 投影 + 按需回读）；
- `@torchwood/functions-sdk` + `torchwood functions dev/deploy` 本地闭环；
- 全程灰度：A 的 concurrency=1 等价性回归是其余支柱的地基验证。

**Non-Goals**

- CPU 并行（单线程事件循环不变；CPU bound 加实例/调 spec）；
- python 运行时（随其 v2 支持落地，直接实现本稿语义）；
- Response 流式回传与 invoke 路径 headers 回传（一期 runner 缓冲完整 body、不回传 headers——OQ7 已收口，按需再评估；触发器 sync 路径的完整 HTTP 透传不受影响）；
- ~~池策略管理 API 面~~（已拍板随 v3 立项：UpdateFunction optional ×5 + Console 池策略卡片，见 Rollout）；
- 实时日志流（dispatcher→server→Console 的流式管道——已拍板后置到**对外开放注册（多租户）前**，OQ6 收口；一期 per-request tail + docker logs 旁路）；
- 事件触发的平台级递归硬防护（一期文档 + Console 警告 + 指标告警，K13）；
- SLA 变化、编排/工作流、多机分发（均维持既有裁决）。

## Proposed Design

### 1. 实例内多路复用（runner 模板 v3）

> 吸收自 `functions-runner-concurrency.md`（已删除，此处为单一事实源）。

#### 1.1 并发模型与语义

- **并发单位**：单 Node 事件循环内多请求交错；模块只加载一次（启动即 require，v2 既有不变量），`main` 被并发调用。
- **可重入契约（红线，文档明示）**：函数作者必须保证 `main` 可重入——模块级可变全局状态在并发下有竞态，与 Lambda / Cloud Run 同款契约。平台责任 = 默认 concurrency=1（不 opt-in 即无暴露）+ 文档明示 + max_requests/崩溃重建兜底（既有）。
- **背压与扩容顺序**（全部既有机制，仅认领条件变化）：①`ClaimIdle` 找 `inflight < concurrency` 且非 draining 且部署匹配的实例 → inflight+1 + 续租；②未命中 → `trySpawn`（spawn 收敛锁不变）；③池满 → 有界排队（QueueDepth=32 / QueueHeadTimeout=10s 不变）→ 超限 ResourceExhausted。concurrency=1 时与现状逐步等价。
- **生效时机**：concurrency 固化进 `InstanceRecord`（spawn 时落账，与 MinInstances 同路）——实例终生按 spawn 时策略服务；函数调大后存量实例按旧值服务至 idle 回收/部署更替（与 min/max_instances 传播语义一致）。

#### 1.2 runner 模板 v3（`internal/infra/functions/runner/runner.js`）

`RunnerTemplateVersion` 2 → 3（`internal/domain/functions/repo.go:48` 单一事实源）。四项变更：

**执行上下文 request-scoped（安全修复核心）**：

```js
// v3 调用约定：main(data, ctx)；ctx 并发安全，process.env 仅同步段安全
// ctx = {
//   executionToken,  // 本次执行的 TW_EXECUTION_TOKEN（async 函数唯一安全来源）
//   apiBaseUrl,      // TW_API_BASE_URL 投影
//   executionId,     // 平台执行 ID（日志关联）
// }
```

- 以 `node:async_hooks` 的 `AsyncLocalStorage` 圈住每次调用：`als.run({ token, apiBaseUrl, executionId, logs }, () => userMain(data, ctx))`；
- `process.env.TW_EXECUTION_TOKEN` **仍设置**（同步 main 与模块顶层读取兼容）：调用前同步写入，同步函数执行期间事件循环不交错故安全；**含 await 的函数恢复执行后 env 可能已被后续请求覆盖——文档与 Console 提示必须读 `ctx`**；
- 分发 header 增补 `x-tw-execution-id`（`ExecuteRequest` 增补 ExecutionID 字段透传，见 1.5）。

**console 捕获按请求分桶**：`patchConsole` 优先写 `als.getStore()?.logs`（per-request 环缓冲），store 为空（模块加载期）落实例级兜底缓冲。响应的 `stdout`/`stderr` 语义从「实例级混流尾部」变为「**本请求** console 输出尾部」——排障改善，行为变化文档明示（OQ3 评估是否需混流旁路）。

**per-request 超时与悬空声明**：runner 按函数超时起 per-request 定时器：到点未决议 → 写 500（连接已断则忽略）并放弃等待该 promise。**诚实声明（与 Lambda 同款）**：超时后用户 main 可能仍在事件循环里跑至实例回收——inflight 按「请求生命周期」释放，不追踪用户代码生命周期。

**信号与自回收不变**：`TW_MAX_REQUESTS`、SIGTERM drain、`/_tw/health`（已含 inflight 上报）原样。

#### 1.3 注册表 inflight 化与释放原子化（最难的一块）

**记录结构**：

```go
type InstanceRecord struct {
    ...
    Inflight    int   `json:"inflight"`    // 取代 Busy bool（busy ≡ inflight > 0）
    Concurrency int   `json:"concurrency"` // spawn 时固化（1.1）
    Timeouts    int   `json:"timeouts"`    // 累计超时（熔断判定，1.4；杀实例即清零）
    SpawnedAtMS int64 `json:"spawned_at_ms"`  // 时间字段统一数值毫秒（见下）
    IdleSinceMS int64 `json:"idle_since_ms"`
    LeaseUntilMS int64 `json:"lease_until_ms"` // 不变（已是数值毫秒）
}
```

**认领脚本**（改造 `claimIdleLua`，原子性不变）：

```lua
local vals = redis.call('HVALS', KEYS[1])
for i = 1, #vals do
  local ok, rec = pcall(cjson.decode, vals[i])
  if ok and type(rec) == 'table' then
    local inf = rec.inflight
    if inf == nil then inf = (rec.busy == true) and 1 or 0 end  -- 旧记录兼容
    local c = rec.concurrency or 1
    if rec.draining == false and rec.deployment_id == ARGV[1] and inf < c then
      rec.inflight = inf + 1
      rec.lease_until_ms = tonumber(ARGV[2])
      redis.call('HSET', KEYS[1], rec.instance_id, cjson.encode(rec))
      return cjson.encode(rec)
    end
  end
end
return nil
```

**释放脚本（新增，修掉最大的竞态洞）**：现状释放是 Go 侧 `HGet→mutate→HSet` 非原子读改写（`registry.go Update`），串行期靠 busy 互斥免除竞态；多路复用后同实例 claim/release 并发交错，丢更新 = 计数漂移（永久「满载」或超卖）。释放必须 Lua 原子：

```lua
-- KEYS[1]=池键 ARGV: instance_id, now_ms, lease_until_ms
local raw = redis.call('HGET', KEYS[1], ARGV[1])
if not raw then return nil end
local ok, rec = pcall(cjson.decode, raw)
if not ok then return nil end
rec.inflight = math.max(0, (rec.inflight or (rec.busy and 1 or 0)) - 1)
rec.requests = (rec.requests or 0) + 1
rec.lease_until_ms = tonumber(ARGV[3])
if rec.inflight == 0 then
  rec.idle_since_ms = tonumber(ARGV[2])  -- 最后一个在途请求完成才开始计 idle
  if (rec.max_requests or 0) > 0 and rec.requests >= rec.max_requests then
    rec.draining = true                  -- 对抗审查修正：用记录固化值，与 concurrency 固化同理
  end
end
redis.call('HSET', KEYS[1], rec.instance_id, cjson.encode(rec))
return cjson.encode(rec)
```

（`now_ms` 由 Go 传入，Lua 不取时钟；max_requests 判断从 `pool.go executeOn` 的 Go Update 移入，且改用记录固化的 `max_requests` 字段——不用请求携带值，避免函数更新后策略与实例不一致的判定歧义。）**超时路径同样走释放脚本**（幂等，并累加 `timeouts` 计数，见 1.4 熔断）。

**时间字段毫秒化（排雷）**：`spawned_at`/`idle_since` 从 RFC3339 改数值毫秒，兑现既有注释「cjson 往返不得改写时间字段形态」不变量的彻底版。**兼容陷阱（必须处理）**：注册表只能从记录侧对账（容器在、记录亡的容器无人回收），**清 Redis 键升级会泄漏容器**——decode 必须双读旧字段名（自定义 `UnmarshalJSON` 兼容 RFC3339 → 毫秒），旧记录自然老化，无升级 runbook。

#### 1.4 dispatcher：超时语义、判活与 drain（`functionsdispatcher/pool.go`）

**超时/取消不再杀实例**：现状 `executeOn` 对 invokeErr 一律 `killInstance`（`pool.go:541-549`）——串行下合理，并发下一个慢请求误杀同实例健康在途请求。改为：

| invokeErr 形态 | 处置 |
|---|---|
| ctx 超时 / 调用方取消（`ctx.Err() != nil` 或 `isTimeoutErr`） | **不杀实例**：走释放脚本（inflight-1 幂等），请求以 DeadlineExceeded/Canceled 收场；runner per-request timer 兜底 abandon |
| 传输层错误（连接拒绝/reset，非超时） | **仍杀实例**（容器崩溃判定，Cloud Run 同款；handler 崩溃 → 进程退 → 连接断，归此类） |

**超时熔断（对抗审查修正：僵尸负载通道）**：「超时不杀」拆掉了串行模型顺带消灭僵尸负载的保护——有 bug 的函数每次超时留下永不决议的 async 操作/setInterval，慢性塞满事件循环，而 health 探针仍响应、实例永不回收（8 实例上限下毒化实例占 1/8 容量）。修复：实例记录加 `timeouts` 计数（超时释放路径累加，正常完成不重置——计数随实例生命周期，杀实例即清零），**累计达熔断阈值（默认 5，`functions.dispatcher.timeout_budget`）→ 杀实例重建** + 指标。正常函数偶发超时不会连续累积 5 次；误熔断一次也只是 drain 语义重建，无害。此机制**进切片一**（否则切片一即引入毒化窗口）。

**判活**：reaper busy 分支改 `inflight > 0`；lease 在认领与每次释放时续；`stuckBusyGrace`（10min）不变——超时请求已即时释放，stuck 仅剩 dispatcher 崩溃残留场景。**drain**：`DrainForDeployment` 的「busy → 宽限强杀」改「inflight > 0 → 宽限强杀」，宽限 ≤ 函数超时不变。

#### 1.5 策略链路与校验

- **迁移 000017**（`internal/infra/projectschema/migrations/000017_function_runner_concurrency.{up,down}.sql`）：

  ```sql
  ALTER TABLE {{schema}}.functions
      ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 1
          CONSTRAINT functions_concurrency_check CHECK (concurrency BETWEEN 1 AND 16);
  ```

- **透传链**：bun model `Function.Concurrency` → domain `Function` → `buildExecution`（`internal/app/functions/executions.go:618` 池策略组旁）→ domain `Execution.Concurrency` + `ExecutionID` → `dispatcher_client.go` → `ExecuteRequest.Pool.Concurrency` + `ExecutionID`（`functionsdispatcher/types.go` PoolPolicy/ExecuteRequest 加字段，后者经分发 header `x-tw-execution-id` 透传）。
- **降级保护（fail-safe，不 fail-closed）**：`fn.Concurrency > 1` 而当前 deployment `template_version < 3` 时**静默按 1 执行** + 指标/日志观测——存量函数不因新列拒绝执行，重部署后自然生效。

#### 1.6 兼容与迁移

| 对象 | 影响 | 处置 |
|---|---|---|
| 存量 deployment（template_version=2） | 不支持 ctx/分桶/per-request 超时 | 降级 concurrency=1；重新 CreateDeployment 获 v3 模板 |
| 存量注册表记录（busy/RFC3339 字段） | 新 Lua/decode 需兼容 | decode 双读 + Lua `or` 兜底，自然老化（1.3） |
| v1 执行器（`functions.executor=docker`） | 不受影响 | 无池概念，Concurrency 忽略 |
| 函数代码（`main(data)` 单参数） | 不破坏 | 第二参数新增；同步函数零改动 |
| 指标/告警 | 池水位口径不变 | 新增见 Observability |

### 2. 接口现代化：Web 标准 fetch handler（runner 模板 v4）

#### 2.1 入口探测与双轨共存

runner v4 加载用户模块时探测导出（探测优先级固定，二者皆无 = 加载错误 `index.js must export main or fetch`）：

```js
// 风格一（v1 起既有，零改动继续可跑）：main(data, ctx) → JSON 进出
exports.main = async (data, ctx) => ({ ok: true });

// 风格二（v4 新增，Web 标准）：fetch(request, env) → Response
export default {
  async fetch(request, env) {
    const { userId } = await request.json();
    return Response.json({ ok: true, userId });
  },
};
```

- `request` 是 Node 18+ 原生全局 `Request`（undici）——零依赖、与 Workers/Deno/Fluid 同构，hono/itty-router/zod 等 npm web 库直接可用；
- `env = { EXECUTION_TOKEN, API_BASE_URL, EXECUTION_ID }`——**env 是每次调用的参数而非 process.env，token 串号问题在该风格下结构性不存在**（1.2 的 AsyncLocalStorage 仅为 main 风格服务）。**对抗审查修正（表述精确化）**：functionVariables 仍是 spawn 时固化进容器 `process.env`（既有语义，函数级非请求级，无串号问题、第三方库读 env 照常工作）——fetch 的 env 参数只带请求级三件；`env` 参数与 data 同计既有 32KB 预算（token/baseUrl 约 200B 量级，与 main 风格同源）；
- 模板 v3（并发）与 v4（接口）**分立升级**（D15）：独立价值独立切片，dogfood 重部署成本可忽略；v4 复用 v3 的全部 per-request 基建（分桶日志、per-request 超时、inflight 计数）。

#### 2.2 invoke 语义映射

| 触发来源 | Request 构造 | Response 处理 |
|---|---|---|
| server/client invoke | `POST http://function/{function_id}`，body = TW_DATA JSON，header 带 `x-tw-execution-id` | body 文本 → `ExecuteResponse.Response`（兼容现有 JSON 字符串透传契约）；HTTP status → `StatusCode` 字段（该字段现为「容器退出码语义位（v1 兼容；v2 恒 0）」，fetch 风格下承载函数 HTTP status）；headers 一期不回传（OQ7） |
| HTTP 触发器 | **封套还原**（见 2.3） | sync 模式透传**完整 HTTP 响应**（status/headers/body——自定义状态码、二进制、重定向从此可达）；async_ack 不变 |
| cron | `POST`，body = 既有 cron 载荷（`{"type":"cron",...}`） | 同 invoke |

#### 2.3 HTTP 触发器封套还原（本支柱最大红利）

现状 HTTP 触发器把请求打成封套 JSON（`{method, path, raw_query, headers, body, body_base64}`）塞进 TW_DATA，函数手动解析。v4 下 runner 把封套**还原为真 Request**：

```js
// runner 端（分发请求体带触发器封套标记时）：
//   url    = http://trigger{path}?{raw_query}   —— query string 回到该在的位置
//   body   = body_base64 优先（无损），退 body 字符串
//   headers= 白名单头原样（大小写保留语义交给 Headers）
const url = new URL(`http://trigger${envelope.path}?${envelope.raw_query}`);
const req = new Request(url, { method: envelope.method, headers: envelope.headers, body: ... });
```

微信 SSV 验签从「解析封套取 raw_query/body」变为标准 `new URL(request.url).searchParams` + `await request.text()`——与任何 HTTP 框架代码同构。**main 风格函数不受影响**（封套照旧进 TW_DATA，双轨并存到 main 退役）。

分发协议配套（对抗审查修正：封套经独立 header 传递）：封套**元数据**（method/path/raw_query/headers 白名单——不含 body）经分发 header `x-tw-trigger-envelope`（base64 JSON）传给 runner，**body 仍走 HTTP body 通道**；封套 header 上限 12KB（修复重审：Node http header 默认 16KB，base64 膨胀后须显式限界，超限 400——封套不含 body，正常体积 <4KB）。曾考虑 body 外层包装 `{"_tw_trigger": {...}, ...data}`，否决——用户 data 是任意 JSON object，合法含 `_tw_trigger` 键会被误判，键空间污染破坏「TW_DATA 即用户数据」契约。runner 探测到该 header 即走还原路径（fetch 风格）或封套注入（main 风格，与现状等价）。

#### 2.4 限制

- Response body 一期完整缓冲（`await response.arrayBuffer()`），流式后置（OQ7）；body 上限沿用 64KB 输出口径截断；
- `waitUntil`（请求后后台任务）不做——超时语义与 inflight 生命周期基于请求完成，后台任务破坏计数契约（Workers/Fluid 有此能力，我们显式不做，函数内自行在超时内完成）。

### 3. 平台代装依赖

#### 3.1 构建规格与分层 Dockerfile

`dockerfileFor`（node）改造为经典分层（lockfile 不变即命中 Docker 层缓存，免费获得增量构建）：

```dockerfile
FROM node:18-alpine
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev --ignore-scripts     # 有 lockfile 时
COPY . .
USER node
```

- **探测**：zip 根含 `package.json` 且 `dependencies` 非空 → 代装；否则维持现状（无依赖函数零变化）；
- **lockfile 强制**：有 dependencies 但无 `package-lock.json` → 构建失败并提示先提交 lockfile（确定性构建原则，与「构建是平台确定性操作」不变量对齐——无锁安装不可复现）；
- **node_modules 禁止入包**：构建期解压校验加一条——zip 含 `node_modules/` 直接拒绝（对齐 Appwrite，防跨平台二进制污染）；

#### 3.2 `--ignore-scripts` 默认（不变量张力的裁决）

「构建期不执行用户代码」不变量 vs npm 生命周期脚本（postinstall 可执行任意代码，含出网）。**裁决（D11）：默认 `--ignore-scripts`**——fail-closed 惯例与 concurrency 默认 1 同路；代价是依赖原生编译（node-gyp）或 postinstall 下载二进制（esbuild/swc）的包不可用，文档明示 + 构建错误信息直指该原因。`allow_scripts` opt-in（per-function 列）后置（OQ4），届时配套构建容器出网白名单（OQ5）。一期构建网络不额外限制（registry 拉包必需），构建容器复用既有 hardening。**残余风险声明（对抗审查）**：`--ignore-scripts` 不消除供应链面本身——lockfile 是用户可控输入，npm 解析器对恶意构造输入的漏洞（原型污染类 CVE 历史）仍可能在构建容器内执行代码；缓解 = lockfile integrity hash 固定 + 构建容器既有 hardening（非 root、无 sock、资源限额）+ 镜像内产物同样跑在执行容器 hardening 下，逃逸面无显著扩大。接受该残余风险并文档化。

python 随其 v2 支持：`requirements.txt` + `pip install --no-cache-dir`（pip 无等价 ignore-scripts，届时单独评估）。

### 4. 数据库事件触发器

#### 4.1 实体与订阅模型

`function_triggers.type` CHECK 扩展 `'event'`（迁移并入 000018，见 Rollout），config：

```json
{
  "events": [
    "databases.{database_id}.collections.{collection_id}.documents.create",
    "databases.{database_id}.collections.{collection_id}.documents.update"
  ]
}
```

- 事件字符串格式对齐 Appwrite（业界同构、迁移友好）；一期支持精确三事件（create/update/delete）+ collection 级通配（`collections.*.documents.*`）；database 级通配后置；
- 管理 RPC 复用既有触发器四方法（type 值域扩展 + config 校验：事件串格式、集合存在性 best-effort）。

#### 4.2 投递链路（独立消费组，零侵入 outbox 主链）

```
outbox 表（重放真源）──(既有)──▶ Redis Stream torchwood:events ──┬─▶ WS 订阅者（既有消费组）
                                                                 └─▶ functions-triggers 消费组（新增，worker 进程）
                                                                       XREADGROUP → 反序列化 Envelope
                                                                       → 匹配订阅（Event + DatabaseID + CollectionID + project）
                                                                       → 逐 trigger 入异步执行队列（既有）
                                                                       trigger_source = event:{trigger_id}
              ▲
              └── 停机恢复补投（对抗审查修正：一期必做，非后置）：
                  worker 启动时检测消费组 last-delivered-id 落后于 Stream 首条
                  （停机超过 trim 窗口，中间条目已被 XTRIM 裁掉）
                  → 从 outbox 表按 seq ∈ (last_seq, stream_first_seq] 分批补投（复用 :changes 语义；
   上界收在 Stream 现存首条——修复重审：无上界会与恢复后的正常消费重叠投递）
```

- worker 新增消费者 goroutine（组名 `functions-triggers`，XACK 在入队成功后）；outbox 主投递路径零改动——重放窗口由 outbox 表承担（Stream 只是传输）。
- **停机补投是一期必做（对抗审查修正，曾列为 OQ8 后置——否决）**：`StreamTrimmer` 每 10min `XTRIM MAXLEN 100k` **不理会消费组进度**，worker 停机超过 trim 窗口后恢复，消费组会静默跳到现存最老条目——**中间事件丢失且无感知**，对计费/对账级语义不可接受。恢复路径：worker 启动时取消费组 last-delivered-id 对应 seq 与 Stream 现存首条 seq 比较，落后即从 `outbox` 表按 `seq > last_seq` 分批（复用既有 `:changes?since_seq=` 的领取语义）重放进匹配投递；补投与正常路径重叠投递由 at-least-once + 函数幂等吸收。停机一天的全量补投按批限量推进（共享异步通道既有信号量兜底，风暴语义与 cron catch_up_once 同款）。
- **data 投影（32KB 预算的关键设计）**：事件信封可达 1MiB，执行 data 上限 32KB——data 只带投影 `{type:"event", event, event_id, seq, database_id, collection_id, document_id, version, envelope_truncated, data?, data_truncated?}`：`data`（文档投影）尽力塞入剩余预算，超限截断标 `data_truncated`（对抗审查修正：与信封级截断字段 `envelope_truncated`（= `Envelope.Truncated`，1MiB 信封预算截断）**分名**，两级截断语义不混）；**函数按 `document_id` 用 `databases:read` scope 回读全量**（execution principal 链路既有）——「ID + 摘要进 data，全量靠回读」与 HTTP 触发器封套「小包透传 + 按需取」同一哲学；
- **幂等**：at-least-once（outbox 既有语义 + 补投重叠），函数幂等键推荐来源 = `event_id`/`seq`；
- **投递保证的诚实边界**：XACK 后入队前崩溃 = 该事件对本订阅者延迟至停机补投路径（不再是永久丢失）；outbox 表 24h 清理窗口之外的极端停机（>24h）才真正丢失——文档明示该窗口（与 WS 订阅 `EVENTS.RESUME_EXPIRED` 同一口径）。

#### 4.3 递归与风暴语义

- `declared_scopes` deny `functions:*`（既有）防「函数造函数」；**自环**（函数订阅自己写入的 collection → 写 → 新事件 → 再触发）一期**不做平台硬防护**（D13）：文档 + Console 订阅编辑处警告「请勿订阅本函数写入的集合」+ `invoke_total{source=event}` 速率告警兜底——Appwrite 官方同款处理（文档警告）；
- 风暴兜底：异步路径无队列深度上限、run 信号量 + dispatcher 有界排队兜底（与 cron misfire 同款既有语义）；per-trigger 并发上限后置（OQ9）。

### 5. 开发者体验

#### 5.1 `@torchwood/functions-sdk`（函数内 SDK，服务端零改动）

- 复用 `sdk/typescript` 既有 transport 与服务类（`sdk/typescript/src/server/*`、`http.ts`）：`HttpTransport` 新增 `auth: "execution"` 模式（`Authorization: Bearer <token>`——token 来源优先级：显式传入 > `TW_EXECUTION_TOKEN` env > main 风格的同步段读取）；
- 入口：`Torchwood.fromExecution()`（读 env，main 同步段/dev 用）/ `new Torchwood({ executionToken, apiBaseUrl })`（fetch 风格从 env 参数构造）；方法面 = server 服务类全量（assets grant / documents / users …）；
- node 18+ 全局 fetch，零运行时依赖；发布 npm（monorepo 内同源构建）。

#### 5.2 `torchwood functions dev`（本地开发闭环）

- **复用生产 runner 本体**：CLI 把 `runner.js`（与镜像模板同源资产）写盘并在用户目录 `node runner.js` 起 `:18080`——本地与生产**同一 runner 二进制**，零分叉；启动时输出模板版本号并提示与目标 server 的 `RunnerTemplateVersion` 比对（CLI 旧版本 + server 新模板的漂移显式化，对抗审查补充）；
- 注入：`--data-file`（TW_DATA）、`--env-file`（variables，本地 `.env`）、`--token`（显式 execution token，指向本地 server 实测鉴权链）或省略（纯本地 mock）；
- 热重载：watch `index.js` 变更 → 进程重启（一期；模块缓存清除后置）；调用即 `curl -X POST localhost:18080/ -d @data.json`；
- 部署摩擦配套 `torchwood functions deploy`：zip（既有逻辑）→ CreateDeployment → 轮询构建状态 → 失败输出 error 尾部——纯 CLI 包装，无服务端改动。

#### 5.3 日志（一期收口）

per-request 分桶 tail（v3 自带，执行结束 Console 即见**本请求**输出）+ 容器全量日志经 `docker logs` 旁路（文档化）。实时流（执行中 tail -f：dispatcher→server→Console 的 SSE/WS 管道）后置到**对外开放注册（多租户）前**（OQ6 收口）。

## Security & 语义声明（跨支柱）

1. **凭证串号修复（A 的最重要安全项）**：async main 下 `process.env.TW_EXECUTION_TOKEN` 有串号窗口（A await 恢复后读到 B 的 token——A 以 B 的身份干活）。ctx（main）/ env 参数（fetch）是并发唯一安全通道；process.env 保留仅为同步兼容。
2. **隔离粒度不变粗**：仍实例级（同函数同租户），容器 hardening 原样；新增暴露面 = 同实例请求间共享事件循环（可重入契约，1.1）。
3. **构建期执行面（C）**：`--ignore-scripts` 默认下构建期不执行第三方脚本；npm 包本体下载仍是供应链面（lockfile 固定版本 + 分层缓存缓解），registry 白名单后置（OQ5）。
4. **事件触发的数据面（D）**：函数回访读取文档仍走 execution principal + RLS（scope 门 + `key:function:<id>` 角色授予，既有 B14 语义）——事件触发不豁免任何权限检查。
5. **stdout/stderr 语义变化**：per-request 化，不再含其他请求输出——审计口径更准。
6. **超时后的代码残留**：1.2 诚实声明，计数不追踪用户代码生命周期。

## Observability

- 新增：`torchwood_functions_instance_inflight{project,function}` Gauge（reaper 周期聚合）、`torchwood_functions_concurrency_downgraded_total`（降级保护）、`torchwood_functions_instance_timeout_fuse_total`（熔断触发，对抗审查补充）、`torchwood_functions_invoke_total` 的 source 枚举扩 `event`、事件订阅投递计数 `torchwood_functions_event_deliveries_total{project,function,result}`、停机补投计数 `torchwood_functions_event_backfill_total{project,result}`（对抗审查补充——补投发生即告警锚点：停机窗口可视）；
- 既有 `dispatch_queue_wait_seconds`/`dispatch_duration_seconds` 直接度量 A 的收益（排队时延下降）；C 的构建时长经既有 build 路径指标；
- SLA 口径不变。

## Rollout Plan / 阶段与切片

依赖关系：**A → C-interface**（v4 复用 v3 per-request 基建）；B-DX、D-事件、C-build 相互独立可并行。

| 阶段 | 内容 | 验收锚点 |
|---|---|---|
| **A 多路复用**（设计就绪，先行） | 切片一（行为不变\*）：registry inflight 化 + 释放 Lua 原子化 + 时间毫秒化 + runner v3 + 超时不杀实例 + **超时熔断** + 指标。切片二（放开）：迁移 000017 + 透传链 + 降级保护 + 可重入契约文档 | concurrency=1 下分发/池/认领语义与 v2 等价（既有测试全绿 + 狗粮 v3 重部署回归）；\*超时处置语义**有意变更**（超时请求后实例存活 + 熔断计数）——专项用例：单请求超时后同实例其他在途请求不受影响、累计 5 次超时触发熔断重建；concurrency=8 压测：queue_wait 显著下降、无 429 误杀、并发请求 ctx token 隔离断言 |
| **B DX 快赢**（与 A 并行） | functions-sdk（transport 加 execution 模式 + npm 发布）+ `functions dev`（runner 写盘本地起）+ `functions deploy` 包装 + **池策略管理面**（UpdateFunction proto3 optional ×5——min/max_instances、idle_ttl、max_requests、concurrency + Console 函数详情「池策略」卡片，OQ2 拍板立项） | 狗粮 SSV 函数改用 SDK（删裸 fetch 样板）；本地改一行 → curl 即测（秒级循环）；deploy 一条命令含构建状态回显；Console 可调 concurrency 并即时生效观测 |
| **C 接口 v4**（依赖 A） | runner v4 fetch 探测 + 封套还原 + ExecuteResponse.StatusCode 承载 HTTP status + 触发器 sync 完整透传 | 新函数 hono 路由零适配可跑；SSV 函数 fetch 风格重写（`request.url.searchParams` 验签）；HTTP 触发器返回 302/二进制验证 |
| **D 事件触发器**（独立并行） | 迁移 000018（type CHECK 扩展）+ worker 消费组 + 订阅匹配 + data 投影 + **停机恢复 outbox 补投** + Console 订阅编辑（含自环警告） | 狗粮：资产变动触发对账函数（event_id 幂等）；文档 >32KB 时 data_truncated + 回读全量验证；**停机超 trim 窗口后恢复，事件经补投零丢失（对账断言）** |
| **E 构建体验**（独立并行） | dockerfileFor 分层 + lockfile 强制 + node_modules 拒收 + `--ignore-scripts` | 带依赖函数部署：平台代装、二次部署命中层缓存（构建时长下降）；无 lockfile 构建失败提示明确 |

每阶段独立 PR 切片沿用派发稿红线自查；**C（接口）与 D（事件触发器）合入前各过一轮安全自查**（对齐 P2 前安全评审惯例：C 的公开端点响应透传面、D 的订阅投递滥用面）；全部落地后 08-functions.md 增补 §4.5（v3 并发与接口）并更新 §3（构建）与 §12（事件触发器）。

## Key Decisions（2026-09-10 owner 拍板通过）

| # | 决策 | 理由 |
|---|------|------|
| D1 | 并发模型 = 单实例单事件循环内多请求交错（非多进程/worker_threads） | 业界同构（Cloud Run/Fluid/Lambda）；零额外内存；模块级状态语义与 v2 连续 |
| D2 | concurrency 默认 1、显式 opt-in、CHECK 上限 16 | fail-closed 惯例；可重入契约由函数作者显式接受；8×16=128 并发对单机够用 |
| D3 | 超时/取消只失败该请求；传输错误仍杀实例；**per-instance 超时熔断（累计 5 次杀实例重建）** | 并发下杀实例误伤在途请求（Cloud Run 同款）；熔断堵住「超时不杀」打开的僵尸负载通道（对抗审查） |
| D4 | token 经 ctx（AsyncLocalStorage）/ env 参数传递，process.env 保留同步兼容 | 修串号窗口；渐进迁移不破坏存量同步函数 |
| D5 | 释放路径 Lua 原子化 + 记录时间字段数值毫秒化 | 多路复用后释放竞态必须消除；顺排 RFC3339 毒化地雷 |
| D6 | concurrency 固化进实例记录，调大后存量实例按旧值服务至回收 | 与 min/max_instances 传播语义一致；避免 claim 时多请求携带异值的判定歧义 |
| D7 | 旧模板降级 concurrency=1 而非拒绝执行 | 行为等价现状，存量不破坏；指标观测降级 |
| D8 | 模板 v3/v4 分立（并发与接口独立升级） | 独立价值独立切片；曾评估合并为一次模板升级，dogfood 重部署成本可忽略，裁决分立 |
| D9 | fetch 双轨共存（导出探测：`fetch` > `main`），不强制迁移 | 存量零改动；新函数渐进采用；两风格共享 per-request 基建 |
| D10 | HTTP 触发器封套还原为真 Request；sync 透传完整 HTTP 响应（status/headers/body） | Web 标准生态可达；自定义状态码/二进制/重定向从此可达；main 风格封套照旧 |
| D11 | 代装依赖默认 `npm ci --omit=dev --ignore-scripts`、lockfile 强制、node_modules 拒收 | 「构建期不执行用户代码」不变量优先；确定性构建；原生模块包不可用文档明示 |
| D12 | 事件投递走独立消费组；**停机恢复 outbox 补投一期必做**（XTRIM 不理会消费组进度，静默丢失不可接受）；data 只带投影（ID+摘要），全量靠 `databases:read` 回读 | outbox 主链零侵入；Stream 只是传输、outbox 是重放真源；32KB 预算与 1MiB 信封的矛盾显式化解；权限链不豁免 |
| D13 | 事件自环一期文档 + Console 警告 + 指标告警，不做平台硬防护 | Appwrite 同款；硬防护语义（深度切断）过强且伤合法链式场景 |
| D14 | 实时日志流后置到对外开放注册（多租户）前；一期 per-request tail + docker logs 旁路 | 流式管道工程量与 dogfood 收益不成比例；对外用户无 docker logs 旁路时才是刚需（2026-09-10 拍板） |
| D15 | DX 三件（SDK/dev/deploy）先行于接口与构建 | 便宜、独立、直接命中「局限大」体感；不依赖任何 runner 变更 |

## Open Questions（2026-09-10 owner 全部收口）

1. ~~concurrency 硬上限~~ **已拍板：16**（CHECK 约束；8×16=128 并发对单机拓扑足够，更高应先质疑单机拓扑）。
2. ~~池策略 API 面~~ **已拍板：随 v3 立项小切片**（UpdateFunction optional ×5 + Console 池策略卡片，进 B 阶段——concurrency 无入口 = 功能不存在）。
3. ~~stdout 混流旁路~~ **已收口：不做**（per-request tail + docker logs 全量覆盖；旁路只让响应体积翻倍）。
4. ~~`allow_scripts` opt-in 形态~~ **已拍板：一期不做**（ignore-scripts 恒定）；opt-in 与 registry 白名单（OQ5）绑定随 egress 代理原语后置。
5. ~~构建出网白名单~~ **已拍板：一期不限制**（registry 拉包必需；残余供应链窄面经 lockfile integrity + hardening 兜底，文档已声明）。
6. ~~实时日志流里程碑~~ **已拍板：后置到对外开放注册（多租户）前**（dogfood 阶段 per-request tail + docker logs 够用）。
7. ~~fetch Response 细节~~ **已拍板：一期不回传 headers、body 全缓冲**（invoke 路径 headers 是伪需求；触发器 sync 完整 HTTP 透传不受影响）；真实需求出现再评估。
8. ~~事件订阅 `since_seq` 补偿~~ **已收口（对抗审查升格）**：停机恢复的 outbox 补投为一期必做（D12）。
9. ~~事件 per-trigger 并发上限~~ **已拍板：一期不做，指标先行**（event_deliveries_total 速率告警；三层既有兜底——异步通道信号量 + dispatcher 有界排队 + 补投分批；狗粮跑出风暴画像后再决定是否精确限流）。

## Alternatives Considered

- **模板 v3+v4 合并为一次升级**：省一次存量重部署，但耦合两个独立价值、互相 block（见 D8 理由）——被否决。
- **Lambda 式 event-handler 签名**（context 对象 + callback）：AWS 自己都要靠 Lambda Web Adapter 兼容 HTTP 容器——Web 标准是业界收敛方向，被否决。
- **Convex 式「函数即数据库事务」**：query/mutation 在事务引擎内执行，毫秒级无冷启动——托管专属激进路线，与容器隔离底座根本冲突；其「声明式毫秒级 vs 容器 ≤100ms」的差距由 P3 声明式原语独立复盘（既有判据不变）。
- **实时日志流一期做**：需要 dispatcher→server→Console 三段流式管道（含背压/断线语义），dogfood 阶段收益不抵——后置（D14）。
- **进程内轻量执行**（PocketBase goja 式 hooks）：零冷启动但放弃多语言与容器隔离，且与 P2 不可信触发面（egress 分类、per-project 网络）完全不兼容——被否决（调研死亡区旁的进程内路线仅适用于「用户代码=可信」前提）。
- **isolate / Firecracker / WASM / K8s 生态**：调研死亡区清单，均被否决（见 Overview「明确不做」）。

## References

- 前置设计：`docs/design/functions-execution-identity-and-triggers.md`（v2 执行器与 P0–P2 全景；本稿 §1 为其 §6/Q12 的继任收口）
- 现状文档：`docs/developer/08-functions.md` §4.3（v2 池与分发）、§12（触发器）、§13（客户端调用）
- 代码锚点：`internal/infra/functions/runner/runner.js`（v2 模板与串行假设注释）、`functionsdispatcher/pool.go:528` `executeOn`、`functionsdispatcher/registry.go:94` `claimIdleLua`、`functionsdispatcher/types.go:80` `PoolPolicy`、`internal/app/functions/executions.go:618` `buildExecution`、`internal/domain/functions/repo.go:48` `RunnerTemplateVersion`、`internal/domain/events/envelope.go:36` `Envelope`（事件信封）、`sdk/typescript/src/http.ts`（transport auth 模式）、`cmd/torchwood/cmd/functions.go`（CLI 管理面现状）
- 迁移先例：`internal/infra/projectschema/migrations/000014_function_executor_v2.up.sql`
- 竞品证据（2026-09-10 调研）：Cloud Run 实例并发（docs.cloud.google.com/run/docs/about-concurrency）、Vercel Fluid（vercel.com/docs/fluid-compute）、Appwrite Functions 构建（appwrite.io/docs/products/functions/develop）、Appwrite 事件订阅、Supabase 自托管现状（github.com/supabase/supabase/issues/38505）、faasd（github.com/openfaas/faasd）
