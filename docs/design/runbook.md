# Runbook：版本化资源迁移（类 sql migrate 的 up/down）

> 状态：**已实施（2026-09-14，五阶段全部完成并通过终验：全量测试绿 + 真机 RPC 链路验证）**
> 相关代码：`proto/server/v1/runbook.proto`、`internal/domain/runbook/`、`internal/app/server/runbook.go`、`internal/infra/bun/{model,bunrepo}/runbook.go`、`internal/api/servergrpc/runbook.go`、`internal/pkg/runbook/`（引擎）+ `cli/runbook.go`（命令组）、`sdk/go/server/runbook.go`、`db/migrations/000009_runbook_steps.*`、`docs/developer/19-runbook.md`（使用者文档）
> 相关现状：`sdk/go/server/invoke.go`（InvokeJSON 动态通道，CLI 唯一 RPC 通路）、`internal/app/leaderboards/provision.go`（幂等 provisioning 先例）、`db/migrations/`（golang-migrate 控制面迁移）、`docs/developer/12-sdk.md` §CLI

---

## 1. 现状与问题

Torchwood 的资源（DocumentDB 库/集合/属性/索引、leaderboard board、storage bucket、asset def 等）目前只能命令式创建：Console 手工点、CLI 手敲、或 ad-hoc 脚本。资源的形状没有版本化事实源——新环境不可复现、资源变更无法审阅、环境间漂移只能靠 `admin schema repair` 事后对账（那是对账 catalog vs 物理表，不是意图 vs 现状）。控制面 SQL 已有 golang-migrate 的成熟心智（`NNNNNN_name.up.sql/.down.sql` + `schema_migrations`），业务资源层没有对应物。

**目标**：把"创建 configs 集合""新建 gold 资产"这类资源变更写成有序号、可审阅、可回退的文本文件，由引擎按版本顺序驱动 Server API 执行，状态记录在服务端供多机/CI 共享。初期唯一驱动入口是 `torchwood` CLI。

**成功标准**：

1. 新项目 + 一份 `runbooks/` 目录 + 一把 API key，`torchwood runbook up` 一步拉起全部资源；
2. 任意环境可重复执行 `up` 而结果一致（幂等）；
3. 已应用的迁移文件被改动时 up 拒绝执行；
4. 两台机器并发 up 只有一台生效；
5. 带 down 段的 step 可逐版回退。

**约束**：CLI 不 import genproto/grpc（`import_guard_test` 兜底），一切动作经 `sdk/go/server` 的 `InvokeJSON` 动态通道；新 RPC 走仓库标准流程（proto + method_auth + swagger 一致性断言 + SDK typed wrapper 覆盖测试）。

## 2. 目标设计

### 2.1 用户视角：文件与命令

文件放项目仓库 `runbooks/` 目录（`--dir` 可覆盖），命名 `NNNNNN_name.yaml`（6 位零填充序号，name 匹配 `^[a-z0-9_]{1,64}$`，仅认 `.yaml` 后缀）。**单文件双段**：

```yaml
# runbooks/000001_create_configs_collection.yaml
up:
  - create_collection:
      database_id: app
      id: configs            # 字段面与 proto 一一对应：Create 系请求用 id，Get/Update/Delete 系用 collection_id
      name: configs
      document_security: true
      permissions: ['read:users']   # 权限串格式 type:role（同 ParsePermissionStrings），write: 自动展开为 create/update/delete
      attributes:
        - { key: key, type: string, size: 64, required: true }
        - { key: value, type: json }
      indexes:
        - { id: key, type: unique, attributes: [key] }
down:
  - delete_collection:
      database_id: app
      collection_id: configs
```

```yaml
# runbooks/000002_create_gold_asset.yaml
up:
  - create_asset_def:
      code: gold
      name: Gold
      class: currency       # string 词表字段按服务端词表小写值（DB CHECK: currency/stack/instance/entitlement），非 proto 枚举
      decimals: 2
      max_quantity: 1000000
down:
  - delete_asset_def:
      code: gold          # 服务端语义为归档（soft），引擎复活语义见 D10
```

