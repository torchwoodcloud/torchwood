# 18 Analytics：内置事件分析

面向后端开发者与端侧接入者：双面事件摄入、月分区只写通道、重算式预聚合与固定形状查询，以及 TS SDK 批量缓冲器与各端接入配方。

> 源码锚点：`internal/domain/analytics/`、`internal/app/analytics/`、`internal/infra/bun/bunrepo/analytics_*`、六表 DDL `internal/infra/projectschema/migrations/000022_analytics.*.sql`、`worker/analytics_*.go`、`proto/client/v1/analytics.proto`、`proto/server/v1/analytics.proto`、`sdk/typescript/src/{client,server}/analytics.ts`、`console/src/routes/analytics/`、`cli/analytics.go`。
> 设计稿：`docs/design/analytics.md`（D1–D15 决策记录）；执行计划：`docs/design/analytics-execution-plan.md`。

## 0. 子系统定义与边界

**Analytics 子系统 = 以事件（Events）为核心的行为分析整体方案**，数据流一条线：

**双面摄入**（client 面 Principal 归因 + server 面 API Key 可信代报）→ 项目 schema 内**只写月分区原始表** → worker **每小时幂等重算**的三张预聚合表（daily / user_days / first_seen）→ **固定形状查询 RPC**（响应带 `source: rollup|raw` 口径标注）→ Console 分析区 / SDK / CLI / Agent 查询消费。

事件通道**独立于文档层**（D1）：静态系统表落位项目数据面 schema、不进 outbox、不发 realtime、不触发函数、无 RLS / `_acl` / `_version`（行为数据按项目整体授权，不做文档级权限）。系统静态表（users / sessions / …）是它的边界邻居：注销钩子在删除用例事务内写入 tombstone，是两个子系统的唯一交点。

### 模块地图

| 层 | 模块 | 职责 |
|---|---|---|
| 领域 | `internal/domain/analytics/` | 平台级上限常量**单一来源**（`limits.go`：批 100 / 名 64 / 键 25 / 16KiB / prop 值截断 256 / 字典软上限 1000 / 钳制窗 `[now-24h, now+5min]`）、事件与查询结果模型、摄入 / 查询 / worker 三组端口、查询窗口护栏常量、`NormalizeRetentionDays` |
| 应用 | `internal/app/analytics/` | 逐事件校验（形状 / 钳制 / 标量化截断 / 16KiB）、归因落定（client = Principal、server = 可信代报）、字典 upsert + 软上限、`accepted/skipped` 部分接收、计量 Incr、查询护栏与 rollup/raw 择路、幂等重算、分区治理与 tombstone 清洗编排 |
| 传输 | `internal/api/clientgrpc/analytics.go` + `servergrpc/analytics.go` | 双面 handler：client 绑定 Principal 归因与 `source=client`；server 走 scope（`analytics.write` 摄入 / `analytics.read` 查询）；请求形状校验由 protovalidate 声明、`ValidateInterceptor` 链尾统一求值 |
| 存储 | `internal/infra/projectschema/migrations/000022_analytics.*.sql` | 项目 schema 内 `analytics_*` 六表 DDL：月 RANGE 分区原始表 + 三张预聚合表 + 字典 + 合规队列 |
| 适配器 | `internal/infra/bun/bunrepo/analytics_{ingest,query,worker}_repo.go` | 全部业务 SQL；值恒绑定参数、schema 名经 ident 校验 + 引号转义；SQL 形状断言进 sqlshape 护栏家族（`*_sqlshape_test.go`） |
| worker | `worker/analytics_rollup.go`（每小时）+ `analytics_maintenance.go`（分区与裁剪每日 / 清洗每 6h） | 周期、日志与 Prometheus 指标壳；业务在 app 层（遍历 active 项目，单项目失败仅记日志继续）；单轮预算 10min，启动即先跑一轮 |
| 注销钩子 | `internal/app/client/account.go` + `internal/app/server/users.go` | 既有删除事务内 tombstone INSERT（幂等），失败随事务回滚 |
| 审计 / 限流 / 计量 | `interceptor/audit.go` 静默清单 + 通用限流拦截器 + billing `MetricAnalyticsEvents` | server 面 IngestEvents 显式豁免审计（护栏测试锁定双面）；client 面摄入按 user 维度通用限流；accepted 条数 best-effort 计入用量 |
| Console | `console/src/routes/analytics/` | 概览（KPI + 趋势 + Top）、事件字典 + 详情、留存矩阵、用户行为轨迹；SourceBadge / SourceNote 口径标注 |
| SDK / CLI | TS `client/analytics.ts`（含 `AnalyticsEventBuffer`）与 `server/analytics.ts`；Go `sdk/go/server/analytics.go`；CLI `torchwood analytics` | 端侧摄入薄封装 + size/time 双阈值缓冲器；server 查询面薄封装；CLI 一等命令组覆盖全部 7 个 server RPC |

