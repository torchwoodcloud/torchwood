# POC 直切点的存量升级路径（转出迁移方案）

> **归档说明（2026-09-06）**：本方案的适用前提——"POC 期任一历史版本上、数据不可弃的真实存量部署"——经拍板确认**不存在**（当前无任何生产/对外存量库），存量处置一律重建（`docker:purge` + `db:migrate`）。本文转历史材料存档，不再维护；同日 `db/migrations` 基线重定为 5 个迁移（000001 控制面 / 000002 事件脊柱 / 000003 全局 catalog+账本 / 000004 RBAC·RLS·roles_sig / 000005 pgvector），文中 000023–000033 为旧编号，映射见 `docs/developer/15-exit-poc.md` 文首横幅。
> 状态：**决策材料（A5 拍板附件），待维护者评审**——本文件履行 redesign 状态头义务（"转出 POC 前需重审本文所有'直接切换'类表述并补迁移方案"），对应 `docs/developer/15-exit-poc.md` A5 条目。评审通过后本文件转为活跃方案；实施类条目（G2 迁移器、000031 补丁）按 §8 的结论立项。
> 成文：2026-09-05，基于对 redesign 全文、`db/migrations/000001..000030`、`internal/infra/documentdb`（rls_policy.go / postgres_collection_ddl.go / catalog_codec.go / acl_column.go）、`internal/infra/projectschema`（000001/000009/000011 与 git 历史 copy.go@47ea7ac/fa0834d）、`internal/pkg/bootkit/hooks.go` 的逐项核验。
> 适用对象：**在 POC 期任一历史版本上建立、且数据不可弃的真实存量部署**。本地/测试/可弃数据一律走 POC 定义（`docker:purge` + `db:migrate` 重建），不适用本文件。

---

## 0. 总原则

1. **判定框架：重建 vs 迁移**，按三轴裁决——①数据价值（可弃？）；②停机容忍（可否整库重导？）；③迁移器成本（元数据搬迁 / 行回填 / 无损直换）。POC 定义下"重建"默认成立；本文件只为"①不成立"的部署给出迁移路径。
2. **元数据搬迁 ≠ 数据搬迁**：本方案的两个大迁移点（catalog 四表→两表、逻辑名→物理名）都是**纯元数据操作**（INSERT catalog 行 + `ALTER TABLE … RENAME`），行数据零拷贝；真正的行级回填只有 `_acl` 一处，且是可分批重跑的幂等 UPDATE。
3. **policy 启用顺序遵循 §11-A4 预置决议**（redesign §11-J 默认值整包）：**先全量回填 `_acl`，再开 RLS policy**。原因：空 `_acl` 在新语义下是"回退集合级"（fail-open 方向）——若先开 policy 后回填，存量带文档级 ACE 的文档会被 policy 视为空 ACL 文档而**放宽**可见性，属安全方向错误。
4. **执行载体复用既有机器**：迁移器形态参照已删的 `internal/infra/projectschema/copy.go`（git 47ea7ac/fa0834d，E5-1 系统表拷贝作业：探测→单事务 TRUNCATE+INSERT→失败不写 schema_migrations；redesign §9.3 教训 4 亦引用其教训）；列授权/policy 修复复用 A1 的 reconcile 扫描骨架（`refreshColumnGrants` / `ensureCollectionRLS` 均幂等重建）。
5. **红线**：`physical_name` 不出现在任何 API 响应（迁移前后都不允许）；业务文档表永不解析一段式 schema；迁移过程 fail-closed——无法转换的行进隔离清单并阻断完成，不静默丢弃。

## 1. 直切点总览与推荐一览