动作动词是扁平的、与 Server RPC 一一映射的键（`create_collection` → `/torchwood.server.v1.DatabasesService/CreateCollection`），请求体字段 = 对应 `*Request` 消息的 proto 字段（**规范写法为 proto 原名 snake_case**，protojson Unmarshal 兼容 camelCase——实现口径，"proto 即文档"），引擎直传 `InvokeJSON`。`attributes`/`indexes` 是唯一复合动词：`create_collection` 引擎自动拆为 `CreateCollection → 逐个 CreateAttribute → 逐个 CreateIndex`。

命令（标准 group，挂全局旗标，需 API key）：

```
torchwood runbook up      [--dir runbooks] [--to N] [--dry-run] [--quiet]   # 应用到最新/指定版
torchwood runbook down    [--dir runbooks] [--to N] [--all] [--dry-run] [--quiet]  # 默认回退一步
torchwood runbook status  [--dir runbooks]                                   # 本地文件 vs 服务端状态对账
torchwood runbook new     <name> [--dir runbooks]                            # 生成下一序号骨架（本地命令，免 key）
torchwood runbook forgive <version> [--dir runbooks]                         # 逃生门：删除 ≥N 的服务端记录，不动资源（D21）
```

输出契约：进度行进 **stderr**（`[runbook] applying 000002_create_gold_asset: create_asset_def(gold) ... ok`），**stdout 始终是单个 JSON summary**（`{runbook, current_version, applied: [{version, name, actions}], duration_ms}`），不破坏 CLI 的 JSON-only 约定。

### 2.2 状态模型与 RunbookService

状态存服务端（本地文件多机/CI 必然分叉；golang-migrate 的 `schema_migrations` 也在库里）。新增薄状态服务——只记录事实，不执行动作：

```proto
// proto/server/v1/runbook.proto（字段级草案）
service RunbookService {          // service_auth: default_access ACCESS_SERVER（与 DatabasesService 同）
  rpc GetRunbookState(GetRunbookStateRequest) returns (GetRunbookStateResponse) {
    // method_auth: api_key_scope { resource: RUNBOOKS, op: SCOPE_OP_READ }
  };
  rpc RecordRunbookStep(RecordRunbookStepRequest) returns (RecordRunbookStepResponse) {
    // api_key_scope { resource: RUNBOOKS, op: SCOPE_OP_WRITE }
  };
  rpc DeleteRunbookStep(DeleteRunbookStepRequest) returns (shared.v1.Empty) {
    // api_key_scope { resource: RUNBOOKS, op: SCOPE_OP_WRITE }
  };
}
message RunbookStepState { int64 version = 1; string name = 2; string checksum = 3; google.protobuf.Timestamp applied_at = 4; }
message GetRunbookStateRequest { string runbook = 1 [(buf.validate.field).string.max_len = 64]; }
message GetRunbookStateResponse { repeated RunbookStepState steps = 1; }   // 升序全集
message RecordRunbookStepRequest {
  string runbook = 1; int64 version = 2 [(buf.validate.field).int64.gte = 1];   // 实现修正：设计初稿 name/version 撞号 2，按寻址键优先重排
  string name = 3; string checksum = 4;                                       // checksum ^[0-9a-f]{64}$
  optional int64 expect_prev_version = 5;   // CAS：absent/0 = 首步；不等当前顶版 → FailedPrecondition
}
message RecordRunbookStepResponse { int64 current_version = 1; }
message DeleteRunbookStepRequest { string runbook = 1; int64 version = 2; } // 必须等于当前顶版
```

authz 注解补充（实现期档位断言的强制要求）：Record/Delete 两个写方法在 `api_key_scope { RUNBOOKS, WRITE }` 之外必须同时声明 `admin_roles: [ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]`（与 leaderboards 写方法同档位；member 不可触碰迁移历史）。

控制面迁移 `db/migrations/000009_runbook_steps.{up,down}.sql`：

```sql
CREATE TABLE runbook_steps (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  runbook TEXT NOT NULL,
  version BIGINT NOT NULL,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, runbook, version)      -- 并发兜底：双跑只剩一个赢家
);
```