### 范围外

服务端 sessionization（会话边界由各端 SDK 定义，平台只收 `session_id`）；漏斗（有序事件序列——关卡进度类可降解为 breakdown）；维度值预聚合表（D9 演进路径，触发条件：长窗拆解性能投诉）；实时推送；OLAP 适配器实现（只留端口接缝）；事件回填导入与事件名合并治理；转发 destination（事件外发 Plausible / PostHog 等）。

### 关键不变量（变更评审锚点）

1. **client 面红线**：请求无 `user_id` 字段、无任何查询方法；归因唯一来源是 Principal（含匿名会话），请求体无法伪造。`source` 列（client | server）由双面 handler 各自绑定，不进请求体（`clientgrpc/analytics.go:47-52`、`servergrpc/analytics.go:61-65`）。
2. **通道红线**：摄入 / 查询 / rollup / 清洗全链路不进 outbox、不发 realtime、不触发函数；无 RLS、无 `_acl`、无 `_version`。
3. **不去重**：摄入幂等 = 接受重复（at-least-once）；`analytics_events` 无唯一约束（PK `(id, occurred_at)` 只为分区路由与 keyset 分页）。
4. **上限常量单源**：`internal/domain/analytics/limits.go` ↔ protovalidate 注解逐字对齐；键数、16KiB 体积、钳制窗、prop 值截断等**复合校验在 app 用例层**（`app/analytics/ingest.go`）。修改任一值必须同步 proto 与本文档。
5. **部分接收**：批内逐事件校验，坏事件（坏形状 / 时间越界 / NaN·Inf 数值 / 超软上限的新名）计入 `skipped`、好事件照收，不整批拒绝；事件名软上限在**摄取期**执行——已达 1000 时只拒新名，存量名不受影响。
6. **UV 口径排除空归属**：全部 unique_users（raw 面 FILTER 与 rollup 写侧 daily/user_days 同口径）只统计 `user_id <> ''` 的归属事件；无归属事件计入 total、不计入 UV——raw 与 rollup 两分支数字严格一致（`bunrepo/analytics_query_repo.go` UV 口径块、`app/analytics/uv_caliber_integration_test.go`）。
7. **审计豁免显式登记**：server 面 IngestEvents 登记进审计静默清单，client 面按"非 AccountService 不落审计"规则天然豁免；护栏测试锁定双面（`interceptor/audit.go:55-57`、`audit_test.go:56-63`）。
8. **查询全参数化 + 白名单**：事件名 / prop_key 服务端白名单正则校验后仍走绑定参数，无字符串拼接值；查询读事务首语句 `SET LOCAL statement_timeout='15s'; SET LOCAL TimeZone='UTC'`（慢查询显式报错不挂死）。护栏：HOUR 窗 ≤7 天、DAY 窗 ≤366 天、Overview 窗 ≤366 天、Breakdown ≤30 天、Retention cohort 窗 ≤92 天、ListUserEvents 窗 ≤92 天、Top-N ≤50、names ≤10。
9. **聚合可重建**：daily / user_days / first_seen 全部可从 raw 幂等重算（容灾重建路径）；rollup = 覆盖式 `ON CONFLICT DO UPDATE`，同窗口重跑不翻倍；first_seen 用 LEAST/GREATEST 极值聚合天然幂等。
10. **分区治理先于写入**：worker 每日预建当月起共 3 个月分区 + DEFAULT 非空搬运（父表 ACCESS EXCLUSIVE 锁内 TEMP 表中转）；保留期裁剪 = DROP 整月分区（分区名必须匹配 `analytics_events_YYYY_MM` 形状，不足整月部分留待下月）。
11. **worker 表边界**：rollup / 清洗只读写项目 schema 内 `analytics_*` 静态表，不触碰其他表。
12. **合规清洗**：注销事务内插 tombstone → worker 异步分批**硬删**身份关联三表（raw 批 ≤5000 行 → user_days → first_seen）→ 三表清完才标 `done_at`（raw 未清空不删用户粒度表，防止 rollup 从残存 raw 复活行）；聚合计数不回退（§2）；重删幂等。
13. **计量口径**：按 **accepted 条数**计量（skipped 不计费），写入当前小时 Redis bucket（WithoutCancel + 200ms，失败仅告警）→ usage_rollups → 账单。
14. **bun 写规范**：摄入 / rollup 写路径显式列白名单（`17-update-write-guard.md`）。

