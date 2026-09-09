# 函数平台补全：执行身份、触发器与客户端调用面

> 状态：**已批准（2026-09-09 owner 拍板：K1–K10 全部裁决、Open Questions Q1–Q13 全部收口；按 P0 → P0.5 → P1 → P2 → P3 分阶段实施）**
> 复审：2026-09-09 二轮复审修正 7 项——SSV 1s 超时×冷启动冲突（双响应模式）、HTTP 封套缺 query string、配额-保留策略交互 bug、cron misfire 危险默认、匿名会话洗配额、proto 冗余字段、K4/Q7/Q9 收口（各节内联标注）
> 定位对齐（2026-09-09 owner 澄清）：产品为**通用型 BaaS——增强基础能力、组合实现具体需求**。本稿全部交付物均为水平原语；per-user 限频由「每日配额」泛化为可配窗口；方案 A 从「后置」改为「不再独立立项」（K6/K7 与组合示例见 Rollout）
> 执行器裁决（2026-09-09 owner）：「每请求一个容器进程」确认为设计缺陷（CGI 形态）——常驻 runner 升级为**默认执行模型 v2**（原 §6「可选 warm pool」吸收重写为 resident-first，冷启动 = 池 0→1 扩容的单一路径）；新增 **P0.5 阶段**：先修地基，再开新面
> SLA（2026-09-09 owner）：**热路径同步分发，简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）**；冷启动（池 0→1）与异步队列路径显式不在 SLA 内——不同档位不伪装成同档（§6 预算表）
> 独立复核（2026-09-09 三轮·双代理）：事实核查（代码断言全数命中，措辞微调）+ 对抗评审修复 4×P0——SLA 预算前置清账（鉴权 4 次 DB 往返/同步审计/部署全量拉取）、单写记账改**两写预占**、分发通路升格为**阻断性决策**（原 Q9）、DocRole 数据面接合；另修 P1 六项与 Rollout 重复行（各节「三轮复核」标注）
> **Owner 拍板（2026-09-09，四主 + 六默认，全部锁定）**：分发通路 = **独立 functions-dispatcher 进程**（Q13 联动收口）；token = **Redis 不透明 token**（主动 DEL 吊销）；客户端调用 = **同步默认**；egress = **不可信函数默认 deny + per-function 白名单**。默认锁定：cron UTC（可配后置）、匿名一期不开、runner 协议容器内 HTTP、URL `/f/{project}/{token}`、常驻上限每 daemon 8、多路复用维持串行
> 动机：BaaS 定位的完整性原语——「客户端可触发的服务端逻辑」是全行业的品类基础设施（竞品分析 §三），也是自有 dogfooding 小游戏 serverless 化的唯一完成路径（竞品分析 §八）
> 前置评审缝：`docs/review/saas-baas-design-2026/04-platform-capabilities.md` PC-5（触发器模块）、PC-6（SecretResolver）
> 关系：`client-self-consume-assets.md`（方案 A）转为本稿 Rollout P3 的复盘项；决策依据与竞品证据见 `economy-client-write-competitive-analysis.md`

---

## Overview

函数执行引擎已就绪（Docker 隔离、资源限额、审计），缺的是三块平台设施，本稿一次规划、分三阶段落地：

1. **P0 执行身份**：函数执行获得平台注入的短期受限凭证（execution principal），替代「开发者往 variables 塞长期 API key」；
2. **P0.5 执行器 v2**：常驻 runner 为默认执行模型——容器长驻、按请求分发、按策略回收，冷启动 = 从零扩容（单一代码路径，见 §6）；
3. **P1 触发器模块**：HTTP 触发器（公开 URL、封套透传、双响应模式）+ cron 触发器（misfire 补跑），喂进既有 CreateExecution 内部路径；
4. **P2 客户端调用面**：Client API（END_USER）可按 per-function 策略调用函数，平台强制可配窗口的每用户限频。

**设计原则（产品定位）**：本稿只交付水平基础能力（执行身份、触发器、调用面、限频），微信广告奖励等任何具体需求一律由原语**组合**实现（组合示例见 Rollout）——垂直专用端点不属于本稿范围，也不应出现在本平台的路线图上。

```
                    ┌────────────────────────────────────────────┐
 微信 SSV 回调 ────▶│ HTTP 触发器 ──┐                            │
 cron 每日重置 ────▶│ cron 触发器 ──┤                            │
 游戏客户端 ───────▶│ Client API ───┤→ CreateExecution 内部路径  │
 admin / API key ──▶│ Server API ───┘        │                   │
                    │              执行身份铸造（短期 token，     │
                    │              per-function 声明 scope）     │
                    │                    │                       │
                    │              Docker 容器（既有）           │
                    │              env: TW_DATA + TW_EXECUTION_TOKEN
                    │                    │                       │
                    │              Server API（Bearer token）    │
                    │              → assets grant/consume 等     │
                    └────────────────────────────────────────────┘
```

## Background & Motivation

### 现状（2026-09-09 代码核实）

- **引擎底座好**：Docker 容器隔离（固定模板 Dockerfile、只读根 fs、CapDrop ALL）、超时 1–300s（同步 ≤30s）、0.5–1 CPU/256–512MB、输出 64KB 截断、输入 ≤32KB、Redis 信号量（build 4 / run 16）、容器级 SIGKILL 超时可靠、全量 gRPC 审计。
- **三层断点**：
  1. **凭证**：执行容器内零平台身份（仅 `TW_DATA` + 用户 variables）；函数调平台能力需开发者自存长期 API key 于 `function_variables`（**明文列**），且 stdout/stderr 截断（≤64KB）后回存 `function_executions`（functions.read 可读）——key 打印进日志即泄漏（评审 PC-6）。
  2. **触发器**：仅 Server 面 HTTP invoke（同步/异步队列）；无公开 URL、无 cron、无事件触发（评审 PC-5：「缺的不是 Client CreateExecution，是平台触发器模块」）。
  3. **客户端面**：END_USER 凭证族在 proto（无 client functions proto）、网关、use-case（`RequireServerPrincipal`）三层被挡。
