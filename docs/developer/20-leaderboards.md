# 20 Leaderboards（排行榜）

面向后端开发者与游戏 / 通用消费方：Board / Entry 模型、期派生与封榜语义、tie-break 全序（rank / position）、幂等与限频、声明式结榜发奖、console 与运维。

## 1. 模型

**术语中立**：被排名的主体是 `subject_id`（不透明字符串 ≤64；client 面恒为当前登录用户，server 面可为战队 / 商家等任意 ID——战队榜今天就能用，缺的只是 client 面"替战队提交"的成员校权，是 v2 接缝）；值是 `value`（int64，小数域用放大整数约定：分 → 厘、秒 → 毫秒）。

**Board（榜）**——project 内配置对象（console CRUD，每项目 ≤100）：

| 字段 | 取值 / 缺省 | 说明 |
|---|---|---|
| `id` | `^[a-z_][a-z0-9_]*$` ≤40 | 与 collection ID 规则对齐 |
| `sort` | `desc`（缺省）/ `asc` | 主值方向 |
| `tiebreak_order` | 空（缺省）/ `asc` / `desc` | 声明单列 tiebreak；声明后 submit 必带 `tiebreak_value`（int64，可为 0） |
| `tie_break` | `parallel`（缺省）/ `earliest` / `latest` | 并列裁决（§3.2） |
| `period.kind` | `none`（缺省）/ `daily` / `weekly` / `monthly` | period_key 服务端按 tz 派生（`YYYYMMDD` / ISO 周 `YYYY-Www` / `YYYYMM` / `all`） |
| `period.tz` | IANA 名 | 期边界所在时区；填非法名拒收，期型 ≠ none 且留空按 UTC 派生 |
| `policy` | `best`（缺省）/ `latest` / `sum` | `best` 方向跟随 sort（desc → 取大、asc → 取小，时间 / 延迟榜可用） |
| `value_min/max` | 可选 | 越界**拒收**（不 clamp）；硬上限 ±(2^53−1) |
| `client_submit` | `false`（缺省） | false 时终端用户提交 `PERMISSION_DENIED` |
| `per_subject_submit_limit` | 100（缺省），1..10000 | 每期每主体提交次数（受理即计数，含 best 策略下的同分重放） |
| `retention_periods` | 0 = 永久（缺省，不默认删数据） | 保留最近 N 期，worker 每 30min 清理更早的期 |
| `subject_kind` | `"user"`（缺省），≤32 字符 | 纯展示提示（console 是否链到用户详情），无语义 |
| `rewards[]` | 空（缺省） | 声明式奖励规则（§6）；≤20 条 |

**Entry（条目）**：`(board_id, period_key, subject_id, value, tiebreak_value, submit_count, created_at, updated_at)`，表主键即模型内建去重——一主体一期一条。

**可变性**：`period.kind / period.tz / sort / tiebreak_order / tie_break / policy` 在榜内已有条目后不可改（`FAILED_PRECONDITION`；封榜前才允许调整排序语义）；value 边界（存量不回溯）、`client_submit`、限频、retention、`subject_kind`、`rewards` 随时可改。

## 2. API

三个 gRPC 服务面共享响应载荷（`shared.v1.LeaderboardScoreSnapshot` / `ListLeaderboardTopResponse`；HTTP JSON 为 snake_case）：

```
# client 面（终端用户 session；subject 恒从会话派生，请求无 subject 字段）
POST /v1/leaderboards/{boardId}:submit            body: { value, tiebreak_value?, period?, request_id? }
GET  /v1/leaderboards/{boardId}/me?period=
GET  /v1/leaderboards/{boardId}/top?period=&page_size=&page_token=

# server 面（API key；可代任意 subject）
POST /v1/server/leaderboards/{boardId}:submit     body: { subject_id, value, tiebreak_value?, period?, request_id? }
GET  /v1/server/leaderboards/{boardId}/entries/{subjectId}?period=
GET  /v1/server/leaderboards/{boardId}/top?period=&page_size=&page_token=
GET  /v1/server/leaderboards/{boardId}/settlements/{period}        # 结算 + 发放明细（只读）
GET  /v1/server/leaderboards/{boardId}/settlements?limit=

# console 面（admin session；读写动词见 §7 角色矩阵）
POST   /v1/console/leaderboards/boards
GET    /v1/console/leaderboards/boards
GET    /v1/console/leaderboards/boards/{boardId}
PATCH  /v1/console/leaderboards/boards/{boardId}
DELETE /v1/console/leaderboards/boards/{boardId}                   # 条目经 FK 级联删
GET    /v1/console/leaderboards/boards/{boardId}/periods?limit=
GET    /v1/console/leaderboards/boards/{boardId}/top?period=&page_size=&page_token=
GET    /v1/console/leaderboards/boards/{boardId}/entries/{subjectId}?period=
DELETE /v1/console/leaderboards/boards/{boardId}/entries/{subjectId}?period=   # 仅未封榜期
GET    /v1/console/leaderboards/boards/{boardId}/settlements
GET    /v1/console/leaderboards/boards/{boardId}/settlements/{period}
POST   /v1/console/leaderboards/boards/{boardId}/settlements/{period}:void
POST   /v1/console/leaderboards/boards/{boardId}/settlements/{period}:rerun
```