## 1. 事件模型与上限

事件 = `name + occurred_at? + props? + session_id?`（server 面多一个可信代报 `user_id`）。`occurred_at` 缺省 = 服务端 now（批级统一时钟，批内判定一致）；app 层钳制 `[now-24h, now+5min]`，越界计入 `skipped`（D4：客户端时间为主语义，离线补传事件算对日，防伪由钳制窗承担；`occurred_at`/`ingested_at` 双列留档）。

| 平台级强制上限 | 值 | 执行点 |
|----|----|----|
| 单请求事件数 | 100 | protovalidate（`min_items/max_items`） |
| 事件名长度 / 形状 | 64 / `^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$` | protovalidate（app 层防御性复检） |
| props 键数 / 键形状 | 25 / `^[a-zA-Z_][a-zA-Z0-9_.]{0,63}$` | 键形状 protovalidate；键数与键正则 app 层 |
| 单事件序列化体积 | 16 KiB（规范化 props JSON 的序列化体积） | app 层 |
| prop 字符串值长度 | 256 字符（超长**截断**入库，不拒收） | app 层 |
| 事件名项目级软上限 | 1000（存量名不受影响，仅拒新名） | app 层（摄取期字典计数前置检查） |
| session_id / user_id 长度 | 255 字符 | protovalidate |

props 值仅接受**标量或浅数组**：null / bool / number（NaN·Inf 判坏事件）/ string（截断）/ 数组（元素递归标量化，空数组合法）；object（含数组内嵌套）拒绝。props 是维度键值不是自由文档。

请求 / 响应：`IngestEventsRequest { events[1..100] }` → `IngestEventsResponse { accepted, skipped }`——**部分接收语义**（移动端重试批中混有脏数据时最大化接收）。存储侧预留通用上下文维度列 `platform` / `app_version`：当前双面摄入 proto 均无写入字段（列已就位、v1 预留展示位），查询响应 `AnalyticsUserEvent.platform/app_version` 恒为空串。

props 最小化指引：只放低基数维度键（场景 / 关卡 / 渠道），不放昵称 / 手机号 / openid 等 PII（PII 硬防护 blocklist 为设计开放问题，当前为文档指引）。

## 2. 口径声明（消费方必读）