- **竞品定位**：身份自动注入是全行业入场券（PlayFab currentPlayerId / Unity context.playerId / Parse request.user / 微信 openid，均「server-controlled and safe」），没有任何主流平台让开发者手工塞长期 key；客户端可调用函数是通用 BaaS 的品类定义能力。见竞品分析 §三/§八。

### 为什么分阶段而不是一步到位

P0/P0.5/P1 服务**所有**函数用户（含纯 Server 面用法），是平台欠账的修复——其中 P0.5 修复的是执行器地基（§6）；P2 才打开不可信触发面。**先修地基再开新面**：触发器与客户端调用面的延迟 SLA 都建立在执行器 v2 之上，不希望在已判定为缺陷的基座上叠加新面再返工。自有 dogfooding 游戏无上线压力，恰好按阶段验收：P1 验收场景 = 微信 SSV 回调发奖（HTTP）+ 赛季/每日重置（cron）；P2 验收场景 = 签到/成绩提交/广告奖励即时暂发。P0–P1 期间游戏广告奖励功能暂不上线，**无需任何止血方案**。

## Goals & Non-Goals

**Goals**

- 函数以最小特权、短期凭证访问平台能力，消灭长期 key 落地执行环境；
- HTTP + cron 两类触发器，覆盖 SSV/webhook 回调与定时结算（serverless 化验证圈定的必要范围）；
- 终端用户按 per-function 显式策略调用函数，平台强制可配窗口的每用户限频与并发；
- **执行器 v2（常驻 runner 为默认）**：容器长驻、按请求分发、按策略回收；冷启动 = 从零扩容的统一路径，消除每请求重复支付的 runtime 启动/模块加载/连接重建成本，服务时限敏感（SSV 同步回包）与高频轻量场景；
- 全程可审计：执行记录关联触发来源与（客户端触发时）调用用户。

**Non-Goals**

- 事件触发器（outbox 订阅）——后置（P1 只做 HTTP + cron）；
- 函数间编排/工作流、VPC/egress 管控——独立议题（egress 管控作为 P2 的安全前置评估项，见 Open Questions）；
- 声明式 self-consume（方案 A）——不再立项，P3 数据复盘（K6）；
- 每实例多路复用（单实例并发 >1）——一期常驻实例串行（1 并发/实例，并发 = 池大小），复用要求函数可重入，后置（Open Questions Q12）；
- 权威实时对战、MinIO 之外的函数代码多机分发（单机假设维持，改造另立项）。

## Proposed Design

### 1. 执行身份（execution principal，P0）

**per-function scope 声明**：`functions` 表加列 `declared_scopes TEXT[]`（默认空 = 无平台访问权限，fail-closed），值为既有 ScopeResource 词表的子集（如 `{assets:write,databases:read}`；枚举单一事实源 `proto/shared/v1/authz.proto`）。CreateFunction/UpdateFunction 校验（app 层跨字段规则：去重、值域、deny 未知 resource）。**危险资源排除（三轮复核 P1）**：deny-list 禁止声明 `functions:*`（自我复制/递归触发——A 执行 B、B 再造 A，低成本递归放大吃光容器与信号量）、`projects:*`、`billing:*`、`outbox:*`；语义上只开放数据面资源（assets/databases/users/groups/storage/subscriptions/payments），app 层与 Console 双侧校验。

**短期 token 铸造**：执行开始时（`buildExecution`，`internal/app/functions/executions.go:470` 附近）铸造不透明 token——Redis `torchwood:exec-token:{token}` → JSON `{project_id, function_id, execution_id, scopes, invoking_user?}`，TTL = 函数超时 + 60s 宽限，经 `TW_EXECUTION_TOKEN` 环境变量注入容器。同步/异步/触发器路径统一。**实现注意**：同步路径在 server 进程、异步在 worker（`ProcessExecution`），两者共用 `internal/app/functions` 包与 Redis——铸造代码必须进程无关可复用；token/base URL 注入计入既有 env+data ≤32KB 预算（约 200 字节，量级无碍）。

**Server API 校验扩展**：`internal/api/interceptor/jwt.go` 新增凭证族——`Authorization: Bearer <execution_token>` 命中 Redis 则构造 `Principal{ActorKind: ActorKindExecution, ProjectID, FunctionID, ExecutionID, InvokingUserID?, Scopes}`；scope 强制复用既有 API-key scope 机制（`api_key_scope` 门方法照常生效）；`requireAssetWrite` 的 use-case 断言集（现状 system/admin-key，`internal/app/assets/authz.go`）**有意扩展**纳入 execution principal——这是 D6 设计内通道的落地（v3 §2.7 本就指定 Functions 为经济写主要场景），不是语义漂移；每次函数发起的写经账本 operator 溯源到 function+user。

**数据面角色接合（三轮复核 P0：缺了它 `databases:*`/`storage:*` scope 是空头支票）**：scope 门只是第一层——文档层 ACE/RLS 判定基于 principal.Roles（API key 携带 `Roles: [keys, key:<id>]`，即 B14 的 key 族数据隔离模型），而 DocRole 词表（`internal/domain/databases/docrole.go`）没有 execution 角色：execution principal 角色为空则函数**读不到任何文档**，错误形态还是最难排查的空结果。接合方案（取改动最小者）：execution principal 携带 `Roles: [keys, key:function:<function_id>]` 复用 B14 模型——开发者像授权 API key 一样把集合授予 `key:function:<id>`；storage ACL（`internal/app/storage` 用 RoleKeys）同路接合；Console scope 编辑处同步明示各 scope 的数据面语义。

**接合点清单（三轮复核 P1：实现时逐项对表，防漏改）**：①`jwt.go` SERVER 面凭证门（现状 `CredentialType != APIKey && ActorKind != Admin` 即拒）需加 execution 分支；②scope 门现在只在 `CredentialType == APIKey` 分支内求值，需平移覆盖 execution（含 `apiKeyScopeTargets` 资源实例寻址——assets 无实例寻址，databases/storage 有）；③`ratelimit.go dimension()` 回落顺序：execution 的 ActorID 非空会**先命中 user 维度**（每次执行独立桶 = 实质不限），`api:execution:` 维度必须在 user 回落**之前**特判；④`shared.ActorKind` 封闭枚举与全部 switch 需扩 `execution`。

