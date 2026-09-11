# Analytics：内置事件分析

> 状态：**已批准（2026-09-11；七项 owner 决策 + D1–D15 经三路独立设计交叉验证）**  
> 执行计划：`docs/design/analytics-execution-plan.md`  
> 日期：2026-09-11  
> 驱动场景：Torchwood 自营微信小游戏运营分析（狗粮消费者，不进设计输入——本稿为通用 BaaS 能力）

---

## 已锁定决策（owner 拍板，2026-09-11）

| 议题 | 决定 |
|------|------|
| 事件治理 | **自由上报 + Console 端事后发现**（无预注册制；字典与上限护栏见 §3/§5） |
| 产品归位 | **roadmap 独立一节**（§4.7 Analytics）——与 Databases / Storage / Functions 同级的一等公民服务 |
| 首版留存 | **轻量实现**（D11：user_days 自连接 cohort 矩阵，不物化 cohort 表） |
| client 摄入门槛 | **要求至少匿名会话**（`ACCESS_END_USER` 门禁即要求有效会话） |
| 摄入与审计 | **不进 audit_logs**（server 面摄入方法需显式登记豁免，见 D13） |
| 交付节奏 | **P0 = S1–S3 先行合入**（摄入 + 存储 + 计量；狗粮场景薄封装直调摄入端点 + SQL 直查兜底），查询面/Console/worker 按真实使用反馈后续切片 |
| 外部转发 | **转发 destination（事件外发 Plausible/PostHog 等）为将来可选补充**（~2 人日小特性，与内置分析共存），不作为内置分析的替代 |

## 关键技术决策（v2，经独立设计交叉验证；来源标注：〔收敛 n/3〕= n 个隔离子代理独立得出相同结论，〔裁决〕= 分歧经论据回溯裁定，〔独有〕= 单方案发现并验证采纳）

