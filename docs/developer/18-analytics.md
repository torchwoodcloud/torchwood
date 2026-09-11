# 18 Analytics：内置事件分析

> 面向后端开发者与端侧接入者：双面事件摄入、月分区只写通道、重算式预聚合与固定形状查询，以及 TS SDK 批量缓冲器与各端接入配方。
> 源码锚点：`internal/domain/analytics/`、`internal/app/analytics/`、`internal/infra/bun/bunrepo/analytics_*`、`worker/analytics_*.go`、`sdk/typescript/src/client/analytics.ts`、`console/src/routes/analytics/`。
> 设计稿：`docs/design/analytics.md`（D1–D15 决策与三路交叉验证记录）；执行计划：`docs/design/analytics-execution-plan.md`。

## 0 子系统定义与边界

**Analytics 子系统 = 以事件（Events）为核心的行为分析整体方案**：双面摄入（client 面 principal 归因 + server 面 API Key 可信代报）→ 项目 schema 内**只写月分区原始表** → worker **每小时幂等重算**的三张预聚合表（daily / user_days / first_seen）→ **固定形状查询 RPC**（响应带 `source: rollup|raw` 口径标注）→ Console 分析区 / SDK / Agent 查询消费。

事件通道**独立于文档层**（D1）：静态系统表落位项目数据面 schema、不进 outbox、不发 realtime、不触发函数、无 RLS/`_acl`（行为数据按项目整体授权，不做文档级权限）。系统静态表（users/sessions/…）是它的边界邻居：注销钩子在删除用例事务内写入 tombstone，是两子系统的唯一交点。

### 模块地图（范围内）

| 层 | 模块 | 职责 |
|---|---|---|
| 领域 | `internal/domain/analytics/`（`limits.go`/`event.go`/`query.go`/`repository.go`/`worker.go`） | 平台级上限常量**单一来源**（批 100 / 名 64 / 键 25 / 16KiB / prop 值截断 256 / 字典软上限 1000 / 钳制窗 `[now-24h, now+5min]`）、事件与查询结果模型、摄入/查询/worker 三组端口、`NormalizeRetentionDays` |
| 应用 | `internal/app/analytics/`（`ingest.go`/`query.go`/`rollup.go`/`maintenance.go`） | 逐事件校验（形状/钳制/标量化截断/16KiB）、归因落定（client=Principal、server=可信代报）、字典 upsert + 软上限、`accepted/skipped` 部分接收、计量 `Incr`、查询护栏与择路回退、幂等重算、分区治理与 tombstone 清洗编排 |
| 传输 | `internal/api/clientgrpc/analytics.go` + `internal/api/servergrpc/analytics.go`（proto：`proto/client/v1/analytics.proto`、`proto/server/v1/analytics.proto`） | 双面 handler：client 绑定 Principal 归因与 `source=client`；server 走 scope（`analytics:write` 摄入 / `analytics:read` 查询）；请求形状校验由 protovalidate 注解声明、`ValidateInterceptor` 统一求值 |
| 适配器 | `internal/infra/bun/bunrepo/analytics_ingest_repo.go`（多行单语句 INSERT + 字典 upsert）、`analytics_query_repo.go`（护栏内查询 + 择路）、`analytics_worker_repo.go`（重算/分区/清洗 SQL）；模型 `internal/infra/bun/model/analytics.go`；迁移 `internal/infra/projectschema/migrations/000019_analytics.up.sql` | 项目 schema 内 `analytics_*` 六表全部 SQL；全参数化；SQL 形状断言进 `*_sqlshape_test.go` 护栏家族 |
| worker | `worker/analytics_rollup.go`（每小时）+ `worker/analytics_maintenance.go`（分区每日 / 清洗每 6h） | 周期与日志壳，业务在 app 层（`RunWorkerOnce` 模式；单项目失败仅记日志，`projects.ListProjects` 遍历） |
| 注销钩子 | `internal/app/client/account.go`（`DeleteAccount`）+ `internal/app/server/users.go`（`DeleteUser`） | 既有删除事务内 tombstone INSERT（`ON CONFLICT DO NOTHING` 幂等） |
| 计量/限流 | `internal/domain/billing/billing.go`（`MetricAnalyticsEvents` 进 `KnownMetric`）+ 限流拦截器（复用 user 维度） | accepted 条数 `UsageCounter.Incr`（best-effort `WithoutCancel`）；自动进 usage_rollups → 账单 |
| Console | `console/src/api/analytics.ts` + `console/src/routes/analytics/`（`pages.tsx`/`EventDetailPage.tsx`/`UserActivityPage.tsx`/`components.tsx`/`shared.ts`） | 概览（KPI+趋势+Top）、事件字典+详情（趋势+拆解）、留存矩阵、用户行为轨迹；`SourceBadge`/`SourceNote` 口径标注 |
| SDK | `sdk/typescript/src/client/analytics.ts`（`ClientAnalyticsService` + `AnalyticsEventBuffer` 批量缓冲器）、`sdk/typescript/src/server/analytics.ts`；`sdk/go/server/analytics.go` | 端侧摄入薄封装 + size/time 双阈值缓冲器；server 查询面薄封装；CLI 零登记（`sdk/go/server` 反射覆盖测试自动纳入新 RPC） |