分层落位：端口 `internal/domain/runbook/`（`StepState` + `StateRepo`），用例 `internal/app/server/runbook.go`（CAS/顶版校验在此），适配器 `internal/infra/bun/bunrepo/runbook.go`，handler `internal/api/servergrpc/runbook.go`。`proto/shared/v1/authz.proto` 的 `ScopeResource` 增加 `RUNBOOKS`。审计、authz-matrix、swagger 一致性断言随标准流程自动纳入。

### 2.3 引擎算法（CLI 侧）

```
共同前置（up/down/status 都做）：
  files = 加载 runbooks/*.yaml → 严格解析（yaml.v3 KnownFields，未知动词/未知字段即 fail）
          版本链校验：从 1 起、连续、无重号；checksum = sha256(文件原始字节)
  state = GetRunbookState("default")
  对账：服务端每个已应用版本必须在本地存在且 checksum 相等，否则 fail
        （文件被删/被改历史 → 拒绝执行，指引用 git 恢复）

up：
  target = --to N 或最大版本
  for v in (state.max+1 .. target):          # 强制顺序，禁止跳版
    for action in files[v].up（声明顺序）:
      reconcile(action)                        # 幂等执行，见 2.4
    RecordRunbookStep(v, checksum, expect_prev=state.max)
    # CAS 撞（并发方已记录）→ 重拉状态校验该版 checksum：相等 = 对方做了同样的事，继续下一版；不等 = fatal（文件分叉）
    进度行 → stderr
  stdout = summary JSON

down：
  target = --to N（须 < state.max）| 默认 state.max-1 | --all → 0
  for v in (state.max .. target+1) 降序:
    if files[v].down 为空 → fail: "版本 N 不可逆（down 段为空）"
    for action in files[v].down: reconcile(action)
    DeleteRunbookStep(v)   # 顶版校验撞（并发方已摘）→ 重拉状态重评估，down 语义幂等自然继续
```

**reconcile（幂等核心，每个动作先读后写）**：

| 情形 | 行为 |
|---|---|
| create：资源不存在 | 调 Create，继续 |
| create：已存在且配置相等（D12 白名单归一后） | skip，输出 drift-free |
| create：已存在但不等 | **fail**，错误信息含字段级 diff（期望 vs 实际），指引"写新 step 对齐或手工修复"——不自动 update，维持"迁移文件是不可变历史"的纪律 |
| create：执行时撞 AlreadyExists（并发双跑或手工同时创建的 TOCTOU 窗口；"先读后建"不是原子检查） | 重读现状再比较：相等 → skip；不等 → fail 带 diff。board 无此分支（服务端原生幂等，AlreadyExists 自带 diff） |
| delete：资源不存在 / 已是目标态 / 执行时撞 NotFound | skip——**NotFound 一律按成功处理，不上抛**（服务端 Delete 前的存在性校验与并发删除都可能产生） |
| `create_collection` 特例 | 声明的 attrs/indexes 视为**全集**：线上缺的逐个补建（支持半应用后重入），线上多出或配置不等 → fail |

归一化比较按各资源 proto 缺省语义（`internal/app/leaderboards/provision.go` 的字段归一是仓库现成先例），每资源一个 normalize + compare 函数，fixture 测试钉死。失败即停整个 up，状态不记录该版本——**重跑即恢复**，因此不需要 golang-migrate 的 dirty 标记。

### 2.4 动作动词表（初期 18 个，全部薄映射）

| 资源 | 动词 | 声明引用键 | 读取路径（reconcile 用） | down 可逆性 |
|---|---|---|---|---|
| database | create / delete | `id` | GetDatabase | 互逆 |
| collection | create（含内嵌 attrs/indexes）/ update / delete | `database_id`+`collection_id` | GetCollection（回读完整形状） | delete 级联硬删，可逆 |
| attribute | create / delete / restore | db+coll+`key` | GetCollection 回读 | delete 是软删，restore 可逆 |
| index | create / delete | db+coll+`id` | GetCollection 回读 | 互逆 |
| leaderboard board | create / update | board `id` | GetLeaderboardBoard；**create 直传即幂等**（服务端原生 provisioning：相等 200 / 不等 409 带 diff） | 服务端无 Delete → down 不可逆 |
| bucket | create / update / delete | **`name`**（ID 是服务端 UUID） | ListBuckets 按 name 过滤解析 id（多命中 fail） | 互逆 |
| asset def | create / update / delete | **`code`**（ID 服务端生成） | ListAssetDefs 按 code 反查 def_id | delete = 归档软删；重建靠 D10 复活，循环闭合 |