**可审计性**：资产等账本 `operator` JSONB 记录 `{kind:"execution", function_id, invoking_user_id?}`——对标 PlayFab CloudScript 的 currentPlayerId 审计语义；审计拦截器照常覆盖。

**泄漏面收敛（三轮复核 P1 修正）**：token 在执行结束（server 快路径与 worker `ProcessExecution` 两处）**主动 DEL**，TTL = 函数超时 + 60s 仅作崩溃兜底——「执行结束即失效」是主动吊销语义，不是等 TTL。两个业界常用补强在本架构下**不可行**（显式记录，防实现期重提）：来源 IP 绑定——函数容器经 bridge NAT 呈现同一 IP（下条已证）；一次性 token——函数需要多次回访平台。剩余防线 = 最小 scope（默认空）+ 主动吊销 + 账本 operator 溯源 + execution 维度限频；stdout 回显风险由主动吊销收敛为秒级时效性，执行记录捕获层再加模式化脱敏兜底。

**网络通路与回调地址（P0 实施前提，2026-09-09 复核补充）**：函数容器挂 per-project bridge，而 **server 不在该网络上**（`docker.go resolveNetwork/ensureNetwork` 仅创建 bridge）——函数回调 Server API 只能经 NAT 走 server 的外部端点（文档 `08-functions.md:63`「无回环网络通道」印证；现状开发者硬编码地址，无平台注入）。因此 P0 需：①新增配置 `functions.execution.api_base_url`（对函数容器可达的 Server API 地址）并注入 `TW_API_BASE_URL` env；②部署文档显式声明可达性前提（自托管须有函数容器可达的端点地址）。内网直达优化（server attach 函数网络）见 Open Questions Q9。

**限流维度修正（2026-09-09 复核发现的 P0 地雷，三轮复核精化）**：通用限流按 API key > user > IP 三维度（`internal/api/interceptor/ratelimit.go`，per-IP 默认 300/min）。execution principal 两种「顺其自然」的走法都错：落入 IP 维度——函数容器全部经 bridge NAT 出网呈现**同一来源 IP**，全部执行共享 300/min 互相击穿；落入 user 维度——每次执行独立 ActorID 桶，实质不限。修正：限流拦截器为 execution principal 新增独立维度 `api:execution:`（按 project:function 计数，默认对齐 api-key 档 6000/min、可配），且必须在 user 维度回落之前特判（接合点清单③）。

### 2. function_variables 密钥缝（PC-6，P0 伴生）

`function_variables` 加列 `kind TEXT NOT NULL DEFAULT 'text'`（`text | secret`）。secret 值：`GetVariables` 沿用既有掩码回显；注入容器前经解析（一期仍同表明文存储 + 访问收敛，加密落库列为二期，理由：execution principal 上线后 variables 不再承载平台凭证，残留敏感面大幅缩小）。文档同步：平台能力一律用 execution token，variables 仅放第三方密钥。

### 3. 触发器模块（P1）

新实体（迁移编号顺排：P0 `000013_function_execution_identity`——functions.declared_scopes + function_variables.kind；P0.5 `000014_function_executor_v2`——池策略列；P1 `000015_function_triggers`；P2 `000016_function_client_invoke`——functions 策略列 + executions.invoking_user_id；projectschema）：

```sql
CREATE TABLE {{schema}}.function_triggers (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, function_id TEXT NOT NULL REFERENCES {{schema}}.functions(id) ON DELETE CASCADE,
  type TEXT NOT NULL CHECK (type IN ('http','cron')),
  config JSONB NOT NULL,           -- http: {token, response_mode: sync|async_ack, ack_body?, handshake: echo} / cron: {expr(5字段,UTC), misfire: skip|catch_up_once}
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  next_run_at TIMESTAMPTZ,         -- cron 专用
  created_at/updated_at ...
);
```

**HTTP 触发器**：

- 公开路由 `/f/{project_id}/{trigger_token}`（token 随机 128bit，不可猜即鉴权；创建时生成、可轮换——URL 含 token 会被代理/访问日志记录，轮换与审计提示写入运营文档）。挂载复用 payments 先例（serverhttp 与网关同 mux，`grpc_gateway.go:88-91`；LimitReader 模式参照 `/v1/payments/callbacks/{provider}`）；
- **平台只路由不验签**：请求以封套进 `TW_DATA`——`{method, path, raw_query, headers(白名单子集), body}`。**必须透传 query string**：微信 SSV 的 signature/timestamp/nonce/encrypt 在 query 而非 body，只透传 body 会丢验签参数。签名校验归函数代码（sha256 握手 + AES 解密，密钥放 function_variables secret；复用 D7「验签需原始报文」理念）；
- **双响应模式**（关键：微信 SSV 回调超时仅 **1s**×重试 3 次，而同步执行含容器冷启动秒级耗时，纯同步模式大概率全部超时致事件丢失）：`sync` 同步执行（≤30s）响应透传，适合非时限场景；`async_ack`（SSV 场景默认）平台立即 200 + 可配置静态 ack body（如 `{"is_valid":true}`），函数走既有异步队列、按 transaction_id 去重。诚实声明：async_ack 的 `is_valid` 在处理前返回，语义弱化为「已受理」；配合官方「前端先发奖 + SSV 兜底」策略与函数幂等，可接受（执行器 v2 热路径下 sync 有机会进 1s 窗，见 §6；async_ack 的定位从「补偿冷启动缺陷」回归为「架构上更稳的异步解耦」，默认地位不变）；
- GET 握手回显（`handshake: echo`）：平台对 GET 直接回 `{"echostr": <query.echostr>}`，不 invoke（echostr 回显不授予任何能力，URL 已有 128bit token 门禁，平台侧跳过 sha256 校验的残余风险可忽略）；
- 请求体上限 64KB、响应透传沿用输出截断预算；该路由**不经 gRPC 拦截器链**（无 AuditInterceptor）——`function_executions` 记录即审计载体（含来源 IP 摘要字段，随本阶段补列）；
- **async_ack 顺序红线（三轮复核 P1）**：先 Enqueue 成功、后写 200；入队失败一律 5xx 让微信重试——先 200 后入队的抖动窗口 = 事件永久丢失且平台无痕迹（一行执行记录都不存在）；
- headers 白名单需定义而非「白名单子集」一句话（三轮复核 P1）：放行全部 `x-*` + `content-type` + 枚举常见验签头（`x-hub-signature-256`、`wechatpay-*` 等）——白名单缺了验签头，「验签归函数」的前提就塌了；ack_body 上限 1KB（防配置成超大响应对公开端点做带宽放大）；body 上限 per-trigger 可配至 1MB（64KB 对狗粮 SSV 够，对 GitHub 类 webhook 不够）；
- per-IP 限频取独立更高默认（3000/min，三轮复核 P1：微信回调出口 IP 段集中，沿用全局 300/min 在发奖风暴下会 429 → 重试耗尽 → 事件丢失；匿名限频的 IP 兜底参数与此统一，防共享 IP 用户误伤）+ `result=quota` 指标告警；执行记录 `trigger_source = http:{trigger_id}`。

