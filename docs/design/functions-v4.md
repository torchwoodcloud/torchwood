# Functions v4：执行面外包——从执行器到路由与身份层

> 状态：**已批准（2026-10-15 owner 拍板：D1–D9 全部收口；同日 D10 协议双轴增补）**
> 拍板记录（2026-10-15 owner）：D1 调用协议 = **JSON-RPC 2.0 over 签名 HTTP**（自有封套否决——同构表达力下零发明、`method`/`id` 字段白送路由与关联、与 MCP 同底座）；D2 fleetly 原生 functions face = **实现后置 + 提案先行**（§9 提案草稿随本稿呈审，审阅后排开发）；D4 迁移 = **一步到位**（单一发布窗口，无并存期，回退 = 整体回滚）；D5 Webhooks = **15 次/24h 指数退避 + 行内 DLQ + Redeliver API，计量一期仅进 metrics**；D8' target = **单一 endpoint kind**（proto 留 fleetly 扩展位）；D9 Variables 面 = **随 v4 退役**；D3 记账两写由代码核实定案（幂等/限频/审计三链）
> 拍板记录（2026-10-15 owner，D10 协议双轴增补）：判定准则 =「有没有调用方在等应用层结果」而非触发器类型——**RPC 轴（JSON-RPC 2.0，有 id）：`invoke`（含 async）+ `http.request`（sync 与 async_ack 统一，同触发器不因响应模式换协议）；事件轴（CloudEvents 1.0 structured mode，通知）：`cron.tick`（平台自产时间事件，misfire catch_up_once 即重投语义）、事件触发（document/system，type=事件目录名）、webhook 投递**——CloudEvents 选 structured mode（属性全在 body 内，整体落在签名保护圈；binary mode 的 ce-* header 不受签名保护）；函数事件触发与 webhook 投递统一为同一投递管道（重试梯度/DLQ 语义免费覆盖函数侧，消灭「异步执行 attempt 重试」独有语义）；语义损失入册：事件轴投递行不记录函数返回值（响应 body 尾部截断留档作审计替代）
> 定位：Functions 子系统第四代。谱系：v1「每请求一容器」（CGI 形态，已裁决设计缺陷）→ v2「常驻 runner + dispatcher 池」→ 底座两轮迭代（多机细胞模型 → fleetly swarm，IMPL-T2-3）→ **v4 = 执行面整体外包：打包/构建/部署/池/冷启动/运行时/变量全部移出 torchwood；torchwood 收敛为纯「路由与身份层」，Webhooks 升一等公民**
> 裁决链（2026-10-15 owner 三轮收敛）：① 复杂度审计定性——「客户端可触发的服务端逻辑」是 BaaS 品类刚需（论证不推翻），但用容器编排机器承载它是 PaaS 错位；② 方向拍板——移除 dispatcher/packer，Webhooks 升一等公民，函数执行目标 = fleetly 托管或 BYO 常驻服务；③ 部署归属拍板——**torchwood 不再负责打包部署**，CLI 内嵌 esbuild 方案废弃，部署链 = fleetly CLI（或用户自有工具链）
> 依据：全链路复杂度审计（§1）＋ fleetly proto 分工自证（其 tasks.proto 明言「池语义（租约/保温/熔断）留给调用方」——dispatcher 全部池机器正是为在一次性 Task 上自建常驻池而付；当时 vendored 副本 `third_party/fleetly` 已随 dispatcher 退役删除）＋ 8 项代码核实（幂等三链/状态机/eventmatch/FDK 验签缺口/SSRF 资产/配置面/透传面/测试面，内联证据见 §4–§8、§10）
> 文档关系：继任 `functions-v3.md`——五支柱中 A（实例内并发复用）与 D（DB 事件触发器）的**调度/订阅语义**保留、执行载体转移；B（Web 标准接口）/C（平台代装依赖）随执行面外包**消解**（协议面由 §5 调用协议承接）；本稿批准后 `docs/developer/08-functions.md` 重写、roadmap §2.6 收口修订
> 产品哲学不变（K6/K7 水平原语）：「货币/库存只能云函数写」的品类刚需论证（`economy-client-write-competitive-analysis.md` §三）继续成立——客户端调用面、执行身份、触发器、配额一个不少；变化的只是**执行位置**：从平台内 dispatcher 池变为任意常驻服务（fleetly 托管为参考路径）
> 兼容性：内部验证阶段，**无存量外部用户、无向后兼容负担**——proto 字段直接 reserved、表直接改名、进程直接删除（§12 迁移按此口径）

---

## §0 Overview