| # | 直切点 | 断裂面 | 存量库症状（不处置） | 推荐 | 处置形态 |
|---|---|---|---|---|---|
| 1 | `_perms` → `_acl`（000027 后旧表为死表） | 权限判定数据源换轴 | 带 ACE 文档权限丢失或越权（先开 policy 时 fail-open）；缺 `_acl` 列的表在首次 DDL touch 即报错（`ensureACLIndex` 对无列表 CREATE INDEX 失败） | **迁移**（回填） | §3：补列→回填→GIN→policy→删死表 |
| 2 | 每项目 catalog 四表 → public 全局两表（000025） | catalog 元数据换轴 | 新二进制读 `catalog_collections` 为空 → 全部集合 NotFound；且启动钩子 EnsureAll 会先触发 projectschema 000011 DROP 四表，**元数据被先销毁**（§2.3 语义切换点） | **迁移**（搬迁器） | §4：四表→两表列映射 + 物理名分配 |
| 3 | 物理名解耦（存量表名=逻辑名 → `c_<base32(8)>`） | 表寻址换轴 | 同上，伴随 §4 一并处置；realtime 频道保持逻辑 ID 无迁移 | **迁移**（RENAME） | §5：ALTER RENAME + 回填 physical_name |
| 4 | 000029 类"原地修订不可重放" | 迁移文件修订语义 | 已应用库缺 R16 内容：旧 `tw_sig_match` 消息为 `roles\|exp`，新 Go 签 `tenant\|roles\|exp` → 验签恒失配 → `tw_roles()` 零角色 → **tw_app 全部查询静默空结果**（fail-closed 但可用性死亡）；首版钳制 INSERT(`_acl`) 的授权缺失 → 带 ACL create 失败 | **幂等补丁**（零用户动作） | §6：000031 前向补丁 + "禁改已应用迁移"规则 |
| 5 | 客户端契约断裂（offset token 失效、`queries` 双栈退役、`filter/order_by` reserved、错误码直换、403→404 翻转） | wire 契约 | 见升级矩阵；最危险一档是 reserved 字段被 proto 运行时静默忽略 → 旧 SDK 过滤条件失效（数据面仍受 RLS 保护，但结果集语义错误） | **无需服务端迁移**；客户端升级矩阵 + 可选服务端守卫 | §7：升级矩阵（与 A10 联动） |

**总体推荐**：真实存量部署走"**原地迁移器 + 幂等补丁**"（§8 载体与排序）；可弃数据维持重建。注意一个硬依赖：**在 B5（export/import）落地前，"重建"对真实用户不可执行**（无导出工具，整库重导只能手工 pg_dump/psql 拼装）——因此迁移器不是"可选项"，而是转出门禁的隐性前提；若拍板"重建优先"，须先把 B5 提升为 A 区前置。

## 2. 存量库风险面：为什么"直接跑新版本"不可接受

升级三步（`db:migrate` → 启动 → DDL touch）在存量库上的实际行为逐点核验如下，这是迁移方案必须存在的原因：

1. **`db:migrate`（public 迁移 000001..000030）本身安全**：000025 建空全局 catalog（注释明示"本迁移不搬数据"）；000026/000027/000029 建 RBAC/RLS/签名面；不动项目 schema。
2. **首次启动的 EnsureAll 是销毁点**：`internal/pkg/bootkit/hooks.go` 的 `ProjectSchemaEnsureHook` 对每个项目幂等执行 `projectschema.EnsureAll` → 存量项目 schema_migrations 落后时逐版重放至最新 → **000011 `DROP document_attributes/indexes/collections/databases`**（IF EXISTS，静默成功）。此后四表元数据不可恢复（行数据表还在，但 catalog 换轴所需的 attrs/indexes/permissions 定义已随表删除）。**迁移器必须挂在 `db:migrate` 之后、服务首次启动之前**，或自行接管 000011 的执行（§4.4）。
3. **首次 DDL touch 是第二个故障点**：`reconcileVersionColumn` → `ensureACLIndex` 对无 `_acl` 列的存量表执行 `CREATE INDEX … USING gin (_acl)` 直接报错（现有代码没有 `ADD COLUMN _acl` 的对账分支——`_acl` 仅存在于 `createCollectionTable` 的新表列清单里）。即存量表连"懒修复"都不存在，必须前置补列。
4. **RLS 面**：000026 三角色 + 000027 函数对存量库生效后，凡被 touch 的表立即进入 policy 管辖（`ensureCollectionRLS`），此时 `_acl` 尚未回填 → §0.3 的 fail-open 窗口。policy 启用与回填的先后必须由迁移器保证，不能依赖 DDL touch 顺序。

## 3. 直切点①：`_perms` → `_acl` 回填与切换

### 3.1 两个数据模型（同构性依据）

- 旧：项目 schema 内独立表 `<schema>._perms (_tenant, _collection, _document, _type, _permission)`，每 (文档, 权限类型, 角色) 一行；`_type` ∈ read/create/update/delete（000008 迁移对 `_type IN ('update','delete')` 的操作证明类型逐行存储）。
- 新：文档行内嵌 `_acl TEXT[]`，元素 `"type:role"`（`acl_column.go`："与 `_perms` 行模型的 (_type,_permission) 二元组同构——只换存储，语义模型不动"）；判定 `tw_can` 对 create/update/delete 同时匹配 `"write:role"`（matchTypes 的 write 展开，语义保留）。