**cron 触发器**：

- worker 增加每分钟调度循环：扫描 `enabled AND type='cron' AND next_run_at <= now()`，**先 CAS 后入队**（`UPDATE ... WHERE id=$1 AND next_run_at=$old` 判 rows=1，失败即跳过——先入队后 CAS 在多实例下会双入队；执行行 queued→building 的 CAS 只防同一 execution 重复消费，防不了两条不同 execution）；
- 5 字段 UTC 表达式；错过策略可配 `misfire: skip | catch_up_once`，**默认 catch_up_once**——纯跳过对「每日重置/赛季结算」是危险的（那一分钟宕机 = 当天不重置）；恢复后按计划时刻补跑一次，`next_run_at` 直接推进到 now 之后的下一计划时刻（宕机 1000 个周期也只补 1 次，风暴由异步通道 + 信号量兜底），函数幂等兜底重复；
- 执行记录 `trigger_source = cron:{trigger_id}`。

### 4. 客户端调用面（P2）

`functions` 表策略列：`client_callable BOOLEAN NOT NULL DEFAULT FALSE`、`client_anonymous_allowed BOOLEAN NOT NULL DEFAULT FALSE`、`client_per_user_limit INTEGER NOT NULL DEFAULT 0` + `client_limit_window TEXT NOT NULL DEFAULT 'day'`（取值 `minute | hour | day`——**通用可配窗口**，广告场景的「每日 N 次」只是 day 窗口的一种配置而非固定语义；client_callable=true 要求 limit ≥1，窗口值域校验入 app 层）。存量函数全 FALSE ⇒ fail-closed。

`proto/client/v1/functions.proto`（新文件，登记 `authzFileDescriptors`）：

```proto
service FunctionsService {
  option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_END_USER };
  rpc InvokeFunction(InvokeFunctionRequest) returns (InvokeFunctionResponse) {
    option (google.api.http) = { post: "/v1/functions/{function_id}:invoke", body: "*" };
  }
}
message InvokeFunctionRequest {
  string function_id = 1 [(buf.validate.field).required = true];
  string data = 2 [(buf.validate.field).string.max_len = 32768]; // JSON object，沿用 32KB
  optional string deployment_id = 3;
  // 可选客户端幂等键：网络超时重试防重复执行。(project, function, user, key)
  // 唯一去重，命中返回既有 execution 原样（running 返回 running）。
  string idempotency_key = 4 [(buf.validate.field).string.max_len = 128];
}
message InvokeFunctionResponse {
  string execution_id = 1;
  string status = 2;            // completed | failed
  string response = 3;          // 函数 stdout 末行 JSON
}
// 配额超额走 ResourceExhausted 错误（ErrorInfo.Reason + RetryInfo），不是响应字段；
// 容器 exit 的 status_code 对客户端无语义，不暴露。
```

- 同步执行复用既有 ≤30s 路径；`function_executions` 加列 `invoking_user_id TEXT` 与 `client_idempotency_key TEXT`（UNIQUE 部分索引 `(project_id, function_id, invoking_user_id, client_idempotency_key)` WHERE NOT NULL）。**幂等是 P2 的正确性关键**：广告暂发类「调用即发资产」的函数对重试敏感——平台级去重（建执行前查键）+ 函数侧幂等（以 execution_id 派生资产幂等键）双保险；重复键在执行进行中返回 409/既有记录，完成则原样返回；
- 鉴权：服务级 `ACCESS_END_USER` 继承，permissions 归一 `["users"]`；use-case 层 `RequireEndUser`（或匿名策略下放宽）二次断言——**这不是 `CreateExecution` 的放开**，而是带独立策略门的新入口，Server 面 API 原样不动；
- SDK：Go/TS client 包补方法 + TS contract.test 登记 + `task gen:authz-matrix`。

### 5. 调用配额与滥用防线（P2）

- **每用户限频（可配窗口，通用原语）**：`client_per_user_limit` + `client_limit_window`（minute | hour | day）。Redis 固定窗口计数 `torchwood:fnq:{project}:{function}:{user}:{window_bucket}`（bucket 按窗口粒度派生，day = UTC 日期），超限 `ResourceExhausted` + `ErrorInfo.Reason = "FUNCTIONS.INVOKE_QUOTA_EXCEEDED"` + RetryInfo（窗口结束时刻）。**含义**：限频语义收归平台强制，函数代码不担额度正确性——这吸收了原方案 A 通道配额的角色，但保持水平原语形态（任意窗口而非资产域专用）。**故障降级**：通用限流的熔断 fail-open 是既有产品决策（J5-1），但承载经济语义的限频保守处理——Redis 不可用时改查 `function_executions` 窗口内计数（`(project_id, function_id, invoking_user_id, created_at)` 索引支撑），DB 亦不可用则拒绝（fail-closed），与 A 稿配额哲学对齐；
- **每用户并发闸门**：全局 run 信号量（16）之外加 per-user 并发上限（建议 2），防单用户挤占执行槽（竞品分析 §三：噪声邻居是开放客户端触发面的头号风险）；
- **执行记录保留（与限频降级的交互修正，二轮复审发现）**：client/trigger 来源的执行改为**时间窗保留（≥2× 该函数配置的最长限频窗口，day ⇒ 48h）**——上方限频的 DB 降级路径按窗口内 `function_executions` 计数，若用条数式保留（如 1000），高量窗口内计数行会被 prune 裁掉导致**少计超发**；server 来源维持 100 条。并加 Prometheus 计数器 `torchwood_functions_invoke_total{project, function, source, result}`；
- 触发器管理（CRUD trigger、轮换 token）走 Server 面新 RPC（`functions.write` scope + admin 角色），纳入既有审计。