| # | 决策 | 来源 |
|---|------|------|
| D1 | 事件通道独立于文档层：项目 schema 静态系统表、不进 outbox/Redis Stream/Realtime、无 RLS | 〔收敛 3/3〕 |
| D2 | 摄入 = 同步多行 INSERT 直写 PG，不经任何缓冲队列；事件是可下钻业务数据而非可丢指标 | 〔收敛 3/3〕 |
| D3 | 双面摄入：client 面（principal 归因，不可伪造）+ server 面（API Key，可信代报 user_id） | 〔收敛 3/3〕 |
| D4 | 事件时间：`occurred_at`（客户端，缺省=服务端 now）为主语义 + 钳制 `[now-24h, now+5min]` 越界丢弃；`ingested_at` 双列留档。分区键=occurred_at（钳制窗保证跨月界安全，DEFAULT 分区兜底竞态） | 〔裁决：3/3 取客户端时间+钳制，弃原稿"接收时间权威"——离线补传事件算对日；防伪诉求由钳制窗承担；±24h 取三案中最紧〕 |
| D5 | 原始表**月度 RANGE 分区** + worker 预建未来 2 月 + DEFAULT 兜底；保留期裁剪 = DROP 整月分区，O(1)。弃日分区（×365/年/项目 的 DDL 对象增长喂养 B12 迁移重放劣化指标）与单表（DELETE+VACUUM 膨胀不可接受，两个单表方案均自报为最薄弱处） | 〔裁决：月分区论据最硬——B12 指标是仓库既有门禁；保留期默认 90 天、可配 7–365〕 |
| D6 | 预聚合三表：`analytics_daily`（事件×日 total+**精确** unique_users）、`analytics_user_days`（用户×日活跃集，UV/留存的共同基座，行数=ΣDAU 可控）、`analytics_user_first_seen`（cohort 锚点+下钻档案）。**弃"窗口外 UV 不可用"**——user_days 使保留期内任意窗口 UV 精确可算；**弃小时粒度聚合**——HOUR 查询走 raw（≤7 天窗） | 〔收敛 3/3（user_days）+裁决（日粒度）〕 |
| D7 | rollup = **幂等重算式**（每小时：昨日终算 + 当日部分聚合，`ON CONFLICT DO UPDATE` 覆盖语义），非水位增量（+Δ 无法维护 distinct 类聚合、水位/CAS 是新机器）；全部聚合可从 raw 重算 = 天然容灾重建路径 | 〔裁决：重算 vs 水位 vs 日终三案，重算+当日部分取新鲜度/复杂度平衡〕 |
| D8 | 查询 = 固定形状 RPC（趋势/拆解/留存/下钻/字典/概览），不用 DSL、不扩 `pkg/query`（白名单纪律建立在 catalog 声明属性上，自由 JSONB props 不适用）；**双源口径标注**：响应带 `source: rollup\|raw`，Console 明示 | 〔收敛 3/3 + 独有（source 标注）〕 |
| D9 | 维度拆解 v1 = raw 直查 `GROUP BY props->>$key` + Top-N（≤50）+ `__other__` 归并 + 窗口护栏（≤30 天）+ 语句超时；`analytics_daily_props` 预聚合列为演进路径（触发条件：长窗拆解性能投诉） | 〔裁决：2 预聚合 vs 1 raw + 原稿 raw，按"留存轻量"基调和工程量裁 raw 先行〕 |
| D10 | 合规 = 注销事务内插 tombstone（`analytics_user_deletions`）+ worker 异步分批**硬删**身份关联三表（raw / user_days / first_seen，`(user_id, occurred_at)` 索引点删）；聚合计数（daily）不可回退为已声明近似口径。**弃同步删**（重度用户百万行拖垮注销事务）、**弃匿名化置 NULL**（props 可含 PII，硬删更彻底；匿名行对下钻无价值） | 〔收敛 2/3（tombstone+硬删）+裁决〕 |
| D11 | 留存轻量 = user_days + first_seen 自连接单 SQL 矩阵（cohort 窗 ≤92 天、D0–D14、日粒度），不物化 cohort 表、不引 HLL/bitmap | 〔收敛 3/3（自连接）〕 |
| D12 | 事件字典摄取时 upsert（发现即时）+ 事件名项目级软上限 1000（存量名不受影响，仅拒新名——护栏必须在摄取期拦截，事后扫描无法拒收）；批内逐事件校验、坏事件 `skipped` 不拒整批 | 〔独有采纳：摄取期字典+软上限是护栏唯一执行锚点；部分接收是移动端重试友好语义〕 |
| D13 | 审计豁免必须**显式登记**：server 面摄入是非读动词、默认会落审计——在 `internal/api/interceptor/audit.go` 的静默清单登记（复用 `auditSilentClientMethods` 先例），`audit_test.go` 补护栏。client 面天然豁免（`auditRowEligible` 只审 AccountService 非读动作，已验证） | 〔收敛 3/3——三案独立发现同一落点；原稿 v1 漏此点〕 |
| D14 | 摄入幂等：v1 **接受重复**（at-least-once，DISTINCT 类指标天然免疫，计数类对 ±1% 不敏感，文档声明口径）；`client_event_id` partial unique 去重列为演进 | 〔收敛 3/4〕 |
| D15 | 新计量点 `MetricAnalyticsEvents`（`UsageCounter.Incr` accepted 条数）进既有用量脊柱；限流复用拦截器（user 维度）；查询 `SET LOCAL statement_timeout` | 〔收敛 3/3〕 |

时区（原稿 D8 修正）：v1 日粒度按 **UTC 切日**（三案独立共识），文档明示；HOUR 粒度 raw 路径接受可选 `timezone` 参数（IANA，`date_trunc` AT TIME ZONE，成本为零）；日粒度时区化（需小时聚合或重桶）列为演进。

---

## Overview