update 动词的请求体用 YAML 字段显式存在性表达 proto3 optional 的 presence（写了 = 修改，没写 = 不动），与 CLI `v.changed()` 同语义；**键存在但值为 `null` 视为解析期错误**（protojson 中 null 不是合法的 presence 表达），显式写值才算设置。update 天然幂等（设置同值无害），reconcile 直接执行不做比较。

**attribute 生命周期状态参与 reconcile**（`GetCollection` 回读的 `Attribute.status`）：

- `active` → 存在，走配置比较（D12 白名单）；
- `deprecated` 且声明需要该 key → `RestoreAttribute`（复活语义，直接 `CreateAttribute` 会撞 key）；
- `retired`（物理列已删）→ 视为不存在，正常 `CreateAttribute`；
- `migrating` → 比较判定 fail，提示"属性迁移进行中，完成后重跑"（中间态不可判定，不猜）。

**asset def 归档语义**（`asset_defs` 的 `UNIQUE (project_id, code)` 不分 status，归档行占位 code；`UpdateAssetDefRequest` 带 `optional string status`）：`create_asset_def` 遇同 code 归档 def——配置相等（除 status）→ 自动复活（`UpdateAssetDef(status=active)`）+ 继续；不等 → fail 带 diff。归档是逻辑删除，复活是其逆；holdings/ledger 随 def 保留正是 soft 语义的正确行为。

## 3. 关键决策及理由

### 主决策（D1–D9）

| # | 分叉点 | 裁决 | 理由（含否掉的选项） |
|---|---|---|---|
| D1 | 命令式 vs 声明式 | **命令式版本迁移**（sql migrate 心智），不做 Terraform 式收敛 | 命令式审计粒度自然（一个 step = 一次变更）；声明式的 drift 自动修复在"Create 均非幂等 + attr 软删/Retire 两段式"的语义下容易做出静默破坏。否掉声明式：它要求全资源可信 diff，当前 API 面撑不住。 |
| D2 | 状态存哪 | **服务端新薄 RPC + 控制面表** | 否掉本地文件（多机/CI 分叉，与"版本化"初衷矛盾）；否掉直连 DB（admin 先例是 owner DSN，CI 下发 owner 凭证不可接受，且绕过 authz/审计）；否掉存在业务 collection（鸡生蛋：v1 建 collection 前状态得先有落点，且污染用户数据面）。3 个 RPC + 1 张表是仓库最熟练的增量路径，并为将来 server-driven runner（CI webhook）预留同一状态面。 |
| D3 | 引擎放哪 | **CLI 侧**，服务端只存状态 | 引擎 = 文件解析 + 读后写编排，全是客户端逻辑；上移服务端是将来加新驱动者时的事，状态模型不变。 |
| D4 | 文件形态 | **单文件双段**（`up:`/`down:`），非 migrate 式双文件 | down 段通常一两行；单文件保证 up/down 在同一个 git diff 里被审阅、不会"改了 up 忘了 down"。**down 段为空 = 不可逆**，不引入额外 `irreversible` 字段——空段即语义，少一个实体。 |
| D5 | 动作语言 | **扁平动词、与 RPC 一一映射、请求体直传 protojson** | 否掉嵌套资源 DSL：多一层发明语法，调试时仍要落到 RPC 形状；扁平动词与 CLI/SDK/OpenAPI 词汇一致，proto 即文档。唯一复合 `create_collection` 内嵌 attrs/indexes，因为逐条写三个动作建一张表的用户体验不可接受。 |
| D6 | create 遇已存在 | **相等 skip / 不等 fail，永不自动 update** | 迁移纪律：已应用的历史不可变，配置演进必须走新 step（显式 update 动作）。自动 update 会把"手滑改错的文件"静默推平线上。同时否掉"服务端全面幂等化"（把 board 式 provisioning 推广到 7 个资源域）：改动面过大且破坏既有 AlreadyExists API 契约，收敛逻辑收在引擎一处更小。 |
| D7 | step 原子性 | **无跨 RPC 事务，靠动作幂等 + 失败重跑** | 资源动作分布在多个服务用例里，无法包成一个事务（这是与 sql migrate 的本质差异，用户文档红字）。半应用状态由 reconcile 的"先读后建/缺者补建"安全重入。失败 step 不记录版本 → 重跑从头执行该 step。 |
| D8 | 并发防护 | **CAS（expect_prev_version）+ 唯一约束兜底** | 两台 CI 同时 up：后到者 Record 时 prev 不匹配 → FailedPrecondition → 重新拉状态评估后自然收敛。不下 DB 锁，无长事务。 |
| D9 | 历史被改 | **checksum 对账，不等即拒绝**。checksum = sha256(归一化后文件字节)：计算前 CRLF→LF 归一 + 去 UTF-8 BOM（Windows 编辑器实场景；本仓库 Windows 开发 + git autocrlf 下原始字节哈希会跨环境误报，Atlas FAQ 的实坑）；建议仓库 `.gitattributes` 标 `runbooks/*.yaml text eol=lf`，但引擎不依赖该建议（归一化保证）。不做 Atlas `atlas.sum` 式目录级 Merkle 文件——它解决本地目录防篡改，我们的状态与 checksum 存服务端，覆盖其场景 | 资源迁移的文件就是逻辑本身，改历史 = 环境分叉之源。注释改动也触发——这是特性不是缺陷，指引"恢复文件 / `forgive` 承认变更 / 写新 step"（业界对照：Prisma 无逃生门导致用户手改 `_prisma_migrations` 表，见修订记录 2026-09-14 竞品调研）。 |