### 6. 执行器 v2：常驻 runner 为默认执行模型（P0.5，owner 裁决 2026-09-09）

**缺陷确认**：「每请求一个容器进程」（env 进 → 跑 → stdout 出 → 删除）是 CGI 形态——历史裁决过一次（CGI → FastCGI），当代 FaaS 无一例外是常驻实例按请求复用（Lambda execution environment、Cloud Run 实例、Supabase Edge isolate）。它在旧画像（server key、低频、异步容忍）下是无害 MVP；作为 P1/P2 要打开的触发器与客户端调用面的地基，则是设计缺陷：

- **每请求重复支付 runtime 启动 + 模块加载**（node 数百 ms 起、随依赖增长）——延迟与成本双重税，且高频下 docker daemon 的 create/stop API 本身成为吞吐瓶颈；
- **使整类场景不可达而非仅变慢**：SSV 1s 回调窗、客户端同步调用的 UX——平台被迫用产品层补丁（双响应模式）补偿基座缺陷，「产品绕基座」是坏味道；
- **连接无法复用**：每请求重建对下游（含回访平台 API）的 TCP/TLS/连接池。

（另一方向上的误区同步排除：「定时保温 ping」无用——跑完即删模型下 ping 不产生热量，只产生计费。修复只能换协议，不是加开关。）

**模型（resident-first，单一代码路径）**：固定 Dockerfile 的 CMD 换为平台 runner（现行模板无 ENTRYPOINT、用户入口在 CMD，`docker.go:503,509`；node/python 各一份，模板资产——「构建期不执行用户代码」不变量保持）。runner 启动即加载用户模块，容器内监听 HTTP（仅 per-project 桥网络可达，无外部暴露），启动握手 + 心跳做健康检查。分发：dispatcher（server 同步路径 / worker 共用）经桥网络 POST 容器 `IP:port`——body = TW_DATA，header 带按请求铸造的 `TW_EXECUTION_TOKEN`（**常驻的是容器不是凭证**，TTL/最小特权语义与身份模型不变）；响应经 HTTP 回传，stdout 退化为纯日志。**冷启动不是独立路径，是池从 0→1 的扩容**：无 ready 实例时 spawn（该请求支付冷启动成本），此后按 idle TTL 保留。对函数代码透明：仍是 `main(TW_DATA)` 进、JSON 出。

**分发通路（三轮复核 P0：阻断性设计决策，由原 Q9 升格）**：标准部署下 server 自身就是容器（`docker/dokploy/docker-compose.yml`），不在 `tw-func-<project>` bridge 上，跨 bridge 无路由——「经桥网络 POST 容器 IP」默认**物理不可达**。这不是优化项，是 P0.5 的硬前置，实施前三选一拍板并写明部署变更：①executor 创建项目网络时把自身容器 attach 进去（实现最顺；注意 docker 网络数量上限与 join 失败重试）；②runner 监听端口发布到宿主 `127.0.0.1` 动态端口，注册表存 host:port（跨平台最稳；注意端口段管理与宿主暴露面）；③独立 functions-dispatcher 进程（Q13）专职挂网络（顺带解决 docker.sock 不进 API 面容器的问题）。任一方案的验收清单都须含「dokploy compose 标准拓扑端到端打通」。**已拍板（2026-09-09 owner）：方案③独立 functions-dispatcher**——compose 新增 functions-dispatcher 服务，专职持有 docker.sock、join 各项目网络、承接全部执行分发（server 同步路径与 worker 异步路径都经它的 HTTP 接口）；server/worker 零 daemon 依赖，host-root 等价凭证收敛到非 API 面进程，Q13 联动收口；多一跳 ≈1ms 在 25ms 预算内；无状态、初期单副本，需要 HA 时双副本 + Redis 注册表仲裁。

**池策略（平台默认 + per-function 覆盖）**：`min_instances`（默认 0 = 纯 scale-from-zero；可设 1–2 保温）、`idle_ttl_seconds`（默认 300）、`max_requests_per_instance`（默认 1000，防函数内存泄漏的定期回收，同 Lambda 实例回收语义）+ 平台级常驻总量上限（每 daemon N 个，防单项目耗尽宿主内存）。**边界补全（三轮复核 P1）**：`max_instances`（默认 2）——突发并发达上限后**有界排队**（深度上限 + 队首超时 → `ResourceExhausted`，同步调用方不得无界等在 30s ctx 上）；drain 语义——部署更新旧池排空上限 ≤ 函数超时，到点强杀在途请求以 504 类错误收场（幂等键的适用场景，文档明示）；判活规则——dispatch 时租约续期 + 注册表 busy 标记，长请求期间区分 busy 与 dead，reaper 不得误杀执行中实例；daemon 重启 = 热实例全灭——spawn 收敛为每函数同时一个（其余请求等注册表），防全量冷启动风暴。常驻实例**不占全局 run 信号量**（16 槽是 per-execution 预算，被常驻占用会饿死并发执行），独立核算。

**生命周期与故障语义**：空闲回收 / 请求数达限排空重建 / 部署更新 = 旧池 drain（借鉴 server drain 模式）/ 心跳丢失与 daemon 重启后的幽灵实例清理（周期 docker inspect 对账）。**隔离粒度变粗（诚实声明）**：请求超时或 handler 崩溃 → 杀整个实例（ContainerStop SIGKILL，复用既有原语）；同实例跨请求共享进程状态（模块级全局变量残留——Lambda 同款语义，文档明示 + 回收策略兜底；仅同函数同租户，无跨租户面）。不变的部分：崩溃不波及 server 进程，容器隔离边界与全部 hardening 不变。

**迁移与兼容**：runner 属镜像模板层变更 → 模板版本化，存量 deployment 按新模板重建（构建是平台确定性操作）；按 runtime 灰度（node 先行）；过渡期保留 v1「每请求一容器」作为 spawn 失败时的降级路径，v2 稳定后移除。