submit 响应 = `{ board_id, period, total, entry, rank, position, below }`；`me` 无条目时 entry / rank / position / below 缺省、total 仍返回（冷启动"已有 N 人参与"场景）。top 响应行 = `{ subject_id, value, tiebreak_value?, rank, position, updated_at }`；分页 `page_size` 缺省 50、上限 100，`page_token` 为 offset 编码，最深 offset 10000。`periods` 与 `settlements` 列表的 `limit` ≤200、缺省 50（period key / 结算行均按期降序）。

**board 配置管控**（server 面）：预置与门禁自动化的控制面——写动词走 **`leaderboards.admin`**（与 `leaderboards.write` 刻意分离：能提交分值的密钥不得改榜配置），读动词走 `leaderboards.read`：

```
POST  /v1/server/leaderboards/boards                        # CreateLeaderboardBoard（幂等建档）
GET   /v1/server/leaderboards/boards/{boardId}              # GetLeaderboardBoard（配置全文——发布门禁主读点）
GET   /v1/server/leaderboards/boards                        # ListLeaderboardBoards（不分页，每项目 ≤100）
GET   /v1/server/leaderboards/boards/{boardId}/periods      # ListLeaderboardBoardPeriods（对账期存在性）
PATCH /v1/server/leaderboards/boards/{boardId}              # UpdateLeaderboardBoard（proto3 optional = 不修改；clear_* 显式清空）
```

- **Create 幂等**：已存在且配置逐字段相等（缺省归一后）→ 200 + 现状（脚本可安全重放）；不等 → `ALREADY_EXISTS`，错误消息附字段 diff——console 或脚本任一侧改动 desired 字段都会被对端发现（漂移探测器）。比较集不含 rewards 与时间戳。
- **Delete 不进 server 面**：条目级联删的破坏性操作留给 console owner。
- **rewards 只在 console 面 Create / Update 消息内**：奖励规则编辑仅 console owner（admin 钥不得间接获得发奖编排能力）；server 面与 console 面的 Get / List 均只读透出 rewards。
- **上限**：每项目 ≤100 榜（console 与 server 两条写路径机械把守，超出 `RESOURCE_EXHAUSTED`）；满员时重放既有榜仍成功（重放判定先于上限）。
- 不可变字段护栏与 console 共用同一 domain 校验。
- **错误码**：`NOT_FOUND`（board / entry / settlement 不存在）、`INVALID_ARGUMENT`（分数越界 / period 格式错或不在提交窗口 / tiebreak 存在性不匹配 / 幂等键冲突）、`PERMISSION_DENIED`（client 提交未开 `client_submit` 的榜）、`RESOURCE_EXHAUSTED`（超每主体限频 / 榜数超限）、`ALREADY_EXISTS`（建榜冲突）、`FAILED_PRECONDITION`（不可变字段、settled 期 void、voided 期重跑）。
- **request_id**：`(project, actor, request_id)` 24h 幂等（复用 documents 写幂等的 `IdempotencyStore` 端口语义；`request_id` ≤191）——网络层重试返回首次快照（`replayed`），不重复烧限频额度；同 key 不同载荷 = `INVALID_ARGUMENT`（KEY_CONFLICT）；同 key in-flight 短轮询 2s，超时 `ABORTED`。request_id 缺省或无稳定归因身份时直接执行、不缓存。
- **Scopes**：`leaderboards.read` / `leaderboards.write`（提交与读）/ `leaderboards.admin`（board 配置写——平台首个配置面方向 op，按资源 opt-in，见 `05-authentication.md` §3）。
- **SDK**：TS `tw.leaderboards`（client：`submitLeaderboardScore(boardId, input)` / `getMyLeaderboardEntry(boardId, period?)` / `listLeaderboardTop(boardId, params?)`）与 `tw.server.leaderboards`（10 个方法覆盖 server 面全部 RPC，可代任意 subject）。Go SDK `sdk/go/server` 的 `LeaderboardsService` 同构。CLI 一等命令组 `torchwood leaderboards`（submit / get-entry / top / settlements get|list / boards create|get|list|periods|update；`torchwood rpc` 逃生舱兜底）。