### 补充裁决（D10–D20，实现者自检补全）

| # | 分叉点 | 裁决 | 理由 |
|---|---|---|---|
| D10 | `create_asset_def` 遇同 code 归档 def | 配置相等（除 status）→ 自动复活；不等 → fail 带 diff | down(归档)→再 up 若不复活就死锁（重跑永远撞归档行）。归档是逻辑删除，复活是其逆；否掉"fail 指引人工清理"：把循环闭合交给 DBA 违背自动化初衷。 |
| D11 | attribute 生命周期状态 | active=比较；deprecated+需要→RestoreAttribute；retired=不存在正常建；migrating→fail 提示稍后重跑 | deprecated 直接 Create 会撞 key（有 RestoreAttribute 专门复活）；migrating 是中间态，不可判定时不猜。 |
| D12 | 相等比较字段白名单 | database: name；collection: name/permissions(排序)/document_security/disabled；attribute: type/size/required/array/default_value/dims；index: type/attributes/orders/distance_metric；bucket: Create/Update 可写字段全集；asset def: name/class/decimals/max_quantity/expires_in/tradable/unique_per_owner/upgradeable/metadata（status 单独走 D10）；board: 引擎不比较，透传服务端原生 diff | 服务端响应的元数据字段（created_at 等）一概不参与，防噪声误报 drift。比较函数返回结构化 diff（字段/期望/实际）直接进错误输出。 |
| D13 | 目录防御 | `--dir` 不存在或无合法 `.yaml` → fail（exit 1） | 否掉静默 no-op：打错路径的"成功"比失败危险。 |
| D14 | `--to` 边界 | up: N ≥ 当前（等于=no-op 退 0，CI 幂等）且存在于本地；down: N < 当前，`--all`=`--to 0`；越界 UsageError | — |
| D15 | 枚举写法 | YAML 一律写 proto 枚举名（真枚举字段），禁数字；**string 词表字段按服务端词表小写值**（如 asset `class: currency`——实现期核对：该字段是 string 非 enum，`CLASS_CURRENCY` 大写形会被服务端矩阵校验拒收） | protojson 对真枚举两者都收，规范为名字——审阅可读性优先；string 字段无枚举名可写，只能按服务端词表。 |
| D16 | status 输出 | `{runbook, current_version, clean, files: [{version, name, state, irreversible}]}`；state ∈ applied/pending/modified(checksum 不符)/orphan(服务端有本地无)；恒退 0 | status 是报告不是门禁；CI 拿 `up` 做门禁（遇 modified/orphan 本来就 fail）。 |
| D17 | `runbook new` | 序号 = 本地最大 + 1（与服务端状态无关）；`--dir` 不存在则创建；骨架含注释模板与不可逆提示 | — |
| D18 | 中断语义 | 动作间 Ctrl-C → 未 Record → 重跑收敛，引擎不做信号补偿 | — |
| D19 | 与 `admin schema repair` 边界 | repair 对账"catalog vs 物理表"（存储层漂移）；runbook 对账"声明文件 vs API 现状"（意图层漂移） | 两层互补不重叠，两份文档互相引用。 |
| D20 | permissions 比较 | 排序归一后比较；实现期核对 `internal/domain/databases/permissions.go` 补两条归一：`write:role` 展开为 create/update/delete 三条后再比；声明侧缺省/空列表跳过 permissions 比较（服务端对空声明赋默认权限集） | 权限数组顺序无语义；不镜像展开则双跑必误报漂移。 |
| D21 | checksum 拦截的逃生门 | **`runbook forgive <version>`**：从顶版起逐个 `DeleteRunbookStep` 删除 ≥N 的全部服务端记录，**不执行任何资源动作**；随后用户重跑 `up`，动作幂等 skip + Record 新 checksum | Prisma 的教训（issue #15328）：checksum 校验若无受控出路，用户被迫手改状态表——我们的状态表在服务端、API key 无表级访问权，没有逃生门就是死局。forgive 是 `runbooks.write` 权限内的有审计操作，滥用面可控；只从顶版逐个摘保证版本链单调。 |