```
                    ┌──────────────────────────────────────────────┐
                    │            torchwood（v4 终态）               │
                    │                                              │
  客户端 SDK ──────▶│  调用面：InvokeFunction（幂等/限频/并发闸）    │
  HTTP 触发器 ─────▶│  触发器调度：http / cron / event              │
                    │  身份：twx_ 执行身份铸造 + scopes + 签名注入   │
                    │  记账：function_invocations 两写 + 计量        │
                    │  事件投递：Webhooks（一等公民）+ 函数事件触发   │
                    └───────────┬──────────────────────┬───────────┘
                                │ 签名 HTTP（§4 协议）  │ 签名 HTTP 投递
                                ▼                      ▼
                    ┌───────────────────┐   ┌───────────────────┐
                    │  函数 = 常驻服务    │   │  Webhook 接收方    │
                    │  （跑 FDK，任意     │   │  （任意 HTTPS 端点）│
                    │   substrate）      │   └───────────────────┘
                    └─────────┬─────────┘
                              │ 部署/构建/池/扩缩
                              ▼
                    ┌───────────────────┐
                    │ fleetly CLI（参考  │
                    │ 部署链）/ 用户自有  │
                    │ 工具链（k8s/裸机）  │
                    └───────────────────┘
```

torchwood 保留的每一块都是 BaaS 本分（路由、身份、记账、投递）；外包的每一块都是 PaaS 机器（打包、构建、镜像、池、冷启动、运行时）。**torchwood 不再编排任何工作负载，不再持有任何用户代码字节。**

| 能力 | v3 现状 | v4 终态 |
|---|---|---|
| 打包/构建/部署 | zip/git/image 三源，平台构建镜像，dispatcher 承接 | **零**——fleetly CLI 或用户自有工具链 |
| 执行底座 | dispatcher 常驻池（Lua 认领/租约/reaper/熔断） | **零**——目标恒为常驻服务，无唤醒无保温 |
| 运行时/变量/池策略 | torchwood proto 一等字段 | **零**——归 substrate（fleetly env/副本数、k8s HPA、用户自管） |
| 触发器调度（http/cron/event） | torchwood | **保留**（语义不变，载体改 endpoint 调用） |
| 客户端调用面 + twx_ 执行身份 | torchwood | **保留**（注入通道改签名请求头） |
| invocation 记账 + 计量 | torchwood | **保留**（两写骨架定案，§8） |
| 事件 → 外部投递 | 无（roadmap P3） | **新增一等公民**：Webhooks（§7） |

## §1 复杂度审计（为什么必须动）

**量化现状**（prod/test 分计，不含 console 前端与 fleetly 外部仓）：

| 区域 | 代码量 | 说明 |
|---|---|---|
| dispatcher（+dockerdriver） | 5,425 prod / 7,436 test | 单一职责最大目录；单副本无 HA（`dispatcher/types.go:11-12` 自认） |
| internal/app/functions | 3,984 prod / 5,435 test | 聚合根注入 13 端口（`functions.go:19-63`） |
| internal/infra/functions（+runner） | 1,871 prod / 3,623 test | dispatcher/packer 客户端、zipstore、runner.js |
| internal/domain/functions | 1,702 prod | 端口与实体 |
| packer + cmd/packer | ~1,150 prod | git 打包进程 |
| worker + cmd/worker | 2,183 prod | 队列/cron/事件消费以 functions 为载荷 |
| API/CLI/SDK/proto/bunrepo | ~7,100 | servergrpc 664 行、serverhttp 两 handler、cli ~2.2k、sdk ~3.5k、function_repo 851 |
| **合计** | **~2.5–2.8 万行 Go（含 ~1.1 万测试）** | 横跨 5 进程形态 |

**定性**：底座已重写四代（v1 CGI → v2 dispatcher 池 → 多机细胞 → fleetly swarm），执行面是全仓迭代最频繁的子系统；安全面最深重（docker.sock 等同宿主 root、egress 二分类、zip 炸弹/滑移、npm 供应链、SSRF、token 串号修复史）；roadmap 风险登记首位（`docs/roadmap.md:555`）。而 fleetly proto 的服务注释把分工错位写得最直白：`tasks.proto:25-27`——平台管 API/网络/配额/审计/回收，**池语义留给调用方**。dispatcher 的 pool.go/registry.go（1,353 行 prod + 两段 Lua 原子脚本 + 六字段生命周期交叉）正是「留给调用方」的那份账单。

**结论**：问题不在「要不要服务端逻辑」（品类刚需），在「谁跑它」。v4 把答案改成：**任何常驻服务**，torchwood 只负责把调用安全可靠地送达并记账。

## §2 重命名：保留面语义对齐（内部验证期一次性完成）

执行语义外包后，旧词汇（Deployment/Executor/Execution）继续保留会误导——它们都在暗示「torchwood 在本地部署/执行」。趁无兼容负担一次改齐。

**四条命名原则**：

1. **产品域名 `Functions` 保留**——行业词汇，调用面语义未变，变的是执行位置（实现细节）。
2. **「一次调用」统一为 invocation**——torchwood 从此只发起调用、不执行。
3. **execution 身份词汇保留**（`twx_` 前缀、`fromExecution`、`ExecutionTokenService`、执行 principal）——指代「某次执行的身份上下文」，无论代码在哪跑都成立，且已在 SDK 定型（`@torchwood/sdk` v0.4.0）。
4. **「谁执行」进 target**——执行位置是一等配置，不再散落在池字段与 driver 配置。