### 范围外

服务端 sessionization（会话边界由各端 SDK 定义，平台只收 `session_id`）；漏斗（有序事件序列——关卡进度类可降解为 breakdown）；维度值预聚合表（D9 演进路径，触发条件：长窗拆解性能投诉）；实时推送；OLAP 适配器实现（只留端口接缝）；事件回填导入与事件名合并治理（设计 §13 开放问题）；转发 destination（事件外发 Plausible/PostHog 等——将来可选小特性，与内置分析共存）。

### 关键不变量（变更评审锚点）

1. **client 面红线**：请求无 `user_id` 字段、无任何查询方法；归因唯一来源是 Principal（含匿名会话），请求体无法伪造（D3）。`source` 列（`client|server`）由双面 handler 各自绑定，同样不进请求体。
2. **通道红线（D1）**：摄入/查询/rollup/清洗全链路不进 outbox、不发 realtime、不触发函数；无 RLS、无 `_acl`、无 `_version`。
3. **不去重（D14）**：摄入幂等 = 接受重复（at-least-once）；"顺手加幂等键"打回（`client_event_id` 去重列为演进，未实现）。
4. **上限常量单源**：`internal/domain/analytics/limits.go` ↔ protovalidate 注解逐字对齐（名/键正则、批 100、session_id 255）；键数 25、单事件 16KiB、钳制窗、prop 值截断 256 等**复合校验在 app 用例层**（protovalidate 覆盖不了复合规则）。修改任一值必须同步 proto 与本文档。
5. **部分接收（D12）**：批内逐事件校验，坏事件（坏形状/时间越界/超软上限的新名）计入 `skipped`、好事件照收，不整批拒绝；事件名软上限在**摄取期**执行——第 1001 个新事件名被 skip，存量名不受影响。
6. **审计豁免显式登记（D13）**：server 面 `IngestEvents` 登记进 `internal/api/interceptor/audit.go` 静默清单（`audit_test.go` 护栏）；client 面天然豁免（`auditRowEligible` 只审 AccountService 非读动作）。
7. **查询全参数化 + 白名单**：事件名/`prop_key`/timezone 服务端校验后参数化（沿 `aggregateDocuments` 纪律），无字符串拼接值；查询事务内 `SET LOCAL statement_timeout = '15s'`（慢查询显式报错不挂死）。护栏：HOUR 窗 ≤7 天、DAY 窗 ≤366 天、Breakdown ≤30 天、Retention cohort 窗 ≤92 天、ListUserEvents 窗 ≤92 天、Top-N ≤50。
8. **聚合可重建（D7）**：daily/user_days/first_seen 全部可从 raw 幂等重算（容灾重建路径）；rollup = 覆盖式 `ON CONFLICT DO UPDATE`，同窗口重跑不翻倍。
9. **分区治理先于写入（D5）**：worker 每日预建未来 2 月分区 + DEFAULT 非空搬运（事务内 INSERT..SELECT 后清空）；保留期裁剪 = DROP 整月分区（分区名形状 `analytics_events_YYYY_MM` 强校验，不足整月部分留待下月）。
10. **worker 表边界**：rollup/清洗只读写项目 schema 内 `analytics_*` 静态表；不触碰其他表。
11. **合规清洗（D10）**：注销事务内插 tombstone → worker 异步分批**硬删**身份关联三表（raw / user_days / first_seen，`(user_id, occurred_at)` 索引点删，批 ≤5000）→ `done_at`（三表清完才标记）；聚合计数不回退（口径声明见 §2）；重删幂等。
12. **计量口径**：`MetricAnalyticsEvents` 按 **accepted 条数**计量（skipped 不计费）；`IngestEvents` 非读动词但不落 audit_logs（不变量 6），计量与审计互不影响。
13. **bun 写规范**：摄入/rollup 写路径显式列白名单（AGENTS.md 更新写规范；SQL 形状护栏测试锁形状）。

