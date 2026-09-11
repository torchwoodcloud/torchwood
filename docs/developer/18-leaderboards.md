# 18. Leaderboards（排行榜）

> 面向后端开发者与游戏/通用消费方。来源：graviton-games dogfooding 提案
> （2026-09-11）评审定型；Phase 1 已落地，Phase 2（声明式结榜发奖）另期。
> 竞品定位与通用化论证见提案归档与 `docs/design/economy-client-write-competitive-analysis.md`。

## 1 模型

**术语中立**：被排名的主体是 `subject_id`（不透明字符串 ≤64；v1 client 面恒为
当前登录用户，server 面可为战队/商家等任意 ID——战队榜今天就能用，缺的只是
client 面"替战队提交"的成员校权，v2 接缝）；值是 `value`（int64，小数域用
放大整数约定：分→厘、秒→毫秒）。

**Board（榜）** —— project 内配置对象（console CRUD，每项目 ≤100）：

| 字段 | 取值 / 缺省 | 说明 |
|---|---|---|
| `id` | `^[a-z_][a-z0-9_]*$` ≤40 | 与 collection ID 规则对齐 |
| `sort` | `desc`（缺省）/ `asc` | 主值方向 |
| `tiebreak_order` | 空（缺省）/ `asc` / `desc` | 声明单列 tiebreak；声明后 submit 必带 `tiebreak_value`（int64，可为 0） |
| `tie_break` | `parallel`（缺省）/ `earliest` / `latest` | 并列裁决（§3.2） |
| `period.kind` | `none` / `daily` / `weekly` / `monthly` | period_key 服务端按 tz 派生（`YYYYMMDD` / ISO 周 `YYYY-Www` / `YYYYMM` / `all`） |
| `period.tz` | IANA 名，期型 ≠ none 时必填 | 期边界所在时区 |
| `policy` | `best`（缺省）/ `latest` / `sum` | `best` 方向跟随 sort（desc→取大、asc→取小，时间/延迟榜可用） |
| `value_min/max` | 可选 | 越界**拒收**（不 clamp）；硬上限 ±(2^53−1) |
| `client_submit` | `false`（缺省） | false 时终端用户提交 `PERMISSION_DENIED` |
| `per_subject_submit_limit` | 100（缺省），1..10000 | 每期每主体提交次数（受理即计数，含 best 策略下的同分重放） |
| `retention_periods` | 0 = 永久（缺省，不默认删数据） | 保留最近 N 期，worker 每 30min 清理更早的期 |
| `subject_kind` | `"user"`（缺省） | 纯展示提示（console 是否链到用户详情），无语义 |

**Entry（条目）**：`(board_id, period_key, subject_id, value, tiebreak_value,
submit_count, updated_at)`，表主键即模型内建去重——一主体一期一条。

**可变性**：`period/sort/tiebreak 声明/tie_break/policy` 在榜内已有条目后不可
改（`FailedPrecondition`）；value 边界（存量不回溯）、`client_submit`、限频、
retention、`subject_kind` 随时可改。

## 2 API

三个面共享响应载荷（`shared.v1.LeaderboardScoreSnapshot` / `ListLeaderboardTopResponse`）：

```
POST /v1/leaderboards/{boardId}:submit          # client 面（session；无 subject 字段——身份从会话派生）
POST /v1/server/leaderboards/{boardId}:submit   # server 面（leaderboards.write；可代任意 subject）
  body: { value, tiebreak_value?, period?, request_id?, subject_id? /*仅 server 面*/ }
  resp: { board_id, period, total, entry, rank, position, below }

GET /v1/leaderboards/{boardId}/me               # client 面：200 + entry 可选（无条目时 total 仍返回）
GET /v1/server/leaderboards/{boardId}/entries/{subjectId}?period=
GET /v1/{,server/,console/}leaderboards/{boardId}/top?period=&page_size=&page_token=
```

- **错误码**：`NOT_FOUND`（board/entry 不存在）、`INVALID_ARGUMENT`（分数越界 /
  period 格式错或不在提交窗口 / tiebreak 存在性不匹配）、`PERMISSION_DENIED`
  （client 提交未开 `client_submit` 的榜）、`RESOURCE_EXHAUSTED`（超限频）、
  `ALREADY_EXISTS`（建榜冲突）、`FAILED_PRECONDITION`（不可变字段）。
- **request_id**：`(project, actor, request_id)` 24h 幂等（与 documents 同一
  中间件语义）——网络层重试返回首次快照，不重复烧限频额度；同 key 不同载荷
  = KEY_CONFLICT。
- **Scopes**：`leaderboards.read` / `leaderboards.write`（API key 点风格）。
- **SDK**：TS `tw.leaderboards.submit(board, value, {tiebreakValue?, period?,
  requestId?})` / `.me(board)` / `.top(board)`；`tw.server.leaderboards.*` 可代
  任意 subject。CLI 零登记自动可用（server 面反射覆盖测试保证）。

## 3 语义

### 3.1 期、宽限与封榜