**映射表**：

| 面 | 现名 | v4 名 / 处置 |
|---|---|---|
| server RPC | `CreateFunction/Get/List/Update/DeleteFunction` | 保留（字段重塑：池字段迁出，target 迁入） |
| server RPC | `ListRuntimes` / `ListSpecifications`（functions.proto:76/:82） | **退役**（运行时目录归 substrate） |
| server RPC | `Create/List/Get/DeleteDeployment`（:121-140） | **退役**（部署台账真身在 fleetly / 用户侧） |
| server RPC | `Set/GetVariables`（:147/:155） | **退役**（D9——env 注入点本就是部署：fleetly 部署下发或用户自管；PC-6 评审本就批评明文 variables 列非平台原语） |
| server RPC | `SetFunctionScopes`（:184） | 保留（declared_scopes 属执行身份模型） |
| server RPC | `CreateExecution`（:162） | **`InvokeFunction`**（与 client 面同名对齐，一个动作一个名字；async 经 option） |
| server RPC | `Get/ListExecutions`（:175/:168） | **`Get/ListInvocations`** |
| server RPC | 触发器四 RPC（:198-221） | 保留 |
| server RPC | — | **新增 `RotateFunctionSecret`**（endpoint target 签名密钥轮换） |
| client RPC | `InvokeFunction`（client/v1/functions.proto:81） | 不变 |
| domain 端口 | `Executor`（executor.go:181-199，四方法） | **`Invoker`**——唯一方法 `Invoke(ctx, Invocation) (*InvocationResult, error)`；Build/ImportImage/RemoveImage 退役 |
| domain 结构 | `Execution`（executor.go:20-80） | **`Invocation`**（池五字段删除；TriggerEnvelope 保留） |
| domain 结构 | `ExecutionResult`（:83-101） | **`InvocationResult`**（字段保留——透传面收敛于此，§6.3） |
| domain 端口 | `ExecutionTokenService` | 保留（原则 3） |
| 表 | `function_executions` | **`function_invocations`**（幂等唯一索引、限频 partial 索引、保留窗随迁改名） |
| 表 | `functions` 池策略列（迁移 000014/000017） | **退役**（池策略归 substrate） |
| 表 | `function_deployments` / `function_variables` | **退役**（D9） |
| 表 | `function_triggers` | 保留 |
| 表 | — | 新增 `webhook_subscriptions` / `webhook_deliveries`（§7） |
| 配置 | `functions.dispatcher/packer/driver` 段、`functions.docker.{host,network,callback_container}`、`functions.image`、`functions.storage` | **退役**（§11 精确表） |
| 配置 | `functions.fleetly`（config.proto:268-289） | 保留，消费方由 dispatcher 改 server/worker（网络挂靠用途，§10） |
| 配置 | — | 新增 `functions.invoke`（超时/响应上限）与 `functions.webhooks`（重试梯度/DLQ） |
| FDK | `StartInvoke/StartCron/StartEvent/StartHTTP`、`Mux` | 保留（内部改为 JSON-RPC method 分发，§8）；新增 `twfdk.Verify` 验签中间件（§8） |
| CLI | `functions deployments *`、`functions deploy`、`functions variables *` | **退役**（无部署面） |
| CLI | `functions executions *` | **`functions invocations *`** |
| CLI | — | 新增 `webhooks` 命令组 |
| Console | deployments 页、pool-policy 卡、variables 卡 | 退役；executions 页改名 invocations；新增 Webhooks 页与 target 编辑器 |

## §3 单一 endpoint target 模型（D8' 已拍板）

部署全面移交 fleetly CLI 后，「fleetly 专属 target」在 v4 核心失去必要性：fleetly 上托管的函数就是一个常驻服务 URL（FDK 容器跑成 fleetly app），与 k8s Service、裸机进程走**完全相同**的签名 HTTP 调用协议。

```protobuf
// Function.target —— v4 只有一种 kind，proto 留扩展位
message FunctionTarget {
  // https 强制（生产；开发环境可放行 http，配置开关）
  string url = 1;
  // 签名密钥的存储引用（实施勘误 2026-10-15：签名方每次调用需明文计算
  // HMAC，哈希落库无法签名——密钥本体加密落库 AES-GCM〔平台密钥派生，同
  // roles_sig 派生先例〕，API 面掩码回显，明文仅创建/轮换时回显一次）
  string secret_ref = 2;
  // 无响应风格字段：响应形状由 §4 调用协议的 method 语义决定（invoke → result
  // 即函数返回值；http.request → result 即 HTTP 响应表示），不需要按函数配置
  // 预留：oneof kind { endpoint, fleetly } —— fleetly kind 仅当需要原生
  // warm invoke/指标/日志透出时立项（§9），v4 不实现
}
```

**推论**：