## 1 事件模型与上限

事件 = `name + occurred_at? + props? + session_id?`（server 面多一个可信代报 `user_id`）。`occurred_at` 缺省 = 服务端 now（批级统一时钟）；app 层钳制 `[now-24h, now+5min]`，越界计入 `skipped`（D4：客户端时间为主语义，离线补传事件算对日，防伪由钳制窗承担；`ingested_at` 双列留档）。

| 平台级强制上限 | 值 | 执行点 |
|----|----|----|
| 单请求事件数 | 100 | protovalidate |
| 事件名长度 / 形状 | 64 / `^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$` | protovalidate |
| props 键数 / 键形状 | 25 / `^[a-zA-Z_][a-zA-Z0-9_.]{0,63}$` | 键形状 protovalidate；键数 app 层 |
| 单事件序列化体积 | 16 KiB | app 层 |
| prop 字符串值长度 | 256 字符（超长**截断**入库，不拒收） | app 层 |
| 事件名项目级软上限 | 1000（存量名不受影响，仅拒新名；并发轻微超扣可容忍） | app 层（摄取期字典 upsert 前置检查） |
| session_id / user_id 长度 | 255 字符 | protovalidate |

请求/响应：`IngestEventsRequest { events[1..100] }` → `IngestEventsResponse { accepted, skipped }`——**部分接收语义**（移动端重试批中混有脏数据时最大化接收）。存储侧预留通用上下文维度列 `platform` / `app_version`（如 `mp`/`web`/`ios` + 版本号）：当前双面 proto 均未开放写入字段（列已就位、演进预留），下钻响应 `ListUserEvents` 可见。

props 最小化指引：只放低基数维度键（场景/关卡/渠道），不放昵称/手机号/openid 等 PII（PII 硬防护 blocklist 为设计 §13 开放问题，当前为文档指引）。

## 2 口径声明（消费方必读，设计 §11 落地）

| 口径 | 声明 |
|----|----|
| **at-least-once（D14）** | 平台**不做摄入去重**。端侧重试、页面隐藏补发、持久队列重放都会造成同一事件重复上报——DISTINCT 类指标（UV/新增/留存）天然免疫；计数类接受 ±1% 量级偏差。TS SDK 缓冲器的重试语义（§6）即建立在此声明上。`client_event_id` partial unique 去重列为演进路径（当前未实现）。 |
| **UV 注销后近似（D10）** | 注销用户的 raw / user_days / first_seen 行被异步硬删，但 `analytics_daily` 聚合计数**不回退**——注销时点之前的 unique_users / 留存历史为**近似值**（含已注销用户），Console 以口径提示标注。 |
| **UTC 切日** | DAY 粒度桶、留存 cohort、`first_day/last_day` 全部按 **UTC 零点**切日；HOUR 桶为 UTC 整点。Console 时间窗同样按 UTC 取整对齐。时区化日粒度是演进项（需小时聚合或重桶）。 |
| **raw 保留期** | 原始事件与 user_days 按保留期裁剪（DROP 整月分区 + user_days 批量修剪）：**默认 90 天**，config `analytics.retention_days` 可配 **7–365**（未配置 = 90，越界值钳制进域，env `TORCHWOOD_ANALYTICS_RETENTION_DAYS`）。保留精度承诺为**整月**（不足整月的过期分区留待下月裁剪）；窗口外查询显式报错（不静默空结果）。daily 聚合表不受裁剪影响（保留期外仅剩日粒度口径）。 |
| **rollup 新鲜度** | rollup worker **每小时**幂等重算（昨日终算 + 当日部分聚合）→ 聚合面数据延迟 ≤1h；worker 停摆时 DAY 查询自动回退 raw（§5 择路），恢复后补算正确（覆盖式）。 |
| **source 口径语义（D8）** | 每个查询响应带 `source: "rollup" \| "raw"`：**rollup** = 预聚合表直读（毫秒级、小时级新鲜度）；**raw** = 原始事件实时扫描（即时、受窗口护栏与 15s 语句超时约束）。择路规则见 §5。同一页面混用两种口径时由 Console `SourceBadge`/`SourceNote` 明示，API 消费方应将 source 与数值一起呈现。 |