```
 端 SDK（TS，批量缓冲 + accepted/skipped 语义）         服务端（支付回调/函数/后台任务）
        │ POST /v1/analytics/events（会话，含匿名）          │ POST /v1/server/analytics/events（API Key）
        └────────────────┬──────────────────────────────────┘
                         ▼
        拦截器链：鉴权 → 限流(user) → Usage(api_calls) → Validate
                         ▼
        app/analytics.TrackEvents（逐事件校验/钳制/归因/字典 upsert/计量）
                         ▼
        infra 适配器（projectschema.Apply 就绪 → 多行 INSERT）
                         ▼
   ┌─────────────────────────────────────────────────────┐
   │ tw_<project>.analytics_events（月 RANGE 分区，追加只写）│
   │   + analytics_event_definitions（字典，摄取时维护）     │
   └───────────────┬─────────────────────────────────────┘
                   ▼ worker（每小时，幂等重算）
   ┌─────────────────────────────────────────────────────┐
   │ analytics_daily（事件×日 total+精确UV）                 │
   │ analytics_user_days（用户×日活跃集，留存/UV 基座）       │
   │ analytics_user_first_seen（cohort 锚点/下钻档案）       │
   └───────────────┬─────────────────────────────────────┘
                   ▼ 查询（固定形状 RPC，source 口径标注）
   Server API（scope analytics:read / Console 会话 / Agent）
         → Console 分析区（概览/事件/留存/下钻）+ CLI（零登记自动覆盖）
```

设计主线：**两条摄入路径 → 单条只写时间序列通道（月分区）→ 重算式预聚合三表 → 固定形状查询**。热路径只做校验 + 一次多行 INSERT + 一次字典 upsert；一切分析读聚合表或带护栏的 raw。

## 1. 背景与动机

- **产品定位缺口**：自托管 BaaS 线（Appwrite/Supabase/PocketBase）均无真产品分析能力；Torchwood 的 Agent-Native 定位使分析查询面天然可授权给 Agent 问答。
- **既有积木**：用量计量脊柱（`UsageCounter` → `usage_rollups` → 账单）、限流拦截器、按项目 schema 物理隔离、`bunrepo.Scoped` 模式、worker Lynx 装配、TS SDK、Console server API 消费先例——全部复用。
- **明确不复用**：文档事件脊柱（outbox 的 1MiB 信封/ACL 快照/24h 重放窗都是为文档写语义付费）与 DocumentDB 文档层（RLS/OCC/两段式 DDL 拖累，无 COUNT DISTINCT/时间分桶，且每事件强制 outbox 行）。

## 2. 明确不在本期范围

服务端 sessionization（会话边界由各端 SDK 定义，平台只收 `session_id`）；漏斗（有序事件序列——关卡进度类可降解为 breakdown）；维度值预聚合表（D9 演进）；实时推送；OLAP 适配器实现（只留端口接缝）；事件回填导入与事件名合并治理（§13 开放问题）；We分析等第三方对接（应用侧策略）；转发 destination（事件外发 Plausible/PostHog 等外部分析系统）——将来可选补充的小特性（与内置分析共存、非替代，owner 拍板，见已锁定决策表）。

## 3. 事件模型

```proto
message AnalyticsEvent {
  // ≤64，^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$（protovalidate）
  string name = 1;
  // 缺省 = 服务端 now；app 层钳制 [now-24h, now+5min]，越界计入 skipped（D4）
  google.protobuf.Timestamp occurred_at = 2;
  // ≤25 键；键 ^[a-zA-Z_][a-zA-Z0-9_.]{0,63}$；值标量或浅数组；单事件序列化 ≤16KiB
  map<string, google.protobuf.Value> props = 3;
  // 端自报会话标识，可空；平台不解释语义
  string session_id = 4;
}
```

| 上限（平台级强制） | 值 |
|----|----|
| 单请求事件数 | 100 |
| 单事件 props 键数 / 序列化体积 | 25 / 16 KiB |
| prop 字符串值长度 | 256 字符（超长截断入库） |
| 事件名项目级软上限 | 1000（存量名不受影响，D12） |
| session_id / user_id 长度 | 255 字符 |