**资源与计费**：请求级 duration_ms 照旧；新增实例存活计量 `function_resident_uptime_ms`（min_instances>0 的保温成本显式计费——常驻是花钱买延迟，账要透明）。

**SLA 与延迟预算（owner 2026-09-09：热路径 P99 ≤ 100ms）**：SLA 口径 = **热路径同步分发的简单函数端到端**（client invoke / HTTP 触发 sync 模式）；冷启动（池 0→1）与异步队列**显式出 SLA**——它们是架构上的不同档位。预算拆账（P99）：

| 环节 | 预算 |
|---|---|
| 网关 + 拦截器链（客户端路径，含 principal 缓存命中¹） | ≤5ms |
| 实例认领（注册表）+ execution token 铸造 | ≤2ms |
| 桥网络 HTTP 往返（同宿主） | ≤5ms |
| runner 内部分发 | ≤2ms |
| 执行记录落库（同步快路径单写，见下） | ≤10ms |
| **平台开销小计** | **≤25ms** |
| 简单函数逻辑（verify/dedup/grant 含一次回访平台） | 20–70ms |
| **端到端合计** | **≤100ms** |

三项由 SLA 逼出的设计约束（三轮复核后修正为预占模型 + 清账前置）：①**同步快路径 = 两写预占记账**（修正原「单条最终态 INSERT」方案——单写在并发同幂等键下双执行、崩溃零痕迹、限频降级「先执行后计数」有整段漏计窗口，同时破坏幂等/审计/限频三个不变量）：分发前 `INSERT (status=running, invoking_user_id, client_idempotency_key)`——唯一索引此刻生效，并发同键第二请求 INSERT 冲突即回 409/既有行，TOCTOU 消失；行即刻成为限频 DB 降级的计数依据（「先占位后执行」，对齐 Redis 路径的先 INCR 后执行）；执行中崩溃行留在 running，可被孤儿恢复扫到。结束后 `UPDATE` 终态。两写多付 1–3ms，由 ③ 的清账足额回补。异步路径状态机原样保留；孤儿恢复配套从「worker 启动一次」改为**周期扫描**并纳入快路径 running 态。②**保温与 SLA 的绑定走文档语义，不加新列**：需要 sub-100ms 的函数设 `min_instances ≥ 1`（并注意 idle_ttl 内连接保活——函数回访平台走宿主内明文 HTTP + keep-alive，闲置后重连只付 TCP ~1ms）。③**热路径 DB 往返清账（三轮复核 P0：原预算表对着拦截器链记账后不成立）**——现状客户端鉴权一次调用 **4 次 DB 往返**（session 校验 + users.GetByID + `LoadUserRoles` 内又一次 users.GetByID + memberships，`internal/infra/auth/validator.go`、`internal/app/client/user_roles.go`），AuditInterceptor 响应路径同步 INSERT，handler 前置 GetFunction + ListDeployments **全量拉取**筛 ready + GetVariables，Prune DELETE 也在返回前——不清账则平台开销 P99 落 30–60ms，SLA 必失守：principal 短 TTL 缓存（(project, session_id, iat) 键，进程内 LRU + 登出/封禁主动失效）或合并为一条 JOIN；修掉 LoadUserRoles 的重复 users.GetByID；client invoke 与 HTTP 触发的审计以 function_executions 行为载体（跳过同步审计写）；functions 表冗余 `latest_ready_deployment_id` 消灭全量拉取；function/variables 短 TTL 缓存；Prune 移出热路径（周期或抽样）。对照：v1 每请求 ≈ 0.1s 容器创建 + 0.2–1s+ runtime boot，结构性无法达标——这正是换模型的理由。对 SSV：热实例 + sync 稳进 1s 窗（预算的 1/10 量级）；`async_ack` 保持默认，定位是「异步解耦」而非「延迟妥协」。对 P3：差距收敛为「声明式 1–10ms vs 容器 ≤100ms」——仍差一个量级，但已进入游戏 UX 可用区；P3 判据不变，复盘报告应记录实测分布。

**与产品定位/P3 的关系**：执行器 v2 是水平基座（与触发来源正交，零产品语义）。它缓解 P3「高频轻量成本/延迟」触发条件，但**不消除**「声明式毫秒级（DB 事务 1–10ms）vs 容器亚秒级」的本质差距——P3 原语立项判据不变。与 Q9（内网直达）同根：分发走桥网络容器 IP，与「server attach 函数网络」是同一通路，宜一并评估。

## Security & Privacy Considerations

1. **不可信触发面（P2）**：args 零信任是函数代码责任（文档明示，对标 PlayFab「zero trust」）；平台强制的是配额、并发、payload 上限与审计。配额判定在 Redis fail-open 之上叠加 DB 侧执行记录兜底对账（用量可查、可封禁）。
2. **执行凭证**：最小特权（declared_scopes 默认空）+ 短 TTL + 脱敏兜底；跨项目不可达（token 绑定 project）。
3. **公开 HTTP 端点**：不可猜 token 即鉴权 + per-IP 限频 + 裸 body 透传不解析（无注入面）；平台不做验签意味着不承担验签缺陷，但也提示运营文档必须要求启用函数侧验签。
4. **egress**：**已拍板（2026-09-09）**——不可信函数（client_callable / HTTP 触发）默认 deny 出网 + per-function 显式白名单（目标域名/网段），可信（server key）函数保持放开；实施为 P2 切片（iptables/网络策略作用于相关容器网络）。常驻模型下长驻容器持久出网连接的暴露时长问题随默认 deny 一并收敛。
5. **匿名会话洗限频（二轮复审补充）**：匿名会话可无限新造，per-user 限频对匿名用户等价于按次绕过。对策：`client_anonymous_allowed` 文档明示「匿名函数不应承载经济语义」；限频对匿名维度叠加按 IP 计数兜底（复用限流 IP 维度）；狗粮游戏用微信登录（unionid 稳定 ID）不受影响；
6. **隐私**：客户端触发的 data/stdout 含用户可控内容，执行记录仅 admin 可读（现状不变）；文档提示不要在 data 传敏感个人信息。
7. **常驻执行模型（v2，§6）**：同实例跨请求共享进程状态（模块级全局残留）——仅同函数同租户，无跨租户暴露面；请求超时/handler 崩溃的隔离粒度为实例级（杀容器，粗于 v1 的请求级——业界 FaaS 同款取舍）；长驻容器可持有持久出网连接（egress 暴露时长上升，并入 Q6 评估）；max_requests/崩溃重建回收是状态污染兜底。