## 3. 语义

### 3.1 期、宽限与封榜

- 提交窗口：`period` 缺省 = 当前期；显式必须命中 **{当前期, 上一期}**（离线补传宽限），其余（未来 / 更早）一律 `INVALID_ARGUMENT`——**封榜即终局，对所有人（含 server 面）一致**，不存在"运营补录任意旧期"的通道（历史迁移将来走 console 导入工具）。
- **封榜时刻 = 当前期变成 N+2 的瞬间**（即宽限自然到期）。seal 后条目不可变（含 console 删条目——作弊处理窗口就是宽限期）。
- 奖励因此晚一个期长到达（daily 榜 = 期结束后 24h）——"补传也算分"的公平性换延迟，结榜发奖（§6）依赖这一不变量。
- 读路径不受窗口限制：任意保留期内 `me` / `top` / `entries` 可读（分享卡复走历史期）。

### 3.2 rank / position / below 与并列

- `rank` = **competition ranking**（1,2,2,4；并列同名次）——展示语义。
- `position` = 全序位次（并列也不同）——机制语义，top 分页稳定与"谁紧挨在我前面"用它。全序键栈：`value`（按 sort）→ 可选 `tiebreak_value`（按声明方向）→ `updated_at`（earliest / parallel 缺省升序，latest 降序）→ `subject_id`（终极兜底，全序必然分出）。
- `tie_break` 三态：`parallel`（并列等同，奖励边界按 rank 含端点——两个并列第 10 都获 top10 奖；并列内展示顺序不承诺语义）；`earliest` / `latest`（先 / 后达到者靠前，奖励边界按 position 截断）。
- `below` = value 严格劣于我的人数（百分位分子，**不受 tie-break 影响**：并列既不算击败也不算被击败）。消费方用 `below/total` 显示"击败 X%"。
- `policy=best` 仅在 (value, tiebreak_value) 字典序严格变优时推进 `updated_at`——重复提交不污染"最早达成"语义；`latest` / `sum` 每次受理都推进。

### 3.3 读一致性与并发

submit 响应中的 rank / total / below 与写入同事务快照；并发提交可能各自看到同一 total（分位是建议性读数）。并发正确性由唯一键 + 行锁保证：N 个不同主体并发提交后 `total == N`（集成测试覆盖）。同键并发首插经唯一约束仲裁，落败方整体重开事务走合并路径（最多补偿 3 次，仍冲突返回 `ABORTED`）。

## 4. 存储

项目数据面 projectschema 三段迁移：`000019` 建 `leaderboard_boards` + `leaderboard_entries`（FK 级联删），`000020` 建 `leaderboard_settlements` + `leaderboard_settlement_grants`，`000021` 给 boards 补 `rewards JSONB`。条目复合索引 `(project_id, board_id, period_key, value DESC, tiebreak_value ASC, updated_at ASC, subject_id ASC)` 服务全部读路径：rank / total / below 是一条 `count(*) FILTER` 单次扫描；top 用 `RANK()`（competition）+ `ROW_NUMBER()`（position）窗口。索引方向取最常见组合（desc + asc + earliest），其余方向组合由 planner 增量排序——v1 量级（单期 ≤10 万行）毫秒级，索引策略对 API 不可见，可后调。量级参考：DAU 1 万 × 365 天 ≈ 365 万条/年/榜，retention 90 期稳态约 90 万条。

## 5. 非目标

滚动窗口（非对齐时间窗，另一种数据结构）、多字段任意 tiebreak（需按 board 生成表达式索引）、条目实时事件流、反作弊检测（信任模型 = 客户端自报 + 声明式区间拒收）、`around` 邻域查询（offset token 的 keyset 升级是预留接缝）、战队成员拆分发放。season / 手动期 / 自定义 interval 是 v2 接缝：存储只见不透明 `period_key`，扩展只是新增推导函数。

## 6. 结榜发奖（声明式 rewards）

**声明式 rewards**（board 配置，仅 console 编辑）：每条规则 = 名次区间（`rank_min` / `rank_max`，含端点，可只声明一侧）+ `value_min` 门槛 + `asset_code` + `amount`（> 0）；两类判据至少其一，多规则独立评估、可叠加命中；≤20 条；要求 `period.kind != none`（全期榜无封榜时点），保存时校验 asset def 存在（封榜前被删则结算明细落 `failed` 可见，重跑前修复 def 即可）。边界语义跟随 `tie_break`：`parallel` → rank 含端点（并列第 rank_max 也发）；`earliest/latest` → position 截断（先达到者占位）。单规则获奖者硬上限 100000。