**映射即行→元素拼接**：`_acl 元素 = _type || ':' || _permission`（角色含冒号如 `user:<id>` 时无损——新解析 `parseACLStrings` 按首个冒号切分，同一约定）。

### 3.2 可执行步骤（每业务库 `tw_<project>_<database>`，每集合一张物理表）

```sql
-- S1 补列（幂等；必须在 GIN/policy 之前——存量表无 _acl 列）
ALTER TABLE tw_p_db.c_xxx ADD COLUMN IF NOT EXISTS "_acl" TEXT[] NOT NULL DEFAULT '{}';

-- S2 回填（幂等可重跑；只对空 _acl 行生效 → 断点续跑天然安全）
UPDATE tw_p_db.c_xxx d
SET    "_acl" = sub.ace
FROM (
    SELECT p._document,
           array_agg(DISTINCT p._type || ':' || p._permission
                     ORDER BY p._type || ':' || p._permission) AS ace
    FROM   tw_p_db._perms p
    WHERE  p._collection = '<逻辑collection名>'   -- catalog 旧四表的 document_collections.id
      AND  p._tenant = <internal_id>
    GROUP BY p._document
) sub
WHERE d."_id" = sub._document AND d."_acl" = '{}';

-- S3 GIN 索引（ensureACLIndex 同形态）
CREATE INDEX IF NOT EXISTS idx_<phys>_acl ON tw_p_db.c_xxx USING gin ("_acl");
-- S4 policy + FORCE + 列级 GRANT：不手写 SQL，调 ensureCollectionRLS（幂等重建）
-- S5 确认回填完成后删死表：DROP TABLE tw_p_db._perms;
```

参数说明：`_collection` 用**逻辑 ID**（`_perms` 与 realtime 频道保持逻辑 ID，物理名解耦不改它）；sentinel 库 `tw_<project>` 无 `_perms`（系统静态表 000009 cut 时已清理）；`default` 是普通第一库，同样走本流程。

### 3.3 完成判据与验证

- 行数守恒：回填前 `SELECT count(DISTINCT _document) FROM _perms WHERE _collection=…` = 回填后该表 `_acl <> '{}'` 行数（同 tenant）。
- 语义抽查：抽样文档按旧 `AllowsDocumentAccess` 语义人工判定，与 `SELECT public.tw_visible(_acl, roles, docsec, …)` 结果一致（roles 用测试角色集）。
- 空 ACL 语义差显式声明：旧模型下"有 ACE 行的文档"回填后走 ACE 分支；"无 ACE 行的文档"新旧都回退集合级——**无语义漂移**。唯一行为变化是已文档化的"可写即可读"（update-only ACE 自然获得可见性，redesign §3.2 连带变化 1，良性放宽，无迁移）。
- 死表清理后探测：`to_regclass('<schema>._perms') IS NULL`（对齐 000008/000009 的既有探测写法）。

## 4. 直切点②：每项目 catalog 四表 → public 全局两表（数据搬迁器设计）

### 4.1 源模型（`internal/infra/projectschema/migrations/000001_catalog.up.sql`，git 221424c 可考）

四张表均为 `TEXT[]` 行模型：`document_databases(project_id,id,name,时间戳)`、`document_collections(id,database_id,project_id,name,document_security,permissions TEXT[],disabled,is_system,时间戳)`、`document_attributes(id,collection_id,database_id,project_id,key,type,size,required,is_array,default_value TEXT,options JSONB,created_at)`、`document_indexes(id,collection_id,database_id,project_id,type,attributes TEXT[],orders TEXT[],created_at)`。

### 4.2 目标模型与列映射（对齐 `catalog_codec.go` 的 JSONB 契约）

| 源 | 目标 | 形态变换 |
|---|---|---|
| `document_databases` | `catalog_databases(project_id, database_id, name, created_at, updated_at)` | 平移：`id→database_id` |
| `document_collections` 行 | `catalog_collections` 行 | `id→collection_id`；`permissions TEXT[]` 元素按**首个冒号**切分为 `permissionJSON{type,role}` 数组；`physical_name` 新分配（§5）；`schema_version=1`、`ddl_seq=1`（终态默认值，新表出生即此值）；`disabled/is_system/document_security` 直拷 |
| `document_attributes` 行集 | `attrs JSONB`（`attributeJSON[]`） | `is_array→array`；`default_value TEXT→default any` **需按 type 定型**：boolean→`'true'/'false'` parse、integer/float→数值 parse、string 族（string/email/url/uuid/enum/datetime）→原样字符串；parse 失败进隔离清单阻断完成（fail-closed）；`id/key/type/size/required/options` 直拷 |
| `document_indexes` 行集 | `indexes JSONB`（`indexJSON[]`） | `attributes/orders TEXT[]` 平移；`metric` 空缺（POC 存量无 hnsw）；`id/type` 直拷 |