## Observability

`torchwood_functions_invoke_total{project, function, source=server|client|http|cron, result=ok|quota|error|timeout}`、执行时长/排队时长直方图（补齐现状无 functions 专属指标的缺口）；配额水位可从 Redis 计数旁路导出；执行器 v2 另有：冷启动（池 0→1）计数 + 初始化时长直方图（对标 Lambda initializationDuration）、池水位（ready/booting/draining）、`function_resident_uptime_ms`（计费口径）、**热路径 SLO burn 告警（端到端 P99 > 100ms 或平台开销 > 25ms）**。计费口径透明化（三轮复核 P2）：execution principal 回访平台照常计入 API 调用计量（一次执行 = 1 invoke + N 回访）；HTTP/cron 触发的执行不经拦截器、不计 API 调用——两条口径写入计费文档。

## Rollout Plan / PR 切片

| 阶段 | 内容 | 狗粮验收锚点（自有小游戏） |
|------|------|--------------------------|
| P0 执行身份 | 迁移 000013（functions.declared_scopes + function_variables.kind）+ `functions.execution.api_base_url` 配置 + token 铸造/校验 + 限流 execution 维度 + requireAssetWrite 断言集扩展 + operator/审计贯通 + Console 函数表单 scope 编辑 + 08-functions.md 更新 | 游戏任一函数迁移到注入身份（TW_API_BASE_URL + TW_EXECUTION_TOKEN），删除 variables 中的长期 API key；账本 operator 可见 function+user；连续调用不触发 per-IP 限流 |
| P0.5 执行器 v2 | 迁移 000014（池策略列：min_instances/max_instances/idle_ttl/max_requests）+ **functions-dispatcher 服务（compose 新组件：docker.sock 收敛、join 项目网络、承接全部分发）** + runner 模板（node 先行灰度，容器内 HTTP）+ reaper/Redis 实例注册表 + 同步快路径**两写预占**记账 + 热路径 DB 清账（principal 缓存、latest_ready_deployment_id、审计载体化、Prune 移出）+ 模板版本化重建 + 幽灵实例周期对账 + 存活计量 + Console 池策略 | 狗粮既有函数在 v2 上回归（结果一致、stdout 日志化）；**简单函数热路径同步端到端 P99 ≤ 100ms（平台开销 ≤ 25ms，SLA 见 §6）**；dokploy compose 标准拓扑端到端打通；daemon create/stop 调用量下降可观测 |
| P1 触发器 | 迁移 000015（function_triggers）+ HTTP 触发器（路由/封套透传/双响应模式/限频/来源 IP 摘要列）+ cron 调度循环（misfire 补跑）+ 触发器管理 RPC + Console 触发器管理页 + 指标 | **微信 SSV 回调接入**（async_ack 模式）：验签 → 按 transaction_id 去重 → grant 复活券；每日/赛季重置（cron，验证 catch_up_once）；热/冷路径时延分布实测（验证 v2 收益） |
| P2 客户端面 | 迁移 000016（client 策略列 + invoking_user_id + client_idempotency_key）+ client proto/handler/gateway（同步默认）+ 每用户限频（可配窗口，含 DB 降级）+ 并发闸门 + **egress 默认 deny（client_callable/HTTP 触发函数）+ per-function 白名单** + SDK + authz-matrix + `task generate:proto`（新 proto 进 swagger 反向覆盖断言）+ Console 策略开关 + 保留策略/指标 | 签到、成绩提交、广告奖励 isEnded 即时暂发（SSV 对账兜底）全部走客户端调用；重试不重复发资产（幂等键）；每用户限频由平台强制；**isEnded 暂发热路径 P99 ≤ 100ms 达标（SLA 见 §6）** |
| P3 复盘组合方案 | 收集组合实现（HTTP 触发器 + client invoke + 限频 + cron + 资产）在狗粮游戏的运行数据（延迟/成本/滥用面）；仅当数据证明组合不足时，立项「声明式限频动作」水平原语（`client-self-consume-assets.md` 的资产域设计降为该原语的应用层参考） | 高频轻量操作（每次广告/签到）的 P99 延迟与执行成本画像（含执行器 v1/v2 前后对照） |

每阶段沿用派发稿红线自查；P2 启动前过一次安全评审（egress 评估 + 不可信触发面清单）。

**组合实现示例（产品方法论落地）**：广告复活奖励这一具体需求**不新增任何专用端点**，全部由水平原语组合——①奖励建模 = asset def（既有）；②SSV 验证发奖 = HTTP 触发器（async_ack）+ 函数验签去重 + execution principal（assets:write）grant（P1）；③isEnded 即时暂发/签到 = client invoke + per-user 限频（day 窗口）+ 函数幂等 grant（P2）；④每日重置/赛季结算 = cron 触发器（P1）；⑤对账审计 = ledger + executions 记录（既有）。「垂直需求 → 基础能力组合」的映射本身就是本稿的狗粮验收方式；任何新的垂直需求到达时，先问「缺哪个水平原语」，而不是「加哪个专用功能」。

## Key Decisions（建议，待 owner 拍板）