**结算执行**（worker `leaderboards-settler`，10min ticker，单轮预算 5min）：扫描 **active 项目 × 配置了 rewards 的榜**（SQL 侧 `jsonb_array_length(rewards) > 0` 预过滤）× **已封榜（period_key < 上一期）且无结算行的期**——基于状态而非定时投放，停机积压自动补算；单轮单榜最多认领 50 期，剩余下轮继续。逐期流程：

- 原子认领（结算行唯一键 `(board_id, period_key)` 仲裁）→ 规则快照冻结进 `leaderboard_settlements.rules_snapshot`（认领时点，后续改榜配置不影响本期）。
- 逐规则以窗口函数算获奖者，展开为发放明细行（`pending`）。
- 逐 `(rule, subject)` 以派生幂等键 `lbsettle:{board}:{period}:{rule_index}:{subject}` 走 Assets Grant（system 主体，ledger `ref_type=leaderboard_settlement`，幂等键经 Assets 项目级全局键仲裁）——at-least-once 执行、exactly-once 效果；对账复用 Assets Reconcile（发放即普通 ledger 行）。
- **单笔失败不阻断整期**：失败明细记 `failed`（含错误），期照常 `settled`；console 重跑只补发非 granted 明细（幂等键保证不双发），全部已发放时为幂等空操作。
- 结算行不随条目 retention 清理（钱的记录不走数据保留策略）。

状态机 `settling → settled`（`error` 仅基础设施级失败，可重跑；旁路 `voided`——运营弃奖）。发放明细状态 `pending | granted | failed | voided`。

- **重跑（rerun）**：settled / error → settling，补发非 granted 明细；voided 拒绝（弃奖不可复活）。
- **弃奖（void）**：settling / error → voided（未发放明细标 voided）；settled 拒绝——已发放只能手工 consume 追回；无结算行可直接预落 voided（抢先冻结本期，结算不再认领）。
- **奖励延迟 = 一个宽限期**（daily 榜期结束后 24h + 至多一个扫描周期）——"补传也算分"的公平性。
- 作弊窗口 = 宽限期：seal 前 console 删条目即可；seal 后只能 void（未发放部分）或手工 consume 追回（已发放）。
- Prometheus 指标：`torchwood_leaderboards_settle_backlog_periods`（待结算期数下界 gauge）与 `torchwood_leaderboards_settle_failures_total`（失败期数 counter）；项目 / 榜维度归因走结构化日志。
- `on_settled` 函数触发（通知 / 自定义逻辑逃生通道）是未落地的 v1.5 接缝。

**面**：console（结算列表 / 明细 / void / 重跑 + 建榜 / 编辑表单的 rewards 编辑器）；server 面只读 `GetLeaderboardSettlement` / `ListLeaderboardSettlements`（`leaderboards.read`）。写操作只有 worker 与 console。

## 7. Console 与 server 管控面

`/console/leaderboards`：榜列表 + 建榜 / 编辑（可变字段 + rewards 规则编辑器）对话框 + 删除（条目级联，二次确认）；榜详情页：期下拉 + top 表（rank / position / subject / value / updated_at，行删条目——仅未封榜期）+ 按 subject 查条目（含 submit_count）+ 结算区（列表 / 明细 / void / rerun）。写操作仅 owner 角色（console 面 `permissions:["owner"]`，读 `["owner","admin"]`）。

server 面的 board 配置管控（建 / 改 / 读，§2）以 `leaderboards.admin` API key 承载，面向预置脚本与发布门禁自动化；console 与 server 两条写路径共用同一 domain 校验与审计，能力不互相回退。

## 8. 审计与运维

- 审计：client 面提交不落审计行（高频日常操作，对齐噪声治理）；server 面提交、server 面 board 管控写动词与 console 写操作自动落审计（含成功与被拒的变更尝试，带 API key 归因与脱敏请求摘要）。
- worker 12 个常驻组件中 leaderboards 占两个：`leaderboards-settler`（10min，结榜发奖扫描，§6）与 `leaderboards-cleaner`（30min，retention 清理——默认 0 = 永久，只有显式配置的榜被清；失败仅告警下一轮重试，清理按期整删、幂等）。两者单轮均带 5min 超时预算。

## 相关文档

- `05-authentication.md` §3 — scope 方向与 `leaderboards.admin` 的 opt-in 语义
- `12-sdk.md` — SDK 方法映射
- `13-operations.md` §1.2 — worker 常驻作业清单