请求/响应：`IngestEventsRequest { repeated AnalyticsEvent events }` → `IngestEventsResponse { int32 accepted; int32 skipped }`——**部分接收语义**（坏事件跳过、好事件照收，移动端重试批中混有脏数据时最大化接收）〔独有采纳〕。

语义约定：**at-least-once**（D14）——补发与重试可致重复计数，DISTINCT 类指标免疫，计数类接受 ±1%，文档声明口径。

## 4. API 设计

### 4.1 server 面（`proto/server/v1/analytics.proto`）

```proto
service AnalyticsService {
  option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_SERVER };

  // 服务端权威事件（支付完成、订阅续费、函数侧业务事件），可信代报 user_id
  rpc IngestEvents(IngestServerEventsRequest) returns (IngestEventsResponse) {
    option (google.api.http) = { post: "/v1/server/analytics/events", body: "*" };
    option (torchwood.shared.v1.method_auth) = {
      admin_roles: [ADMIN_ROLE_MEMBER, ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]
      api_key_scope: { resource: SCOPE_RESOURCE_ANALYTICS, op: SCOPE_OP_WRITE }
    };
  }
  rpc GetOverview(GetOverviewRequest) returns (AnalyticsOverview) { /* READ */ }
  rpc ListEventDefinitions(ListEventDefinitionsRequest) returns (ListEventDefinitionsResponse) { /* READ */ }
  rpc QueryTimeseries(QueryTimeseriesRequest) returns (QueryTimeseriesResponse) { /* READ */ }
  rpc QueryBreakdown(QueryBreakdownRequest) returns (QueryBreakdownResponse) { /* READ */ }
  rpc QueryRetention(QueryRetentionRequest) returns (QueryRetentionResponse) { /* READ */ }
  rpc ListUserEvents(ListUserEventsRequest) returns (ListUserEventsResponse) { /* READ */ }
}
```

- `shared/v1/authz.proto` 增 `SCOPE_RESOURCE_ANALYTICS = 14`；`internal/domain/auth` scope 词表与 `cmd/server/internal/runtime/authz_policy.go` 映射同步（`AssertSemantic` 强制对齐）；swagger 一致性与 sdk scope 契约测试自动跟进。
- 读方法对 Console admin 全角色会话开放（viewer 含）+ API Key `analytics:read`。
- 查询公共字段：`period_start`/`period_end`（必填有界窗）；**护栏**（app 层）：HOUR 粒度窗 ≤7 天走 raw、DAY ≤366 天走 `analytics_daily`、Breakdown/属性过滤窗 ≤30 天走 raw、Retention cohort 窗 ≤92 天走 user_days、ListUserEvents 窗 ≤92 天 keyset 分页；全部语句 `SET LOCAL statement_timeout = 15s`。
- **响应带 `source: "rollup" | "raw"`**（D8 口径标注，Console 展示）。
- `GetOverview`：窗口 KPI（事件总量、UV、新增用户、人均事件）+ Top 事件 + 今日实时数（raw 直读）。

### 4.2 client 面（`proto/client/v1/analytics.proto`）

```proto
service AnalyticsService {
  option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_END_USER };
  rpc IngestEvents(IngestEventsRequest) returns (IngestEventsResponse) {
    option (google.api.http) = { post: "/v1/analytics/events", body: "*" };
  }
}
```

user_id/session_id 一律取自 principal（含匿名会话），请求体不可指定；**只有写入没有查询**（行为数据是项目方资产）。

### 4.3 摄入端平台策略

- **计量**：accepted 条数 → `UsageCounter.Incr(ctx, projectID, MetricAnalyticsEvents, n)`（best-effort `WithoutCancel`，镜像 usage 拦截器模式）；`KnownMetric` 登记，自动进 usage_rollups → 账单。
- **审计豁免**（D13）：server 面 `IngestEvents` 登记进 `audit.go` 静默清单（`audit_test.go` 护栏）；client 面天然豁免。
- **限流**：复用现有拦截器 user 维度（默认档已足：1000 请求/min × 100 事件/批）。