| # | 决策 | 理由 |
|---|------|------|
| K1 | 分阶段 P0→P1→P2→P3，狗粮应用按阶段验收 | P0/P1 是全体函数用户的欠账修复；P2 才打开不可信面 |
| K2 | 执行身份 = 短期不透明 token + per-function 声明 scope（默认空，fail-closed） | 行业入场券的最小实现；消灭长期 key 落地执行环境 |
| K3 | 触发器一期 HTTP + cron，事件触发后置 | serverless 化验证圈定的必要范围；outbox 订阅独立议题 |
| K4 | HTTP 触发器平台只路由不验签（封套透传含 query string，验签归函数；GET 握手 echo 回显除外） | 复用 D7 理念；平台不承担各平台验签语义（微信/AdMob/ironSource 各异）；echo 回显不授予任何能力 |
| K5 | 客户端调用面 = 新入口 + per-function 策略列（client_callable/anonymous/可配窗口的 per-user 限频） | 不放开 CreateExecution；限频是通用原语（「每日 N 次」只是 day 窗口的一种配置），语义收归平台强制 |
| K6 | 方案 A（资产域专用端点）**不再独立立项**——广告奖励等垂直场景由本稿原语组合实现；仅当组合方案被运行数据证明不足（延迟/成本）时，将「声明式限频动作」作为**水平原语**重新立项 | 产品定位：通用 BaaS，垂直需求走组合；A 稿保留为该原语的应用层参考设计 |
| K7 | 本稿全部交付物均为水平基础能力；微信 SSV/广告奖励只作为狗粮验收场景，不进产品语义 | 通用型 BaaS：增强基础能力，组合实现具体需求 |
| K8 | 执行器 v2：常驻 runner 为**默认执行模型**（冷启动 = 池 0→1 扩容，单一代码路径）；一期每实例串行，池大小 = 并发；常驻不占 run 信号量，独立总量上限 + 存活计量 | 「每请求一容器」（CGI 形态）对 P1/P2 的延迟/成本/吞吐画像构成上限；保温 ping 无用，必须换协议而非加开关；先修地基再开新面（P0.5） |
| K9 | SLA = 热路径同步分发，简单函数端到端 P99 ≤ 100ms（平台开销 ≤ 25ms）；冷启动/异步显式出 SLA；同步快路径两写预占记账（预占 INSERT + 终态 UPDATE，落库后才返回） | owner 验收线；逼出快路径记账、保温绑定、热路径 DB 清账三项约束；防止被读成「每次调用 <100ms」的过度承诺 |
| K10 | Owner 拍板（2026-09-09）：独立 dispatcher 分发通路 / Redis 不透明 token / 客户端调用同步默认 / 不可信函数 egress deny + 白名单；六项默认锁定（cron UTC、匿名后置、容器内 HTTP、/f/ 路由、常驻上限 8/daemon、串行实例） | 详见 banner 与 Open Questions 收口记录 |

## Open Questions（需拍板）

1. ~~执行 token 形态~~ **已拍板（2026-09-09）**：Redis 不透明 token——主动 DEL 吊销与三轮复核语义吻合，无密钥管理负担；Redis 故障 = fail-closed（彼时信号量/执行队列同样依赖 Redis，函数链路整体已不可用，无额外可用性损失）。
2. ~~HTTP 触发器 URL 命名空间~~ **已拍板**：`/f/{project}/{token}`（gateway 同 mux 挂载已核实，payments 先例在）。
3. ~~cron 时区~~ **已拍板**：一期 UTC，触发器级可配后置（竞品先例一致）。
4. ~~匿名调用~~ **已拍板**：一期不开（`client_anonymous_allowed` 字段保留）；狗粮走微信登录，匿名 IP 兜底限频做好后再开。
5. ~~客户端调用同步默认~~ **已拍板**：同步默认（≤30s 沿用），长任务走 Server 面 CreateExecution(async=true)。
6. ~~egress 管控~~ **已拍板**：不可信函数（client_callable / HTTP 触发）默认 deny + per-function 显式白名单（目标域名/网段）；可信（server key）函数保持放开；实施进 P2 切片。
7. **执行记录保留分级**：~~已由二轮复审裁决~~——client/trigger 来源必须 time-based（48h），否则配额的 DB 降级路径在高量日内被条数 prune 破坏（§5 交互修正）；server 来源维持 100 条。
8. **微信小游戏虚拟支付（米大师）**：不属于本稿，但同属狗粮路径的支付缺口，需独立条目核实排期。
9. ~~内网直达优化~~ **三轮复核升格为 P0.5 阻断性设计决策**（§6「分发通路」三选一：self-attach / 宿主端口发布 / 独立 dispatcher，实施前拍板）；函数→server 方向的 NAT 外部端点（`functions.execution.api_base_url`）仍是 P0 的默认解。
10. ~~runner 分发协议~~ **已拍板**：容器内 HTTP（dispatcher → 容器 IP:port）。
11. ~~常驻总量与计费~~ **已拍板**：每 daemon 上限 8 个起步（超限排队/拒绝语义已在 §6）；计费档位实施时随计费体系定。
12. ~~多路复用~~ **已拍板**：维持一期串行（池大小 = 并发）；放开需可重入契约，后置。
13. ~~池管理拓扑~~ **已拍板（与分发通路联动）**：独立 functions-dispatcher 进程统一路由执行（§6「分发通路」）。

## Alternatives Considered

- **方案 A（client-self-consume-assets.md）**：声明式 self-consume，PlayFab Rewarded Ads 同构。**不再独立立项**（通用 BaaS 定位：垂直需求走组合实现）——若组合方案被运行数据证明不足，「声明式限频动作」作为水平原语重新立项时，A 稿降为其应用层参考。
- **函数内自管配额（不做平台调用配额）**：把「每日 N 次」写进游戏函数代码。落选：语义责任转移给每个函数作者，与「平台强制限额」的竞品基线（UGS 600/min/player、PlayFab per-entity throttling）相悖。
- **in-process 轻量执行模式**（高频轻量函数不走 Docker）：延迟/成本的最激进方案，但引入进程内隔离议题，工程与安全面另立设计。执行器 v2（§6）落地后，它与容器路线的差距缩小为「进程内 vs 容器边界」；P3 复盘按 v2 实测数据决定是否还需要更激进的原语。

## References

- `docs/design/economy-client-write-competitive-analysis.md`（§三 行业函数模型对照；§八 serverless 化完整性验证）
- `docs/design/client-self-consume-assets.md`（方案 A，后置至本稿 P3）
- `docs/review/saas-baas-design-2026/04-platform-capabilities.md`（PC-5 触发器缝、PC-6 密钥缝）
- `docs/implementation-functions-executor.md`（执行引擎现状）
- 代码锚点：`internal/domain/functions/executor.go`（Executor 端口）、`internal/app/functions/executions.go`（CreateExecution/buildExecution/信号量）、`internal/app/functions/semaphores.go`、`internal/infra/functions/docker.go`（容器隔离与限额）、`internal/api/interceptor/jwt.go`（凭证族扩展点）、`internal/app/shared/authz.go`（RequireServerPrincipal）