- torchwood 函数面**彻底 substrate-blind**——fleetly/k8s/裸机/Cloud Run 无差别，比「适配 fleetly 与其他运行时」更强的满足「不绑死 fleetly」。
- **池策略/扩缩/运行时版本/资源规格在真正拥有它们的地方配置**：fleetly app 副本数、k8s HPA、用户自管。torchwood 不代理任何基础设施旋钮——今天 `Function` 的池五字段（functions.proto:256-263）与 `PoolPolicy` 透传链（dispatcher/types.go:122-131）整体退役。
- **常驻即无唤醒**：用户裁决「按常驻服务来调用最简单」——torchwood 不做 0→1 唤醒、不做保温、不做排队。substrate 侧若要 scale-to-zero（如 fleetly 日后原生 functions face），那是 substrate 在同一 URL 后面自己做的事，对 torchwood 透明。
- **超时与过载语义**：invoke 超时 = 函数记录的 timeout（透传为请求超时）；目标过载/失联 = 快速失败（§5.3），不再有 dispatcher 的有界排队 32 + 队首超时 10s + TimeoutBudget 熔断链。

## §4 调用协议（D1 + D10：RPC 轴 JSON-RPC 2.0 / 事件轴 CloudEvents 1.0）

协议按**语义轴**双轨（D10 拍板，判定准则 =「有没有调用方在等应用层结果」而非触发器类型）：

| 轴 | 协议 | 通道 | 结果去向 |
|---|---|---|---|
| **RPC 轴**（请求-响应，有 id） | JSON-RPC 2.0 | `invoke`（client/server，含 async）、`http.request`（**sync 与 async_ack 统一**——同触发器不因响应模式换协议，async_ack 只是平台不把 result 回传外部调用方的投递策略） | 调用方 / invocation 行 |
| **事件轴**（通知，无应用层结果） | CloudEvents 1.0 structured mode | `cron.tick`（平台自产时间事件，misfire catch_up_once 即重投语义）、事件触发（document/system，type = 事件目录名）、webhook 投递（§6） | 2xx=送达；非 2xx 按重试梯度重投 → 终态失败行=DLQ |

FDK 单端点按 body 探测分流（有 `jsonrpc` → RPC 轴、有 `specversion` → 事件轴）；同一签名方案罩两面。

### §4.1 RPC 轴：JSON-RPC 2.0 over 签名 HTTP

**请求**（torchwood → endpoint，`POST {target.url}`）：

- Body = JSON-RPC 2.0 request；**method 承接调用语义路由**（收编现「TW_DATA 封套 + X-Tw-Source 头」双维度为一）：

| method | params | 对应现状 |
|---|---|---|
| `invoke` | 函数载荷（原 TW_DATA JSON） | client/server 调用面（FDK StartInvoke） |
| `http.request` | `{envelope, body_b64}`（TriggerEnvelope，executor.go:13-18） | HTTP 触发器封套（sync 与 async_ack 同形态）；fetch 语义经 result 形状表达 |

- `id` = execution id（请求关联白送；FDK 回显）。
- 签名/身份走 header（传输与载荷分离）：
  - `X-Tw-Signature: t=<unix秒>,v1=<hex HMAC-SHA256(secret, "t." + body)>`——Stripe 风格；5 分钟时间窗防重放；验签方常数时间比较；`v2` 双密钥轮换窗口（RotateFunctionSecret 后旧密钥保留一个窗口期）。
  - `X-Tw-Execution-Token: twx_…`——执行身份，铸造/校验链路不变（`ExecutionTokenService`，TTL = 超时 + 60s）。
  - `X-Tw-Source / X-Tw-Invoking-User-Id / X-Tw-Project-Id`——身份投影（沿用 FDK 头族，serve.go:34-45）。`X-Tw-Execution-Id` 头退役（由 JSON-RPC `id` 承载）；`X-Tw-Trigger-Envelope` 头退役（进 `http.request` params）。
  - `X-Tw-Protocol-Version: 1`——JSON-RPC 本体由 `"jsonrpc":"2.0"` 自版本化，此头管 method 词表/头语义演进（吸取 runner TemplateVersion 教训）。

**响应**（JSON-RPC over HTTP 惯例——送达成功恒 HTTP 200，函数成败在 body）：

```json
{"jsonrpc":"2.0","id":"…","result":<函数返回值——main 风格直接是函数 JSON>}
{"jsonrpc":"2.0","id":"…","result":{"status":201,"headers":{…},"body_b64":"…"}}     // http.request 的 fetch 语义结果
{"jsonrpc":"2.0","id":"…","error":{"code":-32000,"message":"函数错误文本","data":…}} // 函数报错（应用层，合法结果）
```