## 3 快速上手

### 3.1 端侧摄入（client 面）

摄入门禁 = 至少匿名会话（`ACCESS_END_USER`）；无会话调用被认证拦截器拒绝。

```bash
# ① 创建匿名会话（正式用户用邮箱/OAuth2 登录流，token 同源）
TOKEN=$(curl -s -X POST http://127.0.0.1:9080/v1/account/sessions/anonymous \
  -H 'Content-Type: application/json' \
  -d '{"project_id":"myapp"}' | jq -r '.tokens.access_token')

# ② 批量摄入（user_id 由服务端从 Principal 落定，请求体不携带——红线）
curl -s -X POST http://127.0.0.1:9080/v1/analytics/events \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "events": [
      { "name": "level_start",    "session_id": "s-1", "props": { "level": 3, "scene": "forest" } },
      { "name": "level_complete", "session_id": "s-1", "props": { "level": 3, "duration_ms": 52100 } }
    ]
  }'
# → {"accepted":2,"skipped":0}
```

### 3.2 server 面摄入与查询（API Key，scope `analytics:write` / `analytics:read`）

```bash
K="<project API key>"   # 项目绑定在密钥上，无需 X-Torchwood-Project

# 服务端权威事件（支付完成、订阅续费、函数侧业务事件；user_id 可信代报）
curl -s -X POST http://127.0.0.1:9080/v1/server/analytics/events \
  -H "X-API-Key: $K" -H 'Content-Type: application/json' \
  -d '{"events":[{"name":"purchase_completed","user_id":"u-123","props":{"amount":9.9,"currency":"CNY"}}]}'
# → {"accepted":1,"skipped":0}

# 窗口 KPI（事件总量/UV/新增/人均）+ Top 事件 + 今日实时数
curl -s "http://127.0.0.1:9080/v1/server/analytics/overview?period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z" -H "X-API-Key: $K"

# 趋势（granularity: ANALYTICS_GRANULARITY_DAY | _HOUR；names 可选 ≤10 个）
curl -s "http://127.0.0.1:9080/v1/server/analytics/timeseries?names=level_complete&period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z&granularity=ANALYTICS_GRANULARITY_DAY" -H "X-API-Key: $K"

# 维度拆解（Top-N ≤50 + __other__ 归并 + (unset) 桶；窗 ≤30 天）
curl -s "http://127.0.0.1:9080/v1/server/analytics/breakdown?name=level_complete&prop_key=scene&period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z&top_n=10" -H "X-API-Key: $K"

# 留存矩阵（cohort 窗 ≤92 天 × D0–D14；retained[k] = cohort+k 日仍活跃用户数）
curl -s "http://127.0.0.1:9080/v1/server/analytics/retention?cohort_start=2026-08-01T00:00:00Z&cohort_end=2026-09-10T00:00:00Z" -H "X-API-Key: $K"

# 用户行为轨迹下钻（raw keyset 分页；窗 ≤92 天可选）
curl -s "http://127.0.0.1:9080/v1/server/analytics/users/u-123/events?page_size=50" -H "X-API-Key: $K"

# 事件字典（自由上报事后发现入口；last_seen 倒序）
curl -s "http://127.0.0.1:9080/v1/server/analytics/event-definitions?page_size=100" -H "X-API-Key: $K"
```

超窗/坏参数返回 InvalidArgument（护栏在 app 层，先于落库）；时间参数一律 RFC3339。

## 4 Console 分析区

一级导航 Analytics，四个页面（`console/src/routes/analytics/`）：

| 页 | 内容 | 数据源 |
|----|------|--------|
| 概览 | KPI 卡 + 趋势图 + Top 事件；今日视图 HOUR（raw 即时），7d/30d 走 DAY | GetOverview / QueryTimeseries |
| 事件 | 字典列表（last_seen 倒序 + 页内过滤）→ 详情：趋势 + 维度拆解 | ListEventDefinitions / QueryTimeseries / QueryBreakdown |
| 留存 | cohort × D0–D14 热力矩阵（未来日 `—`、零规模 cohort 行隐藏） | QueryRetention |
| 用户下钻 | 用户详情页"行为轨迹"时间线（session/source/props 展开） | ListUserEvents |

