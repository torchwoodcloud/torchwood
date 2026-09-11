# Analytics 执行计划

> 对应批准设计：`docs/design/analytics.md`（v2，2026-09-11；七项 owner 决策 + D1–D15，经三路独立设计交叉验证）  
> 日期：2026-09-11  
> 交付节奏（owner 拍板）：**P0 = PR1+PR2**（摄入+存储+计量，先行合入）；**P1 = PR3+PR4**（查询面+Console 核心）；**P2 = PR5+PR6**（worker+合规+SDK+文档）。

```
PR1 地基（proto/authz/迁移/端口）
  → PR2 摄入链（P0 完成；狗粮 = 薄封装 + SQL 直查）
       ├─→ PR3 查询面 ─→ PR4 Console（P1 完成）
       ├─→ PR5 worker + 合规（PR2 后可与 PR3/PR4 并行）
       └─→ PR6 SDK + 文档（PR2 后可并行；留存页依赖 PR5 数据）
```

PR3/PR4 串行（Console 消费查询 API 契约）；PR5、PR6 与 P1 并行开发，随 P2 窗口合入。

## 共同约束（每张 PR）

- 读 `AGENTS.md` 与批准设计对应章节；产品决策不重开（七项 owner 拍板 + D1–D15，见设计稿）。
- 禁止手改 `genproto/**` 与 `internal/pkg/config/bind.go` 生成物。改 proto 后 `task generate:proto`；改配置 proto 后 `task generate:config`；改 Wire 后 `task wire:all`。
- 改 Console 后 `task console:build` 再 `task build`。
- 对话与 commit 用简体中文。
- 每张 PR 结束必须：`go vet ./...`、`go test -short ./...` 绿；触及 SDK 时跑 `go test ./sdk/go/...` 与 `sdk/typescript` 测试。
- 不把未完成的 PR 混进同一提交。
- **红线（违反即打回）**：
  - client 面请求无 `user_id` 字段、无任何查询方法；归因唯一来源是 Principal（D3）。
  - 摄入不写 audit_logs：server 面 `IngestEvents` 必须登记进 `audit.go` 静默清单，且 `audit_test.go` 有护栏用例（D13）。
  - 不进 outbox、不发 realtime、不触发函数（D1）。
  - 不做摄入去重（D14）；"顺手加幂等键"打回。
  - 查询 SQL 全参数化；`prop_key` / 事件名 / 时区经服务端白名单校验（沿 `aggregateDocuments` 纪律）；查询事务内 `SET LOCAL statement_timeout = '15s'`。
  - bun 写路径显式列白名单；新 SQL 进 `bunrepo` 形状护栏测试家族。

## PR1 — 地基（proto / authz / 迁移 / 端口）

**目标**：`proto/client/v1/analytics.proto` + `proto/server/v1/analytics.proto` 全量方法签名与 protovalidate 注解（查询方法本 PR 仅出 proto 与注册，实现留 PR3）；`shared/v1/authz.proto` 增 `SCOPE_RESOURCE_ANALYTICS = 14` 及 scope 词表三处对齐（`internal/domain/auth`、`cmd/server/internal/runtime/authz_policy.go`、sdk 契约测试）；projectschema 迁移 `000019_analytics`（六表 DDL + 当月/次月静态分区 + DEFAULT 兜底）；bun model；`internal/domain/analytics` 端口与领域类型（上限常量：批 100 / 键 25 / 16KiB / 名 64 / 软上限 1000 / 钳制窗 `[now-24h, now+5min]`）；bunrepo 摄入仓储（多行单语句 INSERT + 字典 upsert）与 SQL 形状护栏测试。

**不做**：handler 与用例（PR2）、查询实现（PR3）、worker（PR5）。

**关键验收**

- `task generate:proto` 后三个契约测试绿：`grpc_swagger_test`、sdk scope 契约、authz 矩阵；`AssertSemantic` 通过（词表对齐强制）。
- 迁移幂等：同一项目 schema 重复 `Apply` 无差异；存量项目在下次触碰时自动补齐。
- 建表后 `analytics_events` 为月 RANGE 分区父表，索引（name/time、user/time、BRIN、GIN jsonb_path_ops）级联到子分区。
- 摄入仓储 SQL 形状测试进护栏家族（无裸拼接、列白名单）。

**命令**：`task generate:proto`；`task wire:all`；`go test -short ./...`。

## PR2 — 摄入链（P0 收口）

**目标**：clientgrpc / servergrpc `IngestEvents` handler；`internal/app/analytics` 摄入用例——逐事件校验（name 正则 / props 标量化与截断 / 16KiB）、`occurred_at` 缺省 now + 钳制越界 `skipped`、client 面归因 Principal / server 面可信代报、字典 upsert + 事件名 1000 软上限、`accepted/skipped` 响应；计量 `MetricAnalyticsEvents`（`KnownMetric` 登记 + accepted 条数 `Incr`，best-effort `WithoutCancel`）；审计豁免登记 + `audit_test` 护栏；Wire 装配与集成测试。

**关键验收**

- 匿名会话可摄入；无会话调用被认证拦截器拒绝（门禁决策）。
- client 面 proto 无 `user_id` 请求字段；服务端落库 user_id 来自 Principal（伪造用例：body 塞不进）。
- 混合批：含越界时间戳 / 坏 props 的批，好事件落库、坏事件计入 `skipped`，不整批拒绝。
- 字典：新事件名 upsert first/last_seen；第 1001 个新名被 skip、存量名照常（并发软上限轻微超扣可容忍）。
- 计量：集成测试断言 Redis 小时桶值 = accepted 条数。
- server 面 `IngestEvents` 调用后 `audit_logs` 无新行（`audit_test` 断言 + 手工验证）。
- 多行单语句 INSERT；`go test -short ./...` 绿。