搬迁器在 Go 侧实现（复用 `encodeAttributes/encodeIndexes/encodePermissions` 的现有编码函数，**保证与运行时读侧 `decodeAttributes` 逐字段可回读**——这是"形态变换"正确性的机制性验证，不另写第二套编码）。

### 4.3 幂等与断点续跑（redesign §11-G2 要求）

- **探测**（对齐 copy.go `detectCopyAction` 模式）：`to_regclass('<schema>.document_collections')` 存在 且 `catalog_collections` 中该 project 无行 → 待搬迁；catalog 已有行 → no-op；二者并存 → 报错人工介入（半完成态防重入冲突）。
- **单库单事务**：某 project 的 catalog 行 INSERT + 物理名 UPDATE + 四表 DROP 在一个事务（量级 = 集合数，毫秒级）；失败整体回滚、不写任何版本标记（copy.go 纪律："失败不写 schema_migrations"）。
- **进度记录**：public 侧新表 `legacy_migration_log(project_id, phase, finished_at)`（或复用控制面已有元数据表），断点续跑按 project 粒度恢复；行数据不搬，无长事务风险。

### 4.4 EnsureCatalog 语义切换点（G2 明示要求）

现状：`EnsureCatalog` 已收缩为"仅确保 schema 存在"（`postgres_catalog.go` 注释），但 **`projectschema.EnsureAll`（启动钩子）会重放 000011 DROP 四表**。二选一拍板：

- **方案甲（推荐）**：独立一次性命令 `torchwood admin migrate legacy`（CLI `cmd/torchwood` 新子命令，走 Server API 或直连 DSN），部署序列为 `db:migrate` → `torchwood admin migrate legacy` → 首次启动。命令完成后将该项目 schema 的 `schema_migrations` 推到 11（含自身 DROP 四表），启动钩子重放即 no-op。
- 方案乙：给 projectschema 000011 加"catalog 已搬迁才 DROP"的运行时门槛——改动已发布迁移语义，与 §6 的"禁改已应用迁移"规则同病，否决。

## 5. 直切点③：物理名解耦（存量表名 = 逻辑名 → `c_<base32(8)>`）

> **【勘误 2026-09-06：本直切点整体作废，零迁移】** redesign §4.2 勘误登记裁决：随机物理名退役，**物理表名 = collectionID**（collectionID 收紧小写 `^[a-z_][a-z0-9_]*$` ≤40）。存量库的物理表名本来就是逻辑名，RENAME 迁移不再需要——`physical_name` 列值即 collection_id 冗余投影，唯一约束随 000003 基线删除。§4 搬迁器相应简化：catalog 行回填时 `physical_name = collection_id` 直拷，`newPhysicalName()` 分配与 `ALTER TABLE RENAME` 步骤删除。本节以下原文仅存档。

随 §4 搬迁器一并执行，单独成节只为把不变量写死：

1. **分配**：复用 `newPhysicalName()`（`catalog_codec.go`，5 字节熵 base32(8)，全局唯一约束 + 碰撞重试）。
2. **RENAME 与 catalog 回填同事务**：`ALTER TABLE tw_p_db."<逻辑名>" RENAME TO "c_<base32>"` → `UPDATE catalog_collections SET physical_name=…`。RENAME 是纯元数据操作（毫秒级，不重写行），索引/约束随表自动跟随，无需重建。
3. **排除面**：sentinel 系统集合（`database_id='_'`）物理名=逻辑名（静态表不可改名），000025 的部分唯一索引 `WHERE database_id <> '_'` 已承载该语义；realtime 频道与 `_perms` 同理保持逻辑 ID，零迁移。
4. **校验**：迁移完成后跑一遍物理名零泄漏断言——遍历 catalog 全部行，`physical_name ~ '^c_[a-z2-7]{8}$'`；API 响应捕获测试（现有 `postgres_physical_name_test.go` 形态）对 List/Get 集合响应断言不含 `c_` 前缀串。
5. **风险**：外部若存在依赖表名的旁路（直连库的运维脚本）会断——这正是"物理名不进契约"红线的预期效果；迁移公告中写明。