- error code 区段：-32700..-32603 协议层保留（FDK 验签通过后的解析失败等）；函数应用错误用 server error 区段（-32000..-32099 及自定义），torchwood 不消费语义、只透传记录。
- **平台错误 vs 函数结果边界（代码核实定界）**：非 2xx / 连不通 / 超时 / 验签协议不符 = **平台语义错误**（`Unavailable`/`DeadlineExceeded`，HTTP 触发器应答 502/504）；2xx + JSON-RPC body = 函数结果（含 error 对象）。旧模型靠 `StatusCode>=100` 约定（`executions.go:346-348`），新模型靠 HTTP status 语义位 + error 对象。`finishTransparent`（function_triggers_handler.go:325-353）消费面已按 `InvocationResult` 收敛，透传适配只换实现不改 handler。
- 大小上限：请求 body 沿用现有 413 校验；响应 ≤1MiB（对齐 `maxInvokeResponseBytes` 口径，dispatcher/types.go:63-67）。
- **战略对齐**：MCP 构建在 JSON-RPC 2.0 上——roadmap P3「Functions as Tools」届时为函数暴露 JSON Schema（OpenRPC/MCP 工具定义）即同底座复用，无二次搬家。

### §4.2 事件轴：CloudEvents 1.0 structured mode over 签名 HTTP

**请求**（投递引擎 → 消费方 endpoint，`POST {target.url}`）：body 为 CloudEvents 1.0 **structured mode**（全部属性在 body 内——binary mode 的 `ce-*` header 不受签名保护，structured 整体落在签名圈内）：

```json
{
  "specversion": "1.0",
  "id": "<事件投递 ID（seq 派生或 UUID）>",
  "source": "<事件源：cron 触发器 = projects/{p}/triggers/{t}；文档/系统事件 = projects/{p}>",
  "type": "<cron.tick | 事件目录名（documents.created / auth.users.created …）>",
  "time": "<RFC3339：cron = scheduled_for；事件 = outbox 产生时刻>",
  "datacontenttype": "application/json",
  "data": { "<CronTick 或 EventInvocationData/SystemEventInvocationData 投影>" },
  "twproject": "<project id>",
  "twseq": <outbox seq（cron 无）>
}
```

- 签名头 `X-Tw-Signature` 与 RPC 轴同一方案（对整个 body 验签）；`X-Tw-Protocol-Version` 同。
- **响应语义**：2xx = 送达（FDK 回 204 No Content，无应用层结果）；非 2xx / 超时 / 连不通 = 送达失败，由投递引擎按 §6/D5 重试梯度重投，终态失败行进 DLQ——**函数事件触发与 webhook 投递共用此管道与语义**（消灭函数侧独有的「异步执行 attempt 重试」）。
- FDK 分发：`type=cron.tick` → data 解析为 CronTick → Cron handler；其余 → data 解析为事件投影 → Event handler；解析/处理失败回 400（= 失败信号，进入重投）。
- 语义损失入册（D10）：事件轴投递行不记录函数返回值，响应 body 尾部截断留档作审计替代。

## §5 统一出站投递引擎

endpoint 函数调用与 webhook 投递不是两套机制，是一个引擎两种 policy：

```
internal/pkg/ssrfguard      ← packer 拨号点校验抽包（gitpack.go:440-478 整迁：
  GuardedHTTPClient()          checkDialAddress 拒 loopback/private/link-local/
                               unspecified（覆盖 169.254.169.254）；Control 钩子在
                               connect 时校验真实 IP，DNS rebinding 天然失效；
                               重定向后拨号同样受控）
internal/infra/functions/
  invoker.go                 ← Invoker 端口实现：签名注入 + 超时 + JSON-RPC 响应解析
                               + InvocationResult 回填
internal/infra/webhooks/
  delivery.go                ← 同引擎：投递 policy = 重试梯度 + DLQ（§7）
```

- **出站姿态**：server（sync invoke）与 worker（async/webhook）均为出站源。SSRF guard 默认强制；用户配置的任意 URL（SaaS 多租户姿态）不放行私网——**实施勘误（2026-10-16，破坏波 A）**：自托管 fleetly 部署下函数常驻服务的 target.url 是任务网内网服务名（解析为 RFC1918 私址），守卫默认会拒绝，与 §4.2「私网寻址」需求正面冲突。解法 = 平台级配置开关 `functions.invoke.allow_private_targets`（默认 false；自托管运营方显式开启——这是运营者的拓扑信任决策，非 per-request 开关，与 packer `allow_insecure` 的自托管内网 git 同一信任模型）。`functions.fleetly` 配置段随之退役（网络挂靠是 fleetly 部署拓扑〔compose members〕的责任，torchwood 零 substrate 感知）。
- 观测：per-target-kind（function/webhook）的时延/成功率/在途计数指标；dispatcher 池指标族（PoolReady/PoolBooting/PoolDraining/InstanceInflight 等）随池退役。

## §6 Webhooks 一等公民（D5）

roadmap 原排 P3（`roadmap.md:31/:377/:412`「P2 只做内部消费」）——v4 提前并做厚，事件反应类需求从此**无需在平台跑代码**。

**订阅模型**（新 `proto/server/v1/webhooks.proto`）：