口径标注：每张卡片带 `SourceBadge`（rollup 预聚合 / raw 实时扫描）与脚注说明；注销近似口径在留存/UV 相关视图有提示文案。

## 5 存储与查询择路

- 原始表 `analytics_events`：**月度 RANGE 分区**（分区键 `occurred_at`，钳制窗保证跨月界安全）+ DEFAULT 兜底；索引 name/time、user/time（下钻+合规点删）、BRIN、GIN `jsonb_path_ops`。追加只写、无唯一约束（D14）。**不建 project_id 列**——schema 即项目边界（项目删除 = DROP SCHEMA CASCADE 零额外清理）。
- 预聚合三表：`analytics_daily`（事件×日 total + **精确** unique_users）、`analytics_user_days`（用户×日活跃集，UV/留存共同基座，行数 = ΣDAU 可控）、`analytics_user_first_seen`（cohort 锚点 + 下钻档案）；字典 `analytics_event_definitions`（摄取时 upsert，`total_30d` 由 rollup 每小时刷新）；合规队列 `analytics_user_deletions`。
- 查询择路（D8/PR3）：DAY 粒度优先 `analytics_daily`，**daily 无覆盖时（worker 停摆/未部署）窗口 ≤ raw 保留期回退 raw 扫描并标 `source=raw`**——回退同时是生产期 worker 滞后的自愈路径；HOUR 恒 raw（≤7 天）；Breakdown 恒 raw（≤30 天）；Retention 恒 rollup（user_days/first_seen 基座）；ListUserEvents 恒 raw（keyset 分页，`(occurred_at, id)` 游标无重无漏）。

## 6 SDK 用法

### TypeScript（`@torchwood/sdk`）

**端侧摄入 + 批量缓冲器**（`sdk/typescript/src/client/analytics.ts`）：

```ts
import { Torchwood, AnalyticsEventBuffer } from "@torchwood/sdk";

// 端侧摄入走 Client API（Bearer 会话）：匿名或正式登录流获得的 access token。
const tw = Torchwood.withAccessToken(endpoint, projectId, accessToken);

// 批量缓冲器：size/time 双阈值 flush（默认 20 条或 10s）、页面隐藏尽力 flush
// （浏览器自动挂 visibilitychange/beforeunload）、失败静默 + 有界退避重试。
// send 是注入点——Web 用 SDK 的 ingest，小游戏/原生可换成 wx.request 等。
const events = new AnalyticsEventBuffer((batch) => tw.analytics.ingest(batch), {
  maxBatchSize: 20,      // size 阈值（上限 100 = 服务端单批上限）
  flushIntervalMs: 10_000, // time 阈值
  maxRetries: 2,         // 指数退避重试次数（500ms*2^n）；耗尽静默丢弃
});

events.track({ name: "level_up", session_id: sid, props: { level: 3 } });
// 页面卸载前（可选，尽力语义）：await events.flush();
// 停止定时与页面钩子（缓冲保留）：events.dispose();
```

缓冲器语义要点（类注释同步声明）：`track`/`flush` **永不抛错**；`accepted/skipped` 响应不抛错（部分接收语义）；单批失败按 408/429/5xx/网络错误判定可重试（其余 4xx 重试必败直接放弃），重试期间批保留在途（不丢批），耗尽后该批**静默丢弃**——at-least-once（D14），重复计数由平台口径吸收。

**server 查询面**（`sdk/typescript/src/server/analytics.ts`）：`tw.server.analytics.ingest / getOverview / listEventDefinitions / queryTimeseries / queryBreakdown / queryRetention / listUserEvents`——与 §3 curl 一一对应。

### Go（`sdk/go/server`）

```go
client, err := server.New(target, server.WithAPIKey(k))
resp, err := client.Analytics.IngestEvents(ctx, &serverv1.IngestServerEventsRequest{Events: events})
```

（`serverv1` = `github.com/torchwoodcloud/torchwood/genproto/server/v1`；查询方法与 §3 curl 一一对应。）