| 口径 | 声明 |
|----|----|
| **at-least-once** | 平台**不做摄入去重**。端侧重试、页面隐藏补发、持久队列重放都会造成同一事件重复上报——计数类接受 ±1% 量级偏差；UV / 新增 / 留存类指标因按归属用户去重，重复上报天然不放大。TS SDK 缓冲器的重试语义建立在此声明上。`client_event_id` 去重列为演进路径（当前未实现）。 |
| **UV 排除空归属** | UV 类指标（unique_users / 今日 UV / 留存 / 新增）只统计有归属用户的事件；server 面不带 `user_id` 的事件计入总量、不进 UV。raw 面与 rollup 面同口径，同一批数据两分支数字一致。 |
| **UV 注销后近似** | 注销用户的 raw / user_days / first_seen 行被异步硬删，但 `analytics_daily` 聚合计数**不回退**——注销时点之前的 unique_users / 留存历史为**近似值**（含已注销用户），Console 以口径提示标注。 |
| **UTC 切日** | DAY 粒度桶、留存 cohort、`first_day/last_day` 全部按 **UTC 零点**切日（查询读事务 `SET LOCAL TimeZone='UTC'` 钉死）；HOUR 桶为 UTC 整点。Console 时间窗同样按 UTC 取整对齐。时区化日粒度是演进项。 |
| **raw 保留期** | 原始事件与 user_days 按保留期裁剪（DROP 整月分区 + user_days 批量修剪）：**默认 90 天**，`analytics.retention_days`（env `TORCHWOOD_ANALYTICS_RETENTION_DAYS`）可配 **7–365**（未配置 = 90，越界值钳制进域）。保留精度承诺为**整月**；窗口外查询显式报错（不静默空结果）。daily 聚合表不受裁剪影响（保留期外仅剩日粒度口径）。 |
| **rollup 新鲜度** | rollup worker **每小时**幂等重算（昨日终算 + 当日部分聚合）→ 聚合面数据延迟 ≤1h；`total_30d` 同轮覆盖式刷新（窗口外名字归 0）。worker 停摆时查询自动回退 raw（§5），恢复后补算正确（覆盖式）。 |
| **source 口径语义** | 每个查询响应带 `source: "rollup" \| "raw"`：**rollup** = 预聚合表直读（毫秒级、小时级新鲜度）；**raw** = 原始事件实时扫描（即时、受窗口护栏与 15s 语句超时约束）。同一页面混用两种口径时由 Console SourceBadge / SourceNote 明示，API 消费方应将 source 与数值一起呈现。 |

## 3. 快速上手

### 3.1 端侧摄入（client 面）

摄入门禁 = 至少匿名会话（`ACCESS_END_USER`）；无会话或非端用户 Principal 被认证拦截器 / handler 拒绝。

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

### 3.2 server 面摄入与查询（API Key，scope `analytics.write` / `analytics.read`）

```bash
K="<project API key>"   # 项目绑定在密钥上，无需 X-Torchwood-Project

# 服务端权威事件（支付完成、订阅续费、函数侧业务事件；user_id 可信代报，缺省=无归属）
curl -s -X POST http://127.0.0.1:9080/v1/server/analytics/events \
  -H "X-API-Key: $K" -H 'Content-Type: application/json' \
  -d '{"events":[{"name":"purchase_completed","user_id":"u-123","props":{"amount":9.9,"currency":"CNY"}}]}'
# → {"accepted":1,"skipped":0}

# 窗口 KPI（事件总量/UV/新增/人均）+ Top 事件（≤10）+ 今日实时数
curl -s "http://127.0.0.1:9080/v1/server/analytics/overview?period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z" -H "X-API-Key: $K"

# 趋势（granularity: ANALYTICS_GRANULARITY_DAY | ..._HOUR；names 可选 ≤10 个，多名字恒 raw）
curl -s "http://127.0.0.1:9080/v1/server/analytics/timeseries?names=level_complete&period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z&granularity=ANALYTICS_GRANULARITY_DAY" -H "X-API-Key: $K"

# 维度拆解（Top-N ≤50 缺省 20 + __other__ 归并 + (unset) 桶；窗 ≤30 天）
curl -s "http://127.0.0.1:9080/v1/server/analytics/breakdown?name=level_complete&prop_key=scene&period_start=2026-09-04T00:00:00Z&period_end=2026-09-11T00:00:00Z&top_n=10" -H "X-API-Key: $K"

# 留存矩阵（cohort 窗 ≤92 天 × D0–D14；retained[k] = cohort+k 日仍活跃用户数）
curl -s "http://127.0.0.1:9080/v1/server/analytics/retention?cohort_start=2026-08-01T00:00:00Z&cohort_end=2026-09-10T00:00:00Z" -H "X-API-Key: $K"

# 用户行为轨迹下钻（raw keyset 分页，occurred_at 倒序；可选窗 ≤92 天，缺省全保留期）
curl -s "http://127.0.0.1:9080/v1/server/analytics/users/u-123/events?page_size=50" -H "X-API-Key: $K"

# 事件字典（自由上报事后发现入口；last_seen 倒序 + total_30d 排序参考）
curl -s "http://127.0.0.1:9080/v1/server/analytics/event-definitions?page_size=100" -H "X-API-Key: $K"
```