## 4. 分步实施计划

| 步 | 内容 | 交付物 | 验证 |
|---|---|---|---|
| 1 | 状态服务端到端：authz.proto 加 `RUNBOOKS` → runbook.proto → `task generate:proto` → 迁移 000009 → domain/app/infra/handler → swagger → `task wire:all` | 可部署的服务面 | `task build`；app 单测（CAS、顶版校验、checksum 正则）；`migrations_cycle_test` 自动纳入 000009 的 up/down 循环 |
| 2 | SDK：`sdk/go/server/runbook.go` typed wrapper；`invoke_test.go` 计数 146→149 | SDK 面 | `go test ./sdk/go/server/`（两个覆盖测试自动断言） |
| 3 | CLI 文件层：`runbook_file.go`（解析/严格校验/版本链/checksum）+ `runbook new` | 本地可用的文件面 | 纯单测：合法/未知动词/未知字段/重号/跳号 fixtures |
| 4 | CLI 引擎：`runbook_engine.go`（reconciler + 归一化比较 + up/down 主循环 + status + forgive）+ `runbook.go` 命令组注册 | 完整功能 | 决策层单测（喂假状态断言 plan，含 forgive 摘链与 checksum 归一化 CRLF fixture）；testutil 集成：up→status→down→up 循环、双跑幂等、checksum 篡改拦截、中途失败重入 |
| 5 | 文档：`docs/developer/19-runbook.md`（含"动作不可原子回滚"红字）、authz-matrix 重生成、02-quickstart CLI 段落补一行 | 文档面 | `task gen:authz-matrix`；docs 索引更新 |

每步独立可合入；步 1–2 与步 3 无依赖可并行。

## 5. 风险与对策