## 5. 存储设计（projectschema 迁移 `000019_analytics.up.sql`）

```sql
-- ① 原始事件：月度 RANGE 分区，追加只写（D5）。PK 含分区键；BIGSERIAL 仅调试序
--    （PG<17 分区表限 IDENTITY；无唯一约束——接受重复 D14）。
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events (
    id           BIGSERIAL NOT NULL,
    name         TEXT NOT NULL,
    user_id      TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    source       TEXT NOT NULL DEFAULT 'client' CHECK (source IN ('client','server')),
    platform     TEXT NOT NULL DEFAULT '',      -- 通用上下文维度（web/ios/android/mp...）
    app_version  TEXT NOT NULL DEFAULT '',
    occurred_at  TIMESTAMPTZ NOT NULL,
    ingested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    props        JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX IF NOT EXISTS analytics_events_name_time ON {{schema}}.analytics_events (name, occurred_at DESC);
CREATE INDEX IF NOT EXISTS analytics_events_user_time ON {{schema}}.analytics_events (user_id, occurred_at DESC); -- 下钻+合规点删
CREATE INDEX IF NOT EXISTS analytics_events_brin ON {{schema}}.analytics_events USING brin (occurred_at);
CREATE INDEX IF NOT EXISTS analytics_events_props_gin ON {{schema}}.analytics_events USING gin (props jsonb_path_ops);

-- 迁移静态预建当月+次月分区（字面量）+ DEFAULT 兜底（钳制窗保证 DEFAULT 仅竞态）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_events_default
    PARTITION OF {{schema}}.analytics_events DEFAULT;

-- ② 事件字典（D12：摄取时 upsert；软上限的锚点；Console 发现入口）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_event_definitions (
    name        TEXT PRIMARY KEY,
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL,
    total_30d   BIGINT NOT NULL DEFAULT 0      -- rollup 维护（排序用）
);

-- ③ 事件×日聚合（D6：精确 UV；用户删除后为近似口径，文档声明）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_daily (
    day          DATE NOT NULL,
    name         TEXT NOT NULL,
    total        BIGINT NOT NULL,
    unique_users BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (day, name)
);

-- ④ 用户×日活跃集（UV/留存共同基座；行数 = Σ日活，随 raw 保留期同步修剪）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_days (
    user_id TEXT NOT NULL,
    day     DATE NOT NULL,
    events  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

-- ⑤ 用户首见/末见（cohort 锚点 + 下钻档案）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_first_seen (
    user_id    TEXT PRIMARY KEY,
    first_day  DATE NOT NULL,
    last_day   DATE NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ⑥ 合规清洗队列（注销钩子写入，worker 消费，D10）
CREATE TABLE IF NOT EXISTS {{schema}}.analytics_user_deletions (
    user_id     TEXT PRIMARY KEY,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    done_at     TIMESTAMPTZ
);
```

要点：

- **分区管理**：worker 每日预建未来 2 月（`CREATE TABLE IF NOT EXISTS ... PARTITION OF`，幂等）；保留期裁剪按 `analytics.retention_days`（默认 90，域 7–365，config）DROP 整月过期分区（<30 天精度的保留策略 v1 不承诺，文档声明）。
- **user_days 修剪**：与 raw 同窗口（保留期内 UV 精确，窗口外只有 daily 的日粒度精确 UV）。
- **不建 project_id 列**：schema 即项目边界（项目删除 = DROP SCHEMA CASCADE 零额外清理）。

## 6. 查询 SQL 骨架（评审用；实现沿 `aggregateDocuments` 白名单纪律，prop_key/timezone 服务端校验后参数化）