## 6. 直切点④：000029 类"原地修订不可重放"迁移的处置

### 6.1 事实

000029 首版随 02f7288 应用后，R16（910d661）**原地改写了同版本号文件**（sig 消息扩为 `tenant|roles|exp`、新增 `tw_tenant()`、`tw_set_document_acl` 两道强制、INSERT(`_acl`) 列授权恢复）。golang-migrate 只记版本号：已应用库不会重跑 → 函数面停留在首版。redesign §6 记录"000029 为原地修订（POC 未发布同一语义单元，本地库重建）"——该豁免仅对"可重建"成立，真实存量库正是本节要处置的面。

### 6.2 不处置的后果（可验证推演）

- 新 Go（R16 起）签 `tenant|roles|exp`，旧 `tw_sig_match`（首版）按 `roles|exp` 验签 → **恒失配** → `tw_roles()` 返回空数组（fail-closed）→ tw_app 一切查询空结果、一切写被 policy 拒——静默全站不可用，且症状是"空数据"而非报错，排障成本极高。
- 首版对 INSERT(`_acl`) 的钳制残留 → create 携带 ACL 的文档报权限错误。

### 6.3 处置方案（推荐 ①）

- **① 前向幂等补丁迁移 `000031_roles_sig_r16_reconcile.up.sql`（推荐）**：内容 = 逐字重复 R16 后 000029 的函数面（`CREATE OR REPLACE FUNCTION tw_sig_match/tw_roles/tw_tenant/tw_set_document_acl` + GRANT/REVOKE + `tw_secrets` 表兜底）。全部语句天然幂等（CREATE OR REPLACE / GRANT / REVOKE），新库旧库重放结果一致；列级授权的 INSERT(`_acl`) 恢复并入 **A1 的全量 reconcile 扫描**（`refreshColumnGrants` 本就按终态口径重刷，同一次遍历覆盖）。成本一次、规则立住。
- ② 启动钩子每次 boot 重放函数面 DDL：作为 ① 的补充观察项，不做首选——迁移期应显式有序（`db:migrate` 先于启动），把函数面混进每次启动会掩盖漂移来源。

### 6.4 规则固化（防复发）

写入 AGENTS.md 数据库约定：**已应用的迁移文件永不原地修订**；语义修订一律新版本号前向补丁（即使 POC 期本地库"重建即修复"，也以新版本号表达——POC 期重建兜底的只是数据，不是迁移历史）。000031 是该规则的首个执行案例。

## 7. 直切点⑤：客户端契约断裂升级矩阵（与 A10 联动）

### 7.1 升级矩阵

| 断裂项 | 旧形态 | 新形态 | 客户端症状 | 迁移义务（写进 SDK migration note） |
|---|---|---|---|---|
| 分页 token | offset 族：`v1:offset`/base64 JSON offset token；`offset()` 构造器 | keyset-only：`ka:/kb:`+docID 完整游标（多键 tiebreaker） | 旧 token 被服务端**显式拒绝**（InvalidArgument，非静默） | 升级后丢弃在途 token 从首页重取；`offset()` 调用点改 orders+cursor |
| `total` 语义 | 每页精确 total | 续页 `total=0=unknown`；count 独立 API | 依赖 total 的分页 UI 失真 | 首页取 total / 独立 CountDocuments |
| `queries` DSL 双栈退役 | List 请求 `queries` 字符串字段（部分接口曾被静默忽略） | 字段 reserved；唯一过滤 = `query`（typed AST / SDK 构造器 + FromDSL 糖） | **最危险档**：proto 运行时对 reserved 前的未知字段默认忽略 → 旧 SDK 的过滤静默失效（结果集变大；RLS 仍兜底数据安全） | SDK 与服务端同批升级（见 A10 兼容矩阵）；升级期间禁止依赖 queries 的旧客户端写操作 |
| `ListRequest.filter/order_by`（静态面） | 保留但恒拒的过渡态 | reserved（W-K 终结） | 同上，静默忽略类 | 同上；静态面过滤走 `pkg/crud` 参数面 |
| OCC 三态 | `expected_version=0` → FailedPrecondition（错位）；version mismatch → `version_mismatch` | 缺省=拒（`DOCUMENT.VERSION_REQUIRED`）；≤0=InvalidArgument（`DOCUMENT.VERSION_INVALID`）；冲突=FailedPrecondition+`DOCUMENT.VERSION_CONFLICT`（retryable） | 按 gRPC code 分支的客户端基本兼容；按消息文本匹配的失效 | 错误处理改按 `code`（域码）+`retryable` 判别；禁止匹配 message 文本 |
| 不可见文档 403→404 | Get/Update/Delete 不可见行 → `PERMISSION_DENIED` | → `NOT_FOUND`（防枚举，有意翻转） | 依赖 403 探测存在性的逻辑（本就是越权探测）失效 | 按 404 处理"不可见"（与不存在同语义）；upsert 分支改用 exists 探测 |
| Upsert 冲突分支 | 命中行只查 update | 分支两侧分别裁决（create/update）；并发撞唯一键 → `DOCUMENT.ALREADY_EXISTS`（可重试） | 依赖"upsert 自动转 update"的竞态语义变化 | 重试即可（幂等 request_id 已覆盖） |
| 新增面 | — | `execute-tx`、`:aggregate`、`:changes`+last_seq、`vectorSearch`、幂等 `request_id` | 纯增量 | 无义务；按需采纳 |