CLI（`bin/torchwood`）零登记：`sdk/go/server` 的反射覆盖测试保证每个 server RPC 可经 `InvokeJSON` 调用，新增 RPC 无需在 CLI 登记。Agent 经 scoped API Key（`analytics:read`）可直接做运营问答。

## 7 端配方（各端接入）

平台只收 `session_id`（≤255 字符，可空）不解释语义——**会话边界由各端定义**（典型：30 分钟无事件即开启新会话）。各端共同的摄取前提：client 面要求至少匿名会话，端需先取得 Bearer token（匿名 `POST /v1/account/sessions/anonymous` 或正式登录流）。所有端共享 at-least-once 口径：补发可能重复，无需端侧去重（§2）。

### Web（浏览器）

- **主通道**：`AnalyticsEventBuffer`（size/time 双阈值 + 静默失败 + 有界退避）。
- **页面隐藏**：缓冲器已自动挂 `visibilitychange`（`hidden` 时）与 `beforeunload` 尽力 flush。
- **卸载送达增强**：unload 竞速下普通 fetch 不保证送达。推荐给缓冲器注入 `keepalive: true` 的 fetch（≤64KiB，可携带 Authorization 头）：

```ts
const events = new AnalyticsEventBuffer((batch) =>
  fetch(`${endpoint}/v1/analytics/events`, {
    method: "POST",
    keepalive: true, // 卸载阶段仍可送达；可携带 Bearer 头
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify({ events: batch }),
  }).then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
);
```

- **`navigator.sendBeacon` 的边界**：sendBeacon **不支持自定义请求头**，而 Client API 的 Bearer 会话必须走 Authorization 头——直接 beacon 到 `/v1/analytics/events` 无法通过认证。适用形态：端自建同源 relay（页面会话态在 cookie 内、relay 服务端注入凭证后转发），再 `sendBeacon(relayUrl, blob)`。无 relay 的项目用 keepalive fetch 即可覆盖绝大多数卸载场景。

### 微信小游戏

切后台时 `wx.request` 会被平台挂起且不保证送达（**已知平台行为**），因此可靠性**不能依赖 onHide 时刻的网络请求**，主通道是本地持久队列：

1. **写侧**：`track()` → 事件先 `wx.setStorage` 落盘队列（追加批文件，建议按批分段防膨胀），再做内存缓冲的常规 flush（`wx.request`，成功后从队列移除对应段）。
2. **onHide**：尽力 flush——把内存缓冲同步落盘 + 发起一次 `wx.request`（不等待结果，允许挂起失败；队列里已有本批，不丢）。
3. **onShow / 冷启动**：读 storage 队列**补发**（按段逐批 `wx.request`，成功逐段删除）——补发可重复（at-least-once）。
4. **会话**：`session_id` 由端生成并持久（如 UUID），按自定义规则轮换；匿名会话 token 同样落 storage 并随失效刷新。
5. 单批 ≤100、单事件 ≤16KiB 为平台硬限；超限拆批。

### 原生 App（iOS / Android）

- **前后台切换**：`applicationDidEnterBackground`（iOS）/ `onPause`（Android）触发尽力 flush（后台有 ~秒级挂起窗口，配合 BGTask/WorkManager 兜底更佳）；回前台时补发本地队列（可选 SQLite/文件持久化，形态同小游戏的持久队列）。
- **HTTP**：任意 HTTP 客户端直调 `POST /v1/analytics/events`（`Authorization: Bearer <token>`），批 ≤100。
- **会话边界**：端自定义（典型 30 分钟无事件切新 `session_id`）；user 归因 = 项目内登录（或匿名）会话，服务端从 Principal 落定。

## 8 合规与注销

- 注销（server 面 `DeleteUser` / client 面 `DeleteAccount`）在既有删除事务内插入 tombstone（`analytics_user_deletions`，重删幂等）；maintenance worker 每 6h 消费：分批硬删该用户 raw（`(user_id, occurred_at)` 索引点删，批 ≤5000，单用户单轮 ≤50 批、单轮 ≤200 用户）→ user_days → first_seen → `done_at`。
- 聚合计数（daily）不回退——历史 UV/留存为近似口径（§2）；隐私上行为明细（可含 props PII）随注销彻底消失。
- 事件数据不出项目 schema：物理隔离同文档面，无跨项目查询面。