超窗 / 坏参数返回 InvalidArgument（护栏在 app 层，先于任何触库调用）；时间参数一律 RFC3339。读方法对 Console admin 全角色会话开放（viewer 含，需带 `X-Torchwood-Project` 切项目）+ API key `analytics.read`；摄入为 business_write 档（member 及以上 + `analytics.write`）。

## 4. Console 分析区

一级导航 Analytics（对全角色 admin 开放，不设 RequireRole），三个 tab + 两个子页（`console/src/routes/analytics/`）：

| 页面 / 路由 | 内容 | 数据源 |
|----|------|--------|
| 概览 `/console/analytics` | 范围切换 今日 / 7d / 30d：KPI 卡 + 趋势图 + Top 事件表；今日视图趋势走 HOUR（raw 即时），7d/30d 走 DAY | GetOverview / QueryTimeseries |
| 事件 `/console/analytics/events` | 字典列表（last_seen 倒序 + 已加载页内名字过滤 + 近 30 天总量） | ListEventDefinitions |
| 事件详情 `/console/analytics/events/:name` | 24h/7d/30d 趋势（HOUR/DAY 切换，HOUR 下 30d 收敛为 7d）+ 维度拆解条形图（prop_key 客户端正则预检，服务端为准） | QueryTimeseries / QueryBreakdown |
| 留存 `/console/analytics/retention` | 近 7/30/90 天 cohort × D0–D14 热力矩阵（未来日 `—`、零规模 cohort 行隐藏、单元格为留存率） | QueryRetention |
| 用户下钻 `/console/users/:userId` 的行为轨迹页 | 事件时间线（occurred_at 倒序 + session / source / props 展开 + 已加载页内名字过滤；接收时间与上报时间偏差提示离线补传） | ListUserEvents |

口径标注：相关卡片带 SourceBadge（rollup 预聚合 / raw 实时扫描）与 SourceNote 脚注；注销近似口径、rollup 每小时刷新节奏在相应视图有提示文案。

## 5. 存储与查询择路

- 原始表 `analytics_events`：**月度 RANGE 分区**（分区键 `occurred_at`，钳制窗保证跨月界安全）+ DEFAULT 兜底；父表索引级联子分区：name+time（趋势/拆解主路）、user+time（下钻 + 合规点删）、BRIN（时间范围兜底）、GIN `jsonb_path_ops`（props 维度查询）。追加只写、无唯一约束（不去重）；PK `(id, occurred_at)`，id 为 BIGSERIAL 调试序。**不建 project_id 列**——schema 即项目边界（项目删除 = DROP SCHEMA CASCADE 零额外清理）。
- 预聚合三表：`analytics_daily`（(day,name) 主键，事件 × 日 total + **精确** unique_users）、`analytics_user_days`（(user_id,day) 主键活跃集，UV / 留存共同基座，随 raw 保留期同步修剪）、`analytics_user_first_seen`（first_day/last_day，cohort 锚点 + 下钻档案）；字典 `analytics_event_definitions`（摄取时 upsert，`total_30d` 由 rollup 每小时覆盖刷新）；合规队列 `analytics_user_deletions`。
- rollup 写事务首语句 `SET LOCAL statement_timeout='2min'`（大事件量日允许重扫但不挂死）；查询读事务 15s。
- 查询择路（`app/analytics/query.go`）：
  - **GetOverview**：覆盖检测命中走 rollup（total 从 daily 求和、UV 从 user_days 窗口去重跨日不重复、Top 从 daily）；新增用户**恒** first_seen 表口径（表空 = 0）；今日实时块**恒** raw（当日 UTC 零点起）。
  - **QueryTimeseries**：HOUR 恒 raw（≤7 天）；DAY 粒度多事件名（>1）恒 raw（并集 UV 不可从 daily 导出），单名/全事件优先 rollup，覆盖缺失回退 raw（≤保留期）。
  - **QueryBreakdown** 恒 raw（≤30 天；Top-N 子查询重算归并 `__other__`，UV 为其余集整体去重的精确值）。
  - **QueryRetention** 恒 rollup（first_seen ⋈ user_days 自连接；基座表空返回空矩阵）。
  - **ListUserEvents** 恒 raw（keyset 分页，`(occurred_at, id)` 倒序游标，Limit+1 探测 hasMore，无重无漏）。