**附注（P0 独立运行的硬时限）**：迁移静态预建仅覆盖当月+次月。**PR2 合入后若预计超 ~1 个月仍未合入 PR5，必须把 maintenance worker 的分区预建循环单独提前**（几十行），否则新事件落入 DEFAULT 分区。

## PR3 — 查询面（P1 前半）

**目标**：7 个查询 RPC 实现（`GetOverview` / `ListEventDefinitions` / `QueryTimeseries` / `QueryBreakdown` / `QueryRetention` / `ListUserEvents`）；护栏（HOUR 窗 ≤7d、Breakdown 窗 ≤30d、Retention cohort 窗 ≤92d、Top-N ≤50、ListUserEvents 窗 ≤92d）；择路与**覆盖检测回退**；`source: rollup|raw` 口径标注；集成测试。

**关键验收**

- 护栏矩阵全部生效（超窗 InvalidArgument，不落库不扫表）。
- 择路：DAY 粒度优先 `analytics_daily`；**daily 无覆盖时（worker 未部署 / worker 停摆），窗口 ≤ raw 保留期回退 raw 扫描并标 `source=raw`**——该回退同时是 PR5 合入前 P1 阶段的正确性保障，也是生产期 worker 滞后的自愈路径。
- Breakdown：Top-N + `__other__` 归并计数正确；`(unset)` 桶处理。
- Retention：构造测试数据，矩阵（cohort × D0–D14）与手算一致。
- ListUserEvents keyset 分页（occurred_at,id 游标）无重无漏。
- 注入面：prop_key / 事件名 / timezone 参数化测试（含恶意输入用例）。
- `statement_timeout` 生效（慢查询报错不挂死）。

## PR4 — Console（P1 收口）

**目标**：`console/src/api/analytics.ts`；Analytics 一级导航；概览页（KPI 卡 + 趋势 + Top 事件）、事件页（字典列表 → 详情：趋势 + 维度拆解）、用户详情页"行为轨迹"入口（ListUserEvents 时间线）；引入 recharts。

**不做**：留存页（PR5 数据就绪后随 PR6 交付）。

**关键验收**

- 概览/事件页数据与 API 一致；`source` 口径提示可见（rollup/raw 混合时明示）。
- `task console:build && task build` 绿；Console 组件测试。

## PR5 — worker 与合规（P2 前半）

**目标**：`worker/analytics_rollup.go`（每小时：昨日终算 + 当日部分聚合，幂等 `ON CONFLICT DO UPDATE` 覆盖语义；维护 daily / user_days / first_seen / 字典 `total_30d`）；`worker/analytics_maintenance.go`（每日预建未来 2 月分区 + DEFAULT 非空搬运 + 按 `analytics.retention_days` DROP 过期月分区 + tombstone 清洗：批删三表身份关联行后标记 done_at）；`DeleteUser` 与 `DeleteAccount` 双钩子（既有事务内 tombstone INSERT，幂等）；config `analytics.retention_days`（默认 90，域 7–365）；故障与幂等测试。

**关键验收**

- 重跑幂等：worker 同窗口重跑不翻倍（覆盖式，非累加）。
- 停摆恢复：worker 停 1 天重启后补算正确（昨日终算覆盖）。
- 跨月边界：预建分区生效，写入落正确分区；构造 DEFAULT 非空场景验证搬运事务。
- 保留期：DROP 过期分区后，护栏外查询明确报错（不静默空结果）。
- 注销：tombstone → raw/user_days/first_seen 三表硬删（`(user_id, occurred_at)` 点删）→ `done_at`；聚合计数不回退；重删幂等。
- config 绑定经 `task generate:config`；默认值与域校验生效。

## PR6 — SDK 与文档（P2 收口）

**目标**：TS SDK `AnalyticsService`（批量缓冲 size/time 双阈值 + 退避重试 + 失败静默丢弃，accepted/skipped 不抛错）；Console 留存页（cohort 热力矩阵）；端配方文档（Web `visibilitychange`+`sendBeacon`；小游戏 `wx.setStorage` 持久队列 + onHide 尽力 flush + onShow 补发；原生前后台切换）；`docs/developer/18-analytics.md`（模块地图 + 不变量，对齐 06-databases 体例）；authz-matrix 文档更新。

**关键验收**

- SDK 单测：flush 触发（条数/时间）、失败静默、重试不丢批语义（at-least-once 声明）。
- 留存页渲染 PR5 产出的 user_days 数据正确。
- CLI 零登记验证：`sdk/go/server` 反射覆盖测试自动纳入新 RPC（跑一遍确认绿，无 CLI 改动）。
- 18-analytics.md 含 D14（接受重复）、UV 注销近似口径、UTC 切日口径的显式声明。

## 完成后交给审查方

实施方提交：分支 / diff、每张 PR 的自测命令与输出、与设计的偏差清单（无则写「无偏差」）。

审查方对照批准设计逐项读源码并亲自跑命令，不以自报为准。**Analytics 额外审查点**：client 面无 user_id 字段且无查询方法；摄入路径无 outbox/realtime/函数触发引用；无去重逻辑；查询 SQL 无字符串拼接值；server 摄入的审计豁免有护栏测试；bun 写路径全列白名单；P0 过渡债（注销钩子、分区预建）已随 PR5 清偿。