- 提交窗口：`period` 缺省 = 当前期；显式必须命中 **{当前期, 上一期}**（离线
  补传宽限），其余（未来 / 更早）一律 `INVALID_ARGUMENT`——**封榜即终局，
  对所有人（含 server 面）一致**，不存在"运营补录任意旧期"的通道（历史迁移
  将来走 console 导入工具）。
- **封榜时刻 = 当前期变成 N+2 的瞬间**（即宽限自然到期）。seal 后条目不可变
  （含 console 删条目——作弊处理窗口就是宽限期）。
- 奖励因此晚一个期长到达（daily 榜 = 期结束后 24h）——"补传也算分"的公平性
  换延迟，Phase 2 结算（§6）依赖这一不变量。
- 读路径不受窗口限制：任意保留期内 `me`/`top` 可读（分享卡复走历史期）。

### 3.2 rank / position / below 与并列

- `rank` = **competition ranking**（1,2,2,4；并列同名次）——展示语义。
- `position` = 全序位次（并列也不同）——机制语义，top 分页稳定与"谁紧挨在我
  前面"用它。全序键栈：`value`（按 sort）→ 可选 `tiebreak_value`（按声明方向）
  → `updated_at`（`tie_break=earliest` 缺省序 / `latest` 反向）→ `subject_id`
  （终极兜底，全序必然分出）。
- `tie_break` 三态：`parallel`（并列等同，奖励边界按 rank 含端点——两个并列
  第 10 都获 top10 奖；条目并列内的展示顺序不承诺语义）；`earliest`/`latest`
  （先/后达到者靠前，奖励边界按 position 截断）。
- `below` = value 严格低于我的人数（百分位分子，**不受 tie-break 影响**：
  并列既不算击败也不算被击败）。消费方用 `below/total` 显示"击败 X%"。
- `policy=best` 仅在 (value, tiebreak_value) 字典序严格变优时推进 `updated_at`
  ——重复提交不污染"最早达成"语义。

### 3.3 读一致性与并发

submit 响应中的 rank/total/below 与写入同事务快照；并发提交可能各自看到同一
total（分位是建议性读数）。并发正确性由唯一键 + 行锁保证：N 个不同主体并发
提交后 `total == N`（集成测试覆盖——写时聚合函数做不到的那一条）。

## 4 存储

项目数据面 `projectschema` 迁移 `000019_leaderboards`：`leaderboard_boards` +
`leaderboard_entries`（FK 级联删）。一条复合索引
`(project_id, board_id, period_key, value DESC, tiebreak_value ASC, updated_at ASC, subject_id ASC)`
服务全部读路径：rank/total/below 是一条 `count(*) FILTER` 单次扫描；top 用
`RANK()`（competition）+ `ROW_NUMBER()`（position）窗口。索引方向取最常见
组合（desc+asc+earliest），其余方向组合由 planner 增量排序——v1 量级（单期
≤10 万行）毫秒级，索引策略对 API 不可见，可后调。量级参考：DAU 1 万 × 365
天 ≈ 365 万条/年/榜，retention 90 期稳态约 90 万条。

## 5 非目标（Phase 1）

滚动窗口（非对齐时间窗，另一种数据结构）、多字段任意 tiebreak（需按 board
生成表达式索引）、条目实时事件流、反作弊检测（信任模型 = 客户端自报 +
声明式区间拒收）、`around` 邻域查询（形状已预留）、战队成员拆分发放。
season / 手动期 / 自定义 interval 是 v2 接缝：存储只见不透明 `period_key`，
扩展只是新增推导函数。

## 6 Phase 2：结榜发奖（已定型未实现）

声明式 `rewards[]`（名次区间含端点 / `value_min` 门槛 + asset + amount，
≤20 条，封榜时快照冻结）+ worker 按状态扫描结算（`open → sealed → settling
→ settled`，旁路 `voided`）+ 逐 `(rule, subject)` 以派生幂等键
`lbsettle:{board}:{period}:{rule_index}:{subject}` 走 Assets Grant——
at-least-once 执行、exactly-once 效果，对账复用 Assets Reconcile。奖励边界
跟随 `tie_break`（parallel → rank 含端点；earliest/latest → position 截断）。
`rewards` 要求 `period.kind ≠ none`。作弊窗口 = 宽限期；seal 后只能 void 或
手工 consume 追回。`on_settled` 函数触发（通知/自定义逻辑逃生通道）v1.5。

## 7 Console

`/console/leaderboards`：榜列表 + 建榜/编辑（可变字段）对话框 + 删除（条目
级联）；榜详情页：期下拉 + top 表（rank/position/subject/value/updated_at，
行删条目）+ 按 subject 查条目（含 submit_count）。写操作仅 owner 角色
（console 面 `permissions: ["owner"]`，读 `["owner","admin"]`）。

## 8 审计与运维

- 审计：client 面提交不落审计行（高频日常操作，对齐噪声治理）；server 面
  提交与 console 写操作自动落审计（server 面非读方法规则）；board 配置
  变更可溯。
- worker：`leaderboards-cleaner`（30min）retention 清理——默认 0 = 永久，
  只有显式配置的榜被清；失败仅告警下一轮重试（清理幂等）。