### 7.2 服务端可选项：未知字段守卫

对"reserved 静默忽略"档，服务端可在文档面 List 入口用 protoreflect 扫描请求未知字段（`queries`/`filter`/`order_by` 的字段号），命中即回 InvalidArgument + 升级指引，把"静默语义错误"显式化为"可诊断错误"。代价是热路径一次未知字段扫描；推荐**仅在转出后第一个 minor 开启**（等价于兼容窗口），之后移除。是否启用归 A10 拍板材料一并决议。

### 7.3 错误码旧映射裁决（A5 条目明示要求）

**不提供旧码映射**。理由：旧形态没有稳定域码体系（message 文本不构成契约）；gRPC code 除 403→404 一处外全部保持；新的域码 + `retryable` 静态表是 Agent 判定面，映射回旧文本等于把已消灭的错位（C4） reintroduce。替代物 = §7.1 的 migration note + A10 的版本冻结承诺。

## 8. 总排序（部署 runbook 骨架）与推荐结论

单次升级的固定顺序（每步可验证）：

1. 备份：`pg_dump` 覆盖 public + 全部 `tw_*` schema（13-operations §6.3）。
2. 前置：A2 非 superuser authenticator 就位；A3 vector 扩展按 runbook（000030 需要）。
3. `task db:migrate`（public 000001..000030 + 000031 补丁）。
4. **停机窗口内**执行 `torchwood admin migrate legacy`：§4 搬迁器（含 §5 RENAME）→ §3 回填（可在线分批，建议同窗口）→ A1 reconcile 扫描（policy + 列授权终态重刷）。
5. 启动新二进制：bootkit 钩子完成 `SyncRolesSigKey`（tw_secrets UPSERT）与 EnsureAll（此时 000011 已 no-op）。
6. 冒烟：GetCollection 往返（attrs/indexes/permissions 与迁移前 API 快照逐字段比对）、抽样文档 tw_visible 判定、分页/写入/事件 seq 冒烟。
7. 客户端按 §7.1 矩阵升级 SDK（A10 的 migration note 载体）。

**每项推荐汇总**：①②③ 迁移（元数据操作为主，成本低、数据不弃）；④ 幂等补丁（零用户动作）；⑤ 无服务端迁移、客户端按矩阵升级。**整库重建仅推荐给可弃数据**；真实存量部署的迁移器是转出硬前提（B5 未落地前"重建"不可执行——若拍板重建优先，先提级 B5）。

## 9. 实施立项（评审通过后）

| 工作项 | 内容 | 归属 |
|---|---|---|
| `000031_roles_sig_r16_reconcile` | §6.3 ①（函数面幂等重放） | infra/clients + db/migrations |
| `torchwood admin migrate legacy` | §3+§4+§5 搬迁器（探测/单库事务/隔离清单/进度表），复用 catalog_codec 编码与 copy.go 形态 | cmd/torchwood + projectschema/documentdb |
| A1 扫描扩展 | 扫描步骤并入 §3 S1/S3/S4 与 INSERT(`_acl`) 授权修复 | documentdb（A1 同会话） |
| 部署 runbook | §8 固化进 13-operations §6 | 文档会话 |
| 验证用例 | §3.3 守恒/抽查、§4.2 回读一致性（encode↔decode）、§5.4 零泄漏断言 | 各归属会话 |