| 风险 | 对策 |
|---|---|
| step 半应用（第 2/3 个动作失败） | 动作全部幂等（先读后建/删时 skip/缺者补建），文档明示"重跑 `runbook up` 即恢复"；无 dirty 标记必要 |
| down 误删生产数据（`delete_collection` 级联 DROP） | down 默认单步；`--dry-run` 先看；逐动作进度行；文档红字 |
| 线上手工 drift（console 改了资源） | create 相等 skip、不等 fail-fast 带 diff，指引写新 step 对齐——宁可停也不猜 |
| board 的 down 缺口（服务端无 Delete） | down 段为空 → 明确报"不可逆" |
| bucket 按 name 解析失败/重名 | List 过滤 0 命中 → 按不存在处理；多命中 → fail 人工裁决 |
| 改历史文件触发 checksum | 报错指路三条：`git checkout` 恢复 / `runbook forgive` 承认变更后重放 / 写新 step |
| 并发双跑 | D8 的 CAS + 唯一约束，输家自动收敛为 skip |
| 退出码契约 | 动作 RPC 错误走既有 `rpcExitCode`（2=40x/3=5xx/4=429）；本地文件错误=1；`--dry-run` 一律 0 |

回滚路径（功能级）：CLI 侧摘除 `root.go` 一行注册即退场；服务端 RPC 不被调用则闲置无副作用；彻底回退跑 000009 down.sql（DROP TABLE，ON DELETE CASCADE 随项目清理）。

## 6. 测试策略

- **文件层单测**：fixtures 覆盖合法双段、down 空段、未知动词、未知字段（KnownFields）、重号/跳号/非 6 位、`.yml` 拒收、checksum 稳定性。
- **决策层单测**：reconciler 喂"线上现状 vs 声明"的假数据，断言 create/skip/fail/补建四分支；每资源的归一化比较 fixture（缺省值两侧等价）；D10 复活、D11 四状态分支。
- **服务端单测**：Record 的 CAS 语义（prev 不匹配 → FailedPrecondition）、Delete 非顶版拒绝、跨项目隔离。
- **集成**（`internal/pkg/testutil`）：全链路 up → status（对账干净）→ down → up；`up` 连跑两遍零变更；v2 执行中模拟失败后重跑收敛；两 goroutine 并发 Record 只剩一赢家；**双引擎并发执行同一 create 动作（TOCTOU：一方 AlreadyExists 后重读收敛为 skip）**；Record CAS 撞后 checksum 相等继续 / 不等 fatal；board provisioning 相等 skip；asset def 归档→复活循环。
  实现期注：真 server 运行时装配在 `cmd/server/internal/runtime`（Go internal 可见性），CLI 测试内无法拉起——上列用例以"内存假 server（忠实镜像 CAS/顶版校验/权限展开/protojson 零值省略）+ caller 注入"的全链路单测实现；真机端到端（真 server + 真 API key）在阶段 E 手工补验。
- **存量护栏自动覆盖**：`migrations_cycle_test`（新迁移）、`invoke_test`（计数）、`grpc_swagger_test`（swagger/method_auth 一致性）、`import_guard_test`（CLI 不碰 genproto）。

## 7. 明确不做的事（初期）

- **function 资源动作**——deployments 是 zip 二进制，文本文件管不了；将来可考虑"本地路径引用归档"的形式，不在本期。
- **文档/数据迁移**——只管 schema 与配置资源面。
- **声明式收敛 / drift 自动修复**（Terraform 化）——D1 已否。
- **多 runbook 并行线**——表留 `runbook` 列（默认 `'default'`），CLI 不暴露分线参数。
- **env 变量替换 / 模板**——同文件异环境产出不同结果，与 checksum 环境无关性冲突。
- **server 端 runner**（定时执行 / CI webhook）——状态服务已为此预留，驱动者后续可加。
- **down 自动生成**——down 由作者手写（可审阅、可裁剪，如"down 保留数据只删索引"）。业界对照：Prisma 长期 roll-forward（down 无法安全逆转数据变更，v7 才 opt-in down），down 的正确预期是**资源形状回退，不承诺数据恢复**——文档总则与每动词注释均如此声明。
- **users/groups 等 IAM 资源、update_database**（服务端无此 RPC）。
- **auto-migrate 联动**（console 改动自动生成 runbook 文件）——PocketBase 的做法（Admin UI 改动自动落 `pb_migrations/`），远期可考虑"console 保存时提示未纳入 runbook / 导出为 step"，本期不做。

## 8. 改动面清单