- **覆盖检测（新鲜度口径，双信号缺一不可）**：① 窗口内存在 daily 行（该区域曾被计算）；② daily 全表最新 day ≥ 窗口末日 − 3 天容差（worker 活性；零事件日缺行属正常态，容差容纳停摆 ≤2 个整日 + 日界余量）。两信号任一不满足 → 窗口 ≤ raw 保留期才回退 raw 并标 `source=raw`，超窗明确报错——回退同时是生产期 worker 滞后的自愈路径。

## 6. SDK 用法

### TypeScript

**端侧摄入 + 批量缓冲器**：

```ts
import { Torchwood, AnalyticsEventBuffer } from "@torchwood/sdk";

// 端侧摄入走 Client API（Bearer 会话）：匿名或正式登录流获得的 access token。
const tw = Torchwood.withAccessToken(endpoint, projectId, accessToken);

// 批量缓冲器：size/time 双阈值 flush（默认 20 条或 10s）、页面隐藏尽力 flush
//（浏览器自动挂 visibilitychange/beforeunload，非浏览器环境自动跳过）、
// 失败静默 + 有界退避重试。send 是注入点——Web 用 SDK 的 ingest，
// 小游戏/原生可换成 wx.request 等。
const events = new AnalyticsEventBuffer((batch) => tw.analytics.ingest(batch), {
  maxBatchSize: 20,        // size 阈值（钳制 ≤100 = 服务端单批上限，超设自动分批）
  flushIntervalMs: 10_000, // time 阈值
  maxRetries: 2,           // 指数退避重试次数（基延迟 retryBackoffMs 默认 500ms）；耗尽静默丢弃
  flushOnHide: true,       // 页面隐藏钩子开关（默认开）
});

events.track({ name: "level_up", session_id: sid, props: { level: 3 } });
events.pending;            // 当前缓冲事件数（监控/测试）
// 页面卸载前（可选，尽力语义）：await events.flush();
// 停止定时与页面钩子（缓冲保留，仍可手动 track/flush）：events.dispose();
```

缓冲器语义要点：`track` / `flush` **永不抛错**；`accepted/skipped` 响应不抛错（部分接收语义）；单批失败按 408 / 429 / 5xx / 网络错误判定可重试（其余 4xx 重试必败直接放弃），重试期间批保留在途（不丢批），耗尽后该批**静默丢弃**并停止本轮（剩余事件等下次触发）——at-least-once 口径，重复计数由平台吸收。测试可经 `timers` 注入假时钟确定性驱动。

**server 查询面**：`tw.server.analytics` 提供 `ingest` / `getOverview` / `listEventDefinitions`（及带分页 meta 的 `listEventDefinitionWithMeta`）/ `queryTimeseries` / `queryBreakdown` / `queryRetention` / `listUserEvents`（及 `listUserEventsWithMeta`）——与 §3 curl 一一对应；时间参数 RFC3339 字符串。注意 proto int64 字段（`total_events`、`retained[]` 等）在 JSON 序列化中为**字符串**，消费侧需自行数值化。

### Go

```go
client, err := server.New(target, server.WithAPIKey(k))
resp, err := client.Analytics.IngestEvents(ctx, &serverv1.IngestServerEventsRequest{Events: events})
```

`client.Analytics` 暴露 `IngestEvents / GetOverview / ListEventDefinitions / QueryTimeseries / QueryBreakdown / QueryRetention / ListUserEvents` 七个薄封装；scope 常量 `server.ScopeAnalyticsRead/Write`（`analytics.read` / `analytics.write`）。

CLI 一等命令组 `torchwood analytics`：`ingest`（`--file <path|->` 必填，支持 stdin；载荷为裸事件数组或 `{"events":[...]}` 对象，JSONL 不支持并显式拒绝）/ `overview` / `events list` / `timeseries`（`--granularity hour|day`）/ `breakdown`（`--name --prop-key [--top-n]`）/ `retention` / `user-events <user-id>`；`--from/--to` 接受 RFC3339 或 `YYYY-MM-DD`（补 UTC 零点）。`torchwood rpc` 逃生舱兜底任意方法。Agent 经 scoped API Key（`analytics.read`）可直接做运营问答。