```sql
-- Timeseries：DAY → analytics_daily 直读（毫秒级）；HOUR → raw 分区裁剪
SELECT day, total, unique_users FROM analytics_daily WHERE name = ANY($1) AND day BETWEEN $2 AND $3;
SELECT date_trunc('hour', occurred_at) AS bucket, COUNT(*), COUNT(DISTINCT user_id)
FROM analytics_events WHERE name = ANY($1) AND occurred_at >= $2 AND occurred_at < $3 GROUP BY 1;

-- Breakdown（raw + Top-N + __other__ 应用层归并）
SELECT COALESCE(props->>$key, '(unset)') AS val, COUNT(*) AS cnt, COUNT(DISTINCT user_id) AS uv
FROM analytics_events WHERE name = $1 AND occurred_at >= $2 AND occurred_at < $3
GROUP BY 1 ORDER BY cnt DESC LIMIT $top_n;

-- Retention（D11 轻量：first_seen ⋈ user_days 自连接矩阵）
SELECT fs.first_day AS cohort, COUNT(*) AS size,
       COUNT(du.user_id) FILTER (WHERE du.day = fs.first_day + $k) AS retained_k
FROM analytics_user_first_seen fs
LEFT JOIN analytics_user_days du ON du.user_id = fs.user_id AND du.day = fs.first_day + $k
WHERE fs.first_day BETWEEN $1 AND $2 GROUP BY 1;   -- k=0..14 逐列（15 次毫秒级查询或单条交叉表 SQL）

-- ListUserEvents（keyset 分页）
SELECT name, occurred_at, ingested_at, source, platform, app_version, session_id, props
FROM analytics_events WHERE user_id = $1 AND occurred_at < $cursor ORDER BY occurred_at DESC LIMIT $page_size;
```

## 7. Worker 职责（`worker/`，Lynx service + Wire，`projects.ListProjects` 遍历，单项目失败仅记日志）

| 作业 | 周期 | 职责 |
|------|------|------|
| `AnalyticsRollupWorker` | 每小时 | **幂等重算**（D7）：昨日终算 + 当日部分聚合 → `analytics_daily`（`ON CONFLICT DO UPDATE` 覆盖）、`analytics_user_days`（DO NOTHING + 计数覆盖）、`analytics_user_first_seen`（LEAST/GREATEST upsert）、字典 `total_30d` 刷新。全量可从 raw 重算 = 容灾路径 |
| `AnalyticsMaintenanceWorker` | 分区每日 / 清洗每 6h | ① 预建未来 2 月分区 + 按保留期 DROP 过期月分区 + DEFAULT 分区非空搬运（事务内 INSERT..SELECT 后清空）② 消费 `analytics_user_deletions`：批删该用户三表行（`(user_id, occurred_at)` 索引点删，经 user_days/first_seen 反查涉及日期），标记 done_at |

**注销钩子**：`DeleteUser`（server）与 `DeleteAccount`（client）两个删除用例的既有事务内追加 tombstone INSERT（表存在则插、重删幂等）〔独有采纳：client 面删账号同样触发〕。

## 8. 合规

- 删除路径见 D10；聚合计数保留 + Console 脚注声明"注销用户致 unique_users 为近似值"。
- props 最小化文档指引（只放低基数维度键，不放昵称/手机号/openid；PII 硬防护 blocklist 为 §13 开放问题）。

## 9. Console 与 SDK

### Console（一级导航 Analytics）

| 页 | 内容 | 数据源 |
|----|------|--------|
| 概览 | KPI 卡 + 趋势图 + Top 事件 | GetOverview / QueryTimeseries |
| 事件 | 字典列表（事后发现入口）→ 详情页：趋势 + 维度拆解 | ListEventDefinitions / QueryBreakdown |
| 留存 | cohort 热力矩阵（92 天窗 × D0–D14） | QueryRetention |
| 用户下钻 | 用户详情页"行为轨迹"时间线 | ListUserEvents |