- `Webhook` = URL + 订阅串集 + 密钥 + 状态（启用/暂停）。订阅串**复用 eventmatch 内核**（`domain/functions/eventmatch.go`）：系统形态「目录展开 + 全名查表」（`MatchSystem`，:231-240）与文档写形态（`databases.{db}.collections.{col}.documents.{op}`，:99-123）原样可用；需把订阅载体从 `TriggerID` 换成 `WebhookID`（`EventSubscription` 结构 :143-148 去函数绑定）；上限沿用 64 条 × 200B（:31-34）。一期**路由级过滤**（与函数事件触发器同口径），负载级过滤不做。
- RPC：`CreateWebhook / GetWebhook / ListWebhooks / UpdateWebhook / DeleteWebhook / RotateWebhookSecret / ListDeliveries / GetDelivery / RedeliverDelivery`。

**投递语义**：

- 消费 outbox 事件脊柱的新消费组 `webhooks`（与 `functions-triggers` 同模式：XREADGROUP、at-least-once、停机补投经 outbox 按 seq 区间回放——`worker/event_triggers.go:437-489` 同款水位机制，新水位键 `torchwood:wh:lastseq`）。
- 载荷：**CloudEvents 1.0 structured mode**（§4.2——与函数事件触发同一信封同一管道；type = 事件目录名，data 投影 ≤32KB 与函数事件触发器同口径）；签名同 §4 协议（`X-Tw-Signature`，per-webhook 密钥）。
- 重试梯度（D5 已拍板）：即时 → 10s → 1m → 5m → 30m → 2h → 8h ×2 …约 15 次封顶 24h；终态失败行进 DLQ（`webhook_deliveries` 表 status=failed 可浏览），`RedeliverDelivery` 手动重投。
- 顺序：per-webhook at-least-once、尽力保序（不承诺严格序，文档声明）。
- 配额与计量：per-project webhook 数上限、投递速率上限；投递量/时延一期仅进 metrics、不进 billing（D5 拍板）。

## §7 invocation 记账定案（D3，已核实，不再开放）

**两写预占骨架保留**——`INSERT running 预占 + 终态 UPDATE`（`executions.go:243-254`、runExecution :307-355）本来就是纯 DB 记账、与 dispatcher 零依赖，且三链挂在行上：

1. **幂等**：客户端调用幂等去重 = 唯一索引 `function_executions_client_idem_key`（迁移 000016:29-31）INSERT 冲突检测（23505 归一 `ErrExecutionIdempotencyConflict`，function_repo.go:352-369）+ 按键回读（:373-392）。无 Redis 兜底——不写行幂等即破。
2. **限频降级**：Redis 限频器失效时走 DB 降级计数 `CountClientInvocations`（clientinvoke.go:181，partial 索引 000016:36-38）。
3. **审计**：`ClientInvoke` 在 AuditInterceptor 跳过清单内，执行行（含 invoking_user_id）就是其审计载体（clientinvoke.go:26-27 注释自认）。

**状态机收敛**：`building` 态存在的唯一理由是 worker 补构建（`executions.go:432-457`），构建外包后删除。状态集 `queued/building/running/completed/failed`（repo.go:142-146）→ **`queued/running/completed/failed`**；CAS 领取改 `queued→running`（TransitionExecutionStatus，function_repo.go:492-511）；孤儿恢复/终态兜底的 IN 清单随改（:551-579/:513-533）；保留窗清理（PruneOld / PruneTrigger 48h，:586-653）与幂等索引、限频索引、保留窗表名随迁 `function_invocations`。

**计量**：`function_duration_ms`（billing.go:14）语义改为**网关往返时长**（Invoker 侧计时，原 `meterDuration` executions.go:529-535 位置不变）。

## §8 FDK 改造（Go + TS 同期）

核实结论：FDK 的解包/路由/身份回访已齐备（`sdk/go/functions`），**唯一硬缺口是验签**——今天完全信任内网（serve.go:67「内网可信调用方」），公网 BYO 下任何能连到端口的人可伪造 `X-Tw-Execution-Token`。

| 改造项 | 内容 |
|---|---|
| **验签中间件（核心）** | `twfdk.Verify(secret)`：校验 `X-Tw-Signature`（时间窗 + 常数时间比较 + 双密钥窗口），失败 401；Go/TS 同型实现，TS 版进 `sdk/typescript`（dogfood 函数是 JS，**TS FDK 是一期硬依赖**——桥接全靠它） |
| JSON-RPC method 分发 | `Mux` 路由维度从 source 前缀迁至 method 名（`invoke`/`cron.tick`/`event.document`/`http.request`，§4）；`Start*` 语义不变、内部承载改造；`id` 回显进响应 |
| 常驻化 | 去 runner 假设：`TW_RUNNER_PORT`/`TW_MAX_REQUESTS` 自回收/`/_tw/health` 改为可选（常驻服务不需要请求计数自退出；health 探针保留为部署平台可选用） |
| 密钥分发 | `functions create` / `RotateFunctionSecret` 回显明文一次 + 哈希落库；env 注入由用户部署侧自管（D9 一致） |
| 本地开发 | `torchwood functions dev` 保留改造：本地起 FDK server + 环境变量注入本地密钥，联调本地/远端 server |
| runner.js | **退役出 torchwood**（容器内 runner 契约归 substrate 侧——fleetly 若做原生 functions face 自行演化；torchwood 不再持有 `internal/infra/functions/runner/`） |