## 7. 端配方（各端接入）

平台只收 `session_id`（≤255 字符，可空）不解释语义——**会话边界由各端定义**（典型：30 分钟无事件即开启新会话）。各端共同的摄取前提：client 面要求至少匿名会话，端需先取得 Bearer token（匿名会话或正式登录流）。所有端共享 at-least-once 口径：补发可能重复，无需端侧去重。单批 ≤100、单事件 ≤16KiB 为平台硬限，超限拆批。

### Web（浏览器）

- **主通道**：`AnalyticsEventBuffer`（size / time 双阈值 + 静默失败 + 有界退避）。
- **页面隐藏**：缓冲器已自动挂 `visibilitychange`（hidden 时）与 `beforeunload` 尽力 flush（非浏览器环境自动跳过）。
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

- **`navigator.sendBeacon` 的边界**：sendBeacon **不支持自定义请求头**，而 Client API 的 Bearer 会话必须走 Authorization 头——直接 beacon 无法通过认证。适用形态：端自建同源 relay（页面会话态在 cookie 内、relay 服务端注入凭证后转发），再 `sendBeacon(relayUrl, blob)`。无 relay 的项目用 keepalive fetch 即可覆盖绝大多数卸载场景。

### 微信小游戏

切后台时 `wx.request` 会被平台挂起且不保证送达（**已知平台行为**），可靠性**不能依赖 onHide 时刻的网络请求**，主通道是本地持久队列：

1. **写侧**：`track()` → 事件先 `wx.setStorage` 落盘队列（追加批文件，按批分段防膨胀），再做内存缓冲的常规 flush（成功后从队列移除对应段）。
2. **onHide**：尽力 flush——把内存缓冲同步落盘 + 发起一次请求（不等待结果，允许挂起失败；队列里已有本批，不丢）。
3. **onShow / 冷启动**：读 storage 队列**补发**（按段逐批，成功逐段删除）——补发可重复（at-least-once）。
4. **会话**：`session_id` 由端生成并持久（如 UUID），按自定义规则轮换；匿名会话 token 同样落 storage 并随失效刷新。缓冲器的页面钩子在非浏览器环境自动跳过， onHide/onShow 自行驱动 flush 与补发。

### 原生 App（iOS / Android）

- **前后台切换**：`applicationDidEnterBackground`（iOS）/ `onPause`（Android）触发尽力 flush（后台有秒级挂起窗口，配合 BGTask / WorkManager 兜底更佳）；回前台时补发本地队列（可选 SQLite / 文件持久化，形态同小游戏）。
- **HTTP**：任意 HTTP 客户端直调 `POST /v1/analytics/events`（Bearer 头），批 ≤100。
- **会话边界**：端自定义（典型 30 分钟无事件切新 session）；user 归因 = 项目内登录（或匿名）会话，服务端从 Principal 落定。

## 8. 合规与注销

- 注销（client 面 DeleteAccount / server 面 DeleteUser）在既有删除事务内插入 tombstone（`ON CONFLICT (user_id) DO NOTHING` 重删幂等；软删提交而 tombstone 缺失的窗口不存在——失败向上传播，注销整体可重试收敛）。maintenance worker 每 6h 消费：单轮 ≤200 用户、按 enqueued_at 先进先出；单用户分批硬删 raw（(user_id, occurred_at) 索引 + PK 精确行定位，批 ≤5000、单用户单轮 ≤50 批）→ user_days → first_seen → `done_at`；raw 未清空（预算耗尽）时三表不动、done 不标，下一轮续删。
- 聚合计数（daily）不回退——历史 UV / 留存为近似口径（§2）；隐私上行为明细（可含 props PII）随注销彻底消失。
- 事件数据不出项目 schema：物理隔离同文档面，无跨项目查询面。

## 相关文档

- `12-sdk.md` — SDK 全景与 `AnalyticsEventBuffer` 位置
- `06-databases.md` — 文档面与事件脊柱（analytics 刻意不接入的对照面）
- `17-update-write-guard.md` — bun 显式列写规范（analytics 写路径护栏同源）
- `docs/design/analytics.md` — 设计决策全文