图表库 **recharts**〔裁决 3:1 弃自绘 SVG——热力矩阵/多序列交互成本超收益；纯前端依赖不破部署〕。实现：`console/src/api/analytics.ts` + `console/src/routes/analytics/*`。

### SDK（`sdk/typescript`）

- `AnalyticsService.ingest` + 批量缓冲（size/time 双阈值 flush + 退避重试 + 失败静默丢弃，响应 accepted/skipped 不抛错）。
- **端配方文档**（各端会话边界与可靠性，平台不感知）：Web `visibilitychange`+`sendBeacon`；小游戏 `wx.setStorage` 持久队列 + onHide 尽力 flush + onShow 补发（切后台 request 挂起是已知平台行为）；原生前后台切换。
- CLI 零登记（`sdk/go/server` 反射覆盖测试自动纳入）；Agent 经 scoped key `analytics:read` 可做运营问答。

## 10. 落地切片与工程量（v2 修正——v1 估 10–18 人日显著偏低，三案独立估计 23–38 人日收敛于 25–35）

| 切片 | 内容 | 人日 |
|------|------|------|
| S1 | proto 双面 + authz 扩展 + 生成 + 三测试（swagger/scope/authz 矩阵）过绿 | 2–3 |
| S2 | 迁移 000019（分区/索引/字典）+ model + domain 端口 + bunrepo（SQL 形状护栏） | 4–5 |
| S3 | 摄入链：双面 handler + 用例（校验/钳制/归因/字典/软上限）+ 计量 + 审计豁免 + 集成测试 | 3–4 |
| S4 | 查询面 7 RPC + 护栏/择路/超时 + 集成测试 | 4–5 |
| S5 | worker 两作业 + 注销双钩子 + 幂等/故障测试 | 4 |
| S6 | Console 4 页 + recharts + 导航 | 5–6 |
| S7 | TS SDK 缓冲器 + 端配方文档 + 18-analytics.md 开发者文档 | 3 |

**合计 ≈ 25–32 人日**。交付节奏（owner 拍板 2026-09-11）：

- **P0（先行合入）= S1–S3**（≈ 10–12 人日）：摄入双面 + 存储 + 计量 + 审计豁免。狗粮端以薄 HTTP 封装直调摄入端点，运营分析暂以 SQL 直查 `analytics_events` 兜底——即"先采集、分析后补"的形态；后续切片按真实使用反馈校准，比一次性做完 25 天更抗"做不好"的风险。
- **P1 = S4 + S6 核心**（≈ 10–11 人日）：查询面 7 RPC + Console 概览/事件页。
- **P2 = S5 + S7**（≈ 7 人日）：rollup/maintenance worker、注销双钩子、TS SDK 缓冲器、留存页、开发者文档。

**P0→P1/P2 过渡期已知债**（对外发布门禁 `docs/developer/15-exit-poc.md` 前必须补齐）：①注销清理钩子在 S5 才接线（tombstone 表结构 P0 已建，过渡期注销用户的行为数据延迟清理）；②分区预建/保留期裁剪依赖 maintenance worker——迁移静态预建仅覆盖当月+次月，**P0 若独立运行超过 ~1 个月仍未合入 S5，须把分区预建循环提前**（几十行 worker 代码），否则新事件落入 DEFAULT 分区。

## 11. 风险

| 风险 | 缓解 |
|------|------|
| 单 PG 写入天花板与 GIN 索引膨胀未经千万级/天实测（三案共同自报的最薄弱处） | `analytics_events` 计量第一时间暴露异常增长；写入端口化（缓冲落库可替换）；文档声明已验证量级；压测列入 S7 后 |
| 今日数据口径：rollup 小时级新鲜度 vs raw 即时（同页并存困惑） | `source` 字段标注 + Console 口径提示 |
| 高基数 props 滥用 | 25 键/16KiB/256 字符硬限 + Top-N 归并 + 语句超时；事件名 1000 软上限拦字典爆炸 |
| user_days 表体量（100 万 DAU 鲸鱼项目 × 90 天 = 9000 万行） | 保留期同步修剪；D9 预聚合演进路径 |
| 大项目长窗口 Breakdown 超时 | 窗口护栏 ≤30 天 + statement_timeout 显式报错（不挂死） |