## §9 fleetly 协作定位（D2 已拍板：实现后置、提案先行）

- **v4 实施零依赖 fleetly 新面**：FDK 常驻服务用现有 fleetly deploy（compose/build-from-upload）托管；server 挂靠项目任务网用现有 `EnsureTaskNetwork` members；一步到位切换（§11）不等待任何 fleetly 开发。
- **提案先行（owner 裁决）**：fleetly 原生 functions face **现在起草提案、owner 审阅后再排开发**——草稿如下，定稿后提交 fleetly 仓立项：

> **提案草稿：fleetly Functions face（native 托管）**
>
> - **动机**：v4 后函数 = 常驻 FDK 服务；fleetly 原生托管可在同一处提供 warm 保温/scale-to-zero 选项、指标与日志投影，torchwood Console 经 fleetly kind 透出，缓解规模化后的常驻成本。
> - **范围（deploy + run only，触发器调度不进）**：FunctionsService——`DeployFunction`（复用现有 build-from-upload/compose 产物 + env 下发 + 副本/资源策略）→ 返回稳定 URL；`GetFunctionStatus` / 指标 / 日志投影；`ScaleFunction`。
> - **协议纪律**：调用协议唯一事实源在 torchwood（§4 JSON-RPC/签名 HTTP）；fleetly 只做托管与保温，**不定义第二协议**，`X-Tw-Protocol-Version` 对齐；FDK 是协议参考实现。
> - **torchwood 侧配套**：`FunctionTarget` 预留的 fleetly kind（§3）——本质是「URL 供给 + 元数据透出」的增强，不是第二调用路径。
> - **启动与验收**：owner 审阅通过后排期；验收 = 一个 dogfood 函数经 fleetly face 部署、torchwood Console 可见状态/指标、调用路径与 endpoint kind 逐字节一致。

## §10 退役清单（终版）

**进程/组件**：`cmd/dispatcher` + `dispatcher/` 全包（含 dockerdriver）、`cmd/packer` + `packer/` 全包（SSRF guard 抽包迁走后）、`internal/infra/functions/` 的 dispatcher_client.go / packer_client.go / zipstore.go / runner/（runner.js）、`internal/app/functions/` 的 deployments.go / deployments_git.go / deployments_image.go / rebuild.go、`internal/domain/functions/` 的 sourcepack.go / zipstore.go / runtime.go 及 Executor 端口的 Build / ImportImage / RemoveImage 三方法；`docker/functions-runtime-node`、`docker/functions-hello` 镜像。

**API 面**：部署四 RPC、变量两 RPC、runtimes/specifications 两 RPC（proto reserved 纪律）；serverhttp multipart 上传路由（functions_handler.go 50MiB 面）；grpc_gateway 对应注册。

**表/迁移**：`function_deployments`、`function_variables` 表退役；池策略列退役（000014/000017 相应列）；`function_executions` → `function_invocations` 改名迁移（幂等/限频/保留窗索引随迁）；新增 webhook 两表迁移。

**配置**（config.proto 精确退役表）：

| 配置段 | 处置 |
|---|---|
| `functions.dispatcher`（:162-223，11 字段） | 退役 |
| `functions.driver`（:228-233） | 退役 |
| `functions.docker.{host,network,callback_container}` | 退役 |
| `functions.packer`（:312-344，8 字段） | 退役（git 源整体消解） |
| `functions.image`（:345-363） | 退役（torchwood 无镜像准入） |
| `functions.storage`（:364-373） | 退役（零代码字节持有） |
| `functions.execution`（:153-161）/ `functions.trigger`（:290-297）/ `functions.client_invoke`（:298-311） | 保留 |
| `functions.fleetly`（:268-289） | 保留，消费方改 server/worker（网络挂靠） |
| 新增 `functions.invoke` / `functions.webhooks` | 超时/响应上限；重试梯度/DLQ/配额 |

**Redis 键**：`torchwood:fninst:*`/`fnimg:*`/`fnspawn:*`（实例注册表/镜像映射/spawn 锁）与 rebuild dedup 键退役；`fnq:*`（限频）、`torchwood:fnevent:lastseq`（事件水位）、执行 token 键、`torchwood:queue:functions-executions`（异步队列，改投递 invocation）保留；新增 `torchwood:wh:lastseq`。

**连带**：项目删除 purge 的 zip 桶清理（`internal/app/server/projects.go:66-71`）退役；audit 面部署/触发器审计裁剪；egress 分类逻辑（`EgressUntrusted` 链路）退役——网络隔离归 substrate（fleetly internal task-group / k8s NetworkPolicy / 用户自担）；wire/compose（local、dokploy、fleetly 三套）去 dispatcher/packer 服务。