新增：`proto/server/v1/runbook.proto`、`db/migrations/000009_runbook_steps.{up,down}.sql`、`internal/domain/runbook/`、`internal/app/server/runbook.go`、`internal/infra/bun/bunrepo/runbook.go`、`internal/api/servergrpc/runbook.go`、`sdk/go/server/runbook.go`、`internal/pkg/runbook/`（引擎 `runbook_file`/`runbook_engine`/`runbook_reconcile*`；初版位于 cli/，后移入业务共享内核）、`cli/runbook.go`（命令组）（+ 各 `_test.go`）、`docs/developer/19-runbook.md`。

修改：`proto/shared/v1/authz.proto`（RUNBOOKS）、`genproto/`（生成）、`sdk/go/server/invoke_test.go`（146→149）、`cmd/torchwood/app.go`（Register 一行；当时位于 `cmd/torchwood/cmd/root.go`）、`docs/developer/authz-matrix.md`（重生成）、docs 索引；`cmd/server/wire_gen.go`（`task wire:all`）。

---

修订记录：

- 2026-09-14：初稿（D1–D9 主决策 + D10–D20 实现者自检补全，全部拍板，无待定项）。
- 2026-09-14：竞品调研修订（PocketBase/Hasura/Appwrite/Amplify/Supabase/Prisma/Atlas 对照）：D9 增加 CRLF→LF 归一化（Atlas FAQ 实坑）并明确不做 atlas.sum；新增 D21 `runbook forgive` 逃生门（Prisma #15328 教训）；§7 补 down 数据恢复总则与 auto-migrate 远期备注。收敛点确认：状态存服务端、checksum 防篡改、版本链顺序应用、down 保守默认均为业界收敛；Amplify 尸检支撑 D1 命令式裁决。
- 2026-09-14：深度复查修订（视角：并发时序 TOCTOU / 安全与恶意输入 / 被否替代方案再攻击）：修正三处——reconcile create 补 AlreadyExists 收敛分支（先读后建非原子，并发双跑必经路径）、000009 DDL `project_id` BIGINT→TEXT（projects.id 实为 TEXT，000001:6）、checksum 归一化补去 BOM；补充 Record/Delete CAS 撞后的收敛分支、delete 动作 NotFound 一律按成功、YAML `null` 值解析期报错、D6 补"否掉服务端全面幂等化"理由、TOCTOU 并发集成用例。
- 2026-09-14：阶段 A 实现回写：§2.2 proto 草案字段号撞号修正（name 2→3、checksum 3→4、expect_prev 4→5）与写方法 admin_roles 档位补充（实现期 ClassifyTier 断言强制）；`internal/domain/auth/policy.go` 顺带修复 AllScopeResources 既有重复段。
- 2026-09-14：阶段 C 实现回写：§2.1 示例字段面修正（create_collection/create_index 动作用 `id` 而非 `collection_id`，与 proto Create 系请求一致；权限串格式实为 `type:role` 非函数式）；D20 补 write 展开与空声明跳过比较；§6 注明真 server 端到端受 internal 可见性限制，以假 server 全链路单测 + 阶段 E 真机手工验证替代。
- 2026-09-14：阶段 D 实现回写：§2.1 示例 `class: CLASS_CURRENCY` → `class: currency`（该字段是 string 词表非 proto 枚举，D15 补双轨规则）；D12 补 bucket permissions 为排序归一精确比较（服务端原样存取、不做 write 展开，storage.go A8——与 collection 的 D20 处理刻意不同）；动词名统一为资源全名（create_leaderboard_board 等 8 个）。
- 2026-09-14：终验与状态收口：五阶段（A 服务端 / B 文件层 / C 引擎核心 / D 三动词域 / E 文档）全部完成；字段写法口径定为 proto 原名 snake_case（protojson 兼容 camelCase，修正 §2.1 初稿的"camelCase"表述）；终验证据=全量 `task test` 绿 + authz-matrix 无漂移 + 000009 迁移落库 + 备用端口新 server 上 RunbookService RPC 可达（Unauthenticated 而非 Unimplemented，CLI→gRPC→authz 链路通）。遗留：真 API key 的完整 up/status/down 真机循环待用户环境首用验证（19-runbook.md 有快速上手）。