## 12. 独立设计交叉验证记录（2026-09-11）

三个互不可见的子代理基于相同任务契约（背景事实 + owner 五项锁定决策 + 硬约束，不含本稿 v1 任何倾向）独立设计，v1 稿同时隐去。比较矩阵摘要：

| 维度 | 案 A | 案 B | 案 C | v1 原稿 |
|------|------|------|------|---------|
| 原始表物理 | 单表 | 单表（identity PK） | **月分区** | 日分区 |
| 事件时间 | occurred_at±7d | occurred_at±24h | occurred_at±72h | 接收时间权威 |
| rollup | 15min 重算近 3 天 | 5min 水位 +Δ | 日终 | 小时 count-only |
| user 粒度聚合 | daily_users | user_daily | user_days | 无 |
| 注销合规 | tombstone 异步硬删 | 同步删 | tombstone 异步硬删 | 内联匿名化 |
| 维度拆解 | 预聚合 top50 | 预聚合+修剪 | raw+TopN | raw+TopN |
| 字典 | 摄取时+1000 上限 | rollup 维护 | 日终扫 | 查询发现 |
| 工程量 | 26–31 | 23–32 | 28–38 | 10–18（偏低） |

**收割**：

- **收敛（3/3，直接采纳）**：项目 schema 静态表落位；不进 outbox、同步直写；双面摄入与 principal 归因；固定形状查询 RPC；`user_days` 用户×日基座；留存自连接；`SCOPE_RESOURCE_ANALYTICS=14`；新计量点；注销需处理身份关联表且聚合保留声明口径；server 摄入审计豁免显式登记（v1 盲点）；Console 走 server API；CLI 零登记；TS SDK 批量缓冲。
- **裁决**：月分区（B12 DDL 对象增长论据）；occurred_at 钳制 ±24h 为主时间；重算式 rollup（否决水位 +Δ——distinct 类聚合固缺陷）；维度拆解 v1 raw 先行；tombstone 硬删（否决同步删/匿名化）；recharts（3:1）；时区 v1 UTC（弃 v1 原稿 tz 参数化为 HOUR 路径可选）。
- **独有采纳（已验证）**：摄取期字典 upsert + 事件名软上限（案 A——护栏唯一执行锚点）；accepted/skipped 部分接收（案 A）；`source: rollup|raw` 口径标注（案 A）；DeleteAccount 双钩子（案 C）；platform/app_version 上下文列（案 C）；occurred_at 缺省服务端 now（案 C）；unnest 多行 INSERT 实现注记（案 C）。
- **独有记录未采纳**：identity 水位 CAS（案 B，演进备选）；client_event_id 去重（案 B，演进）；hourly 聚合（案 B，时区化演进时启用）；自绘 SVG 图表（案 A，被 3:1 裁决）；同步删三表（案 B，重度用户事务风险）。
- **v1 原稿被修正**：D2（时间权威）、D3/D8（UV 窗口外不可用 → user_days 取代；时区参数降级）、D10（匿名化 → tombstone 硬删）、日分区 → 月分区、工程量 10–18 → 25–32 人日、补 server 摄入审计豁免落点。

## 13. 开放问题

1. **事件回填/历史导入**（backfill API 与口径）——四方案（含 v1）均未设计，待首个真实需求。
2. **事件名治理**（改名/合并/归档）——自由上报制的运营债，Console 治理面待定。
3. **props PII 硬防护**（键/值 blocklist）——v1 文档指引，硬防护待合规压力。
4. 表命名前缀：本稿用无前缀 `analytics_*`（与 usage_rollups 等系统表一致）；案 C 建议的 `sys_` 前缀与 000008/000009 系统表迁移的实际命名约定待实现时核对。