**测试预算**（代码核实）：app/functions 22 文件 145 测试函数中约 80–90（55–60%）实质改动——构建/镜像链 7 文件 ~51 函数必然重写，Execute 载荷断言面追加 30–40；worker 侧 `retryExecutor` 等桩随端口收窄调整；验收面仅 1 函数 + 桩（acceptance 已桩化，无 dispatcher 进程依赖）；`dispatcher_client_test.go` 整文件随删。

## §11 迁移（D4 已拍板：一步到位）

单一发布窗口完成切换，**无双轨并存期**（owner 裁决：内部验证期 dogfood 分钟级停机可忍，以风险集中换取总工作量最小、无双轨维护）：

```
发布窗口内（一个 PR 族，提交序即依赖序）：
① 新面落地：FDK 验签（Go+TS）+ JSON-RPC 协议（§4）+ endpoint target
   + Invoker 投递引擎（§5）+ Webhooks（§6）+ invocation 改名迁移（§7）
② dogfood 重部署：函数经 fleetly CLI 部署为跑 FDK（TS）的常驻 app
   → torchwood 注册 endpoint target → 触发器指向切换（分钟级停机）
③ 旧面删除：dispatcher/packer 进程、部署/变量/运行时 RPC、
   runner/zipstore/rebuild、配置与 Redis 键（§10 全清单）
```

- 回退策略：发布前 git tag + staging 全流程预演一次；窗口内任一环节失败 = **整体回滚**重来（一步到位的固有代价，owner 已接受）。
- fleetly 原生 functions face 不在本窗口（§9：提案先行、实现后置，互不阻塞）。

## §12 决策点汇总

| # | 决策 | 拍板（2026-10-15 owner） |
|---|---|---|
| D1 | 调用协议 | **JSON-RPC 2.0 over 签名 HTTP**（method 词表承接触发器路由；自有封套否决） |
| D2 | fleetly 原生 functions face | **实现后置 + 提案先行**（§9 提案草稿随本稿呈审，审阅后排开发） |
| D3 | invocation 记账 | 定案保留两写（幂等/限频/审计三链，§7 证据），不再开放 |
| D4 | 迁移节奏 | **一步到位**（单一发布窗口、无并存期、回退 = 整体回滚） |
| D5 | Webhooks 规格 | **15 次/24h 指数退避 + 行内 DLQ + Redeliver API；计量一期仅进 metrics 不进 billing** |
| D8' | target 模型 | **单一 endpoint target**（proto 留 fleetly kind 扩展位） |
| D9 | Variables 面 | **随 v4 退役**（env 归部署侧：fleetly 下发/用户自管） |

## §13 风险与对策

| 风险 | 对策 |
|---|---|
| 一步到位发布风险（无并存期，回退 = 整体回滚） | 发布前 git tag + staging 全流程预演；窗口选 dogfood 低峰；分钟级停机 owner 已接受 |
| TS FDK 一期工作量（dogfood 全是 JS 函数） | 一期范围收窄为「验签中间件 + 身份头解析 + invoke method 分发」三件；http.request（fetch 语义）二期 |
| 公网 egress 姿态转变（server/worker 成为出站源） | ssrfguard 默认强制无开关；响应大小/类型上限；出站指标与告警；渗透自查进发布前置门 |
| 协议双面漂移（torchwood ↔ FDK） | `X-Tw-Protocol-Version` 版本头 + FDK 对旧版本宽容窗口；协议契约测试双仓同步 |
| SLA 重估 | 热路径少两跳（server→endpoint 直连，原 server→dispatcher→容器）；P99 ≤ 100ms 平台开销 ≤ 25ms 口径重测，预期更易达成 |
| dogfood 常驻成本 | fleetly app 常驻副本 × 函数数——内部验证期可接受；规模化压力即 §9 提案的启动信号 |

## Alternatives（否决留档）

- **WASM/进程内沙箱底座（wazero+QuickJS）**：v3 死亡区裁决（functions-v3.md:40）本可重开（goja 否决理由对 capability-based wasm 不成立），但与「执行面外包、torchwood 零编排」路线根本不同向——留作 substrate 侧的未来选项（fleetly 若做原生 functions face 可评估），torchwood 不再持有执行底座。
- **Substrate 端口 + 四实现（docker/k8s/裸机/fleetly）+ fnagent**：owner 否决——为保留 torchwood 编排能力而造的机器，与减复杂度目标背道而驰。
- **只换部署单元不动池（slug + stock 镜像）**：保守半措——保留最难的池状态机，复杂度只减一半。
- **纯 Webhooks 无函数调用面**：品类完整性不成立——「货币/库存只能云函数写」的同步客户端调用刚需（竞品分析 §三）无纯投递替代。
- **dispatcher 池语义整体搬入 fleetly（原「fleetly 中实现 Functions API」字面解）**：触发器调度离不开 torchwood 事件脊柱/DB，切分线应在「调度 vs 执行」而非「整体搬家」；且 v4 单一 endpoint target 下 fleetly 无需任何特殊面（§9）。

---

> 附：本稿证据行号均以 2026-10-15 main（f51ef6db）为基准；实施期行号漂移时以符号名为准。
