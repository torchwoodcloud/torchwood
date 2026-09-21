# 06 数据库：三类库、标识与动态文档

面向后端开发者：Postgres 三层 schema、标识规则、DDL 约束、静态 / 动态表分工，以及查询与权限的完整实现语义。本章是 DocumentDB 子系统的权威参考。

> 源码锚点：`pkg/ident/ident.go`、`internal/infra/documentdb/`、`pkg/query/`、`pkg/crud/`、`internal/domain/databases/`。

## 0. 子系统定义与边界

**DocumentDB 子系统 = Torchwood 的文档数据存储整体方案**。核心是 `Databases → Collections → Documents` 三级资源模型，端到端覆盖五件事：

1. **元数据目录**（全局 catalog 两表）；
2. **物理表管理**（collection DDL 与项目数据面迁移）；
3. **文档 CRUD 与查询编译**（typed AST → SQL）；
4. **权限集成**（集合 / 文档两级 ACL + 认证期角色注入）；
5. **事件集成**（事务性 outbox → realtime 按可见性扇出）。

七张系统静态表（`users/sessions/identities/groups/memberships/buckets/files`）是本子系统的**边界邻居**而非组成部分——它们共用项目数据面 schema 与 Principal / 角色语义，但不走动态文档路径。`IsSystemCollection` 名单仅用于 sentinel 写保护与测试重建。

### 模块地图

| 层 | 模块 | 职责 |
|---|---|---|
| 领域 | `internal/domain/databases/` | Document / Collection / Attribute / Index / Permission / Principal 模型；权限判定（`AllowsDocumentAccess` / `CollectionAllows` 等）；文档角色词表（`docrole.go`，唯一构造 / 解析源）；`DocumentDB` 四端口（Catalog / SchemaApplier / Documents / ChangeFeed，`repository.go`） |
| 查询 | `pkg/query/` + `pkg/query/proto/` | 单 typed AST（客户端语法糖解析器 + 程序化构造器 + `ToWireJSON`；proto↔AST 编解码）。`shared.v1.Query` 是服务端唯一消费形态，DSL 串仅作 SDK / CLI 客户端糖 |
| 适配器 | `internal/infra/documentdb/` | 全局 catalog 寻址、collection DDL、文档 CRUD（OCC / Upsert / Bulk / advisory lock）、查询编译与执行、权限 SQL 下推、SQLSTATE 翻译、catalog JSONB 编解码 |
| 应用 | `internal/app/documents/` + `internal/app/server\|client` 的 Databases 用例 | Client/Server 共用核；用例守卫（sentinel 拒绝 / 标识校验 / 系统集合拦截 / disabled）、空 ACE 种子、grant 展开与校验、错误映射 |
| 数据面 | `internal/infra/projectschema/` + `pkg/ident/` | 项目 schema 生命周期（Apply / 迁移 / 孤儿对账 / 缓存失效桥接）；两段式寻址与标识规则 |
| 规模观测 | `internal/infra/documentdb/scale_metrics.go` | schema-per-project 布局的量化预警：三平面物理表计数（catalog / 一段式静态面 / 两段式业务面）与 pg_dump 时长指标骨架，启动钩子采集（runbook 见 `13-operations.md` §5.1） |
| 迁移 | `db/migrations/`（public 控制面）+ `internal/infra/projectschema/migrations/`（项目数据面模板） | catalog / outbox 控制面演进；新项目一次性建面 + 存量 `EnsureAll` 自愈。legacy 每项目四表已退役（projectschema 000001 no-op + 000011 DROP 存量） |
| 导入导出 | `internal/infra/documentdb/export.go` / `import.go` + `cli/admin_export_import.go` | 项目级文档面导出 / 恢复（`torchwood admin export/import`）：catalog 快照 manifest + 每集合全行 NDJSON + `snapshot_seq`——读取包在单一 REPEATABLE READ 快照事务内，`snapshot_seq` 与 `:changes?since_seq=` 续接恰好闭合（导出后变更无重无漏） |
| 事件 | `internal/infra/events/` → `internal/infra/realtime/` | 写路径同事务落 `document_events_outbox`（全局 seq + `pg_notify` 唤醒）→ worker XADD Redis Stream `torchwood:events` → 每实例一消费组 XREADGROUP → hub 按快照 ACL 过滤扇出（`VisibleTo`），出站帧剥 ACL；补偿走 `:changes` / WS `last_seq` 重放。经济 / 系统行为事件共用同一 outbox / Stream（显式 `channel` 列），另有函数事件触发器独立消费组 |
| 分页 | `pkg/crud/pagination.go` + documentdb keyset token | HMAC 签名 offset token 仅供静态表 / 控制面列表；文档面 **keyset-only**（`ka:/kb:` token，见不变量 12） |
| 幂等 | `internal/domain/databases/idempotency.go` + bunrepo + app 核层 | `request_id` 写幂等（public.`idempotency_keys`）：只缓存成功响应、24h 重放、`KEY_CONFLICT` / `IN_PROGRESS` 域码 |
| 传输 | `internal/api/servergrpc\|clientgrpc/databases.go` + 对应 proto | 请求校验、authz 注解、AST 参数绑定（`BindListQuery`）、OpenAPI 契约 |

### 范围外

Storage 对象本体（`files` 行只是元数据，对象在 S3/MinIO）、Functions 执行、账本 / OAuth 目录（同在 `tw_<project>` 但独立演进）、billing 用量统计（消费 `files.SumSize`，不经过文档端口；`SumDocumentField` 已删除，`:aggregate` 的 sum 是其继任）。

### 关键不变量（变更评审锚点）

1. **租户隔离**：所有文档行访问强制 `d._tenant = ?`（`_tenant = projects.internal_id`，进程内缓存带 30s 回库核验 + 项目删除失效桥接）；`_tenant` 列对 `tw_app` 列级授权锁死不可写。
2. **DDL 只走两段式**：`businessSchema` 显式拒绝 sentinel `_` 与一段式；`DROP SCHEMA` 永不指向 `tw_<project>`。
3. **同事务原子性**：文档数据行、`_acl`、outbox 事件三者同事务提交，任一失败整体回滚。`_acl` 写入按行生命周期分两条治理通道：create / upsert 插入支随 INSERT 携带（新行无旧行、无可见性复检语义，授予治理在 app 层）；既有行替换唯一通道 `tw_set_document_acl` 函数（同事务；函数内强制 p_tenant = 验签 tenant + 目标行 `tw_visible` 可见性）。
4. **OCC**：用户集合强制 `_version`，Update / Delete 必填且须匹配；列缺失 / 类型冲突 fail-closed（不落 PG 42703）。
5. **注入防御与数据键 fail-closed**：标识符 `safeNameRe` + `quoteIdent` 双重转义；查询值全程参数绑定；LIKE 走 `escapeLikePattern` + `ESCAPE`；写入面非法数据键（`_` 前缀 / 标识符语法 / 超 63 字节）显式 InvalidArgument 拒绝，不静默丢弃。
6. **判定单源**：业务集合的权限判定执行点 = RLS policy（`tw_can` / `tw_visible` SQL 函数，public 迁移 000004；SQL golden 矩阵 `rls_policy_test.go` 锁语义，禁止 Go 侧等价实现）。sentinel 系统集合保留应用层判定（`AllowsDocumentAccess`）。policy 的 catalog 取值一律 `(SELECT ...)` InitPlan 化（EXPLAIN 门禁常驻），集合级权限变更零 DDL 实时生效。
7. **事件语义**：at-least-once；**同文档事件按 seq 全序；集合内为分配序（跨文档不保证与提交序一致）；seq 有空洞（空洞 = 回滚事务，不丢事件）**。客户端按 `event_id` 幂等去重、以 `seq` 作续传游标（`last_seq` / `:changes?since_seq=`）；出站帧永不含 ACL 快照。Redis Stream 只承担传输——正确性与重放窗口在 outbox 表（published 24h 清理 ≫ 1h 重放承诺）。
8. **默认私有**：`DefaultCollectionPermissions` 不含 `read:any`；空 ACE 文档按种子规则私有化（key 主体 `key:<自身id>` / owner `user:<id>` / 创建者角色 / `__private__`）。
9. **标识长度**：`project.id` / `database.id` ≤28（schema 名 ≤60 字节）；**collectionID ≤40，`^[a-z_][a-z0-9_]*$` 小写**（集合 ID 同时是物理表名，小写使 psql / pg_dump 等运维路径免引号直用）；属性 key ≤63；索引 ID ≤40。**物理表名 = collectionID**：DDL / 行查询 / 索引名（`idx_<coll>_<id>`）走逻辑名，运维直接可读；63 字节截断由组合校验把守（app 入口 `validateIndexNameLen` + infra 二道防线，各自合法但组合超限即 InvalidArgument）；`catalog_collections.physical_name` 列保留为 collectionID 冗余投影；sentinel 系统集合物理名 = 逻辑名。跨项目 / 跨库同名集合合法——凡按 `physical_name` 反查 catalog 的 SQL（RLS policy 子查询、`tw_set_document_acl` 白名单）必须按 (project, database) 三元组收窄。
10. **查询单栈**：wire 只收 `query`（typed AST `shared.v1.Query`）；`queries` DSL 字符串字段已 reserved，服务端文档查询栈零字符串解析。算子全集 `eq ne lt lte gt gte in between notBetween isNull isNotNull contains notContains startsWith notStartsWith endsWith notEndsWith search notSearch containsAny containsAll` + `and/or`（嵌套深度 ≤8；无通用 NOT，取反由 not* 变体承担——索引友好；containsAny / containsAll 仅 array=true 属性可用）；`select` 投影。DSL 串是 SDK / CLI 客户端糖，解析为 AST 后发送。跨 filter 绑定参数累计 ≤2000（封死 PG 65535 语句参数上限）。
11. **写幂等**：携带 `request_id` 的写请求键作用域 `(project_id, actor_id, request_id)`；只缓存成功响应（失败释放、重试重新执行）；同 key 异体 → `IDEMPOTENCY.KEY_CONFLICT`；并发同 key 短轮询 ≤2s 后仍 in-flight → `IDEMPOTENCY.IN_PROGRESS`；重放返回原响应 + `x-torchwood-replayed: true` 响应头；done TTL 24h、in_flight 兜底 TTL 5min、惰性清理。
12. **keyset-only**：`ListDocuments` 只发 / 只认 `ka:/kb:` token；`offset()` 算子与非 keyset token 一律 InvalidArgument。ORDER BY = 全部排序键 + `_id` tiebreaker（方向随首键）；keyset 谓词按方向行比较或逐键 OR 展开（多键游标完整支持；token 只编码 docID，服务端查行取全部键值）。
13. **聚合一律在可见行集上执行**：`:aggregate` 的可见性由 SELECT policy（securityQuals）承载且过滤先于 GROUP BY——不可见行不进聚合、group 键不泄露；聚合目标必须是声明的数值属性（integer / float）。
14. **连接模型与角色分层**：单一变色龙 authenticator（DSN 用户，成员含 `tw_owner` / `tw_app` / `tw_system` 三角色）+ 每请求一事务（含读，autocommit 退役）。事务首条 `SET LOCAL ROLE` + `set_config('app.roles', …, true)`（漏注入 = policy 恒 false，fail-closed；`SET LOCAL` 事务结束自动失效）。SystemPrincipal / PlatformAdmin → `tw_system`（BYPASSRLS），DDL → `tw_owner`，其余 → `tw_app`；业务文档表 `ENABLE + FORCE ROW LEVEL SECURITY`（owner 亦受 policy，仅 BYPASSRLS 旁路）。**roles_sig 验签**：tw_app 注入同时携带 `app.tenant` 与 `app.roles_sig = HMAC-SHA256(密钥, tenant|roles|exp)`（180s 窗口 = 3×pgdriver ReadTimeout，覆盖 execute-tx 长事务与 DB 时钟偏差；密钥 = `HMAC-SHA256(jwt.secret, "tw-roles-guc-v1")` 进程派生，落 `tw_secrets`；双钥轮换：current / previous 槽位 + `tw_sig_match` 任一钥命中——换钥窗口内旧 sig 不降级）。`tw_roles()` / `tw_tenant()` 为 SECURITY DEFINER 验签函数——`app.roles` / `app.tenant` GUC 可被任何持 SQL 会话者 set_config 伪造，验签通道封死（无 sig / 错 sig / 过期 → 零角色 / NULL tenant fail-closed；`tw_set_document_acl` 强制 p_tenant = 验签 tenant，跨租户伪造在签名层死锁）。已知豁免面：DSN 用户为 superuser 时绕过 policy（生产应配非 superuser 应用账号，runbook 见 `13-operations.md`）。
15. **可写即可读**：SELECT policy = `tw_visible`（read ∨ update ∨ delete 命中）；不可见行对 Get / List / Aggregate 一律"不存在"（防枚举，NotFound 取代 403）；写路径 0 行探测区分 NotFound（不可见）/ PERMISSION_DENIED（可见不可写）/ VERSION_MISMATCH。`_acl` 直改旁路从 UPDATE 列授权封死（见 §7.3）；upsert 拆预查分支 + 普通 INSERT/UPDATE（ON CONFLICT 推测插入要求拟插入行过 SELECT policy，结构性冲突）。

## 1. 三类库

| 层 | Schema 形态 | 技术 | 关键表 |
|---|---|---|---|
| `public` 控制面 + 事件脊柱 | 固定 `public` | bun + golang-migrate（`db/migrations/`） | `projects` / `admins` / `admin_projects` / `api_keys` / `audit_logs` / `project_oauth_providers` / `provider_resource_index` / `idempotency_keys` / `document_events_outbox`(+`_dead`) + **全局 catalog 两表 `catalog_databases` / `catalog_collections`**（迁移 000003） |
| 项目数据面 `tw_<project>` | 一段式 | bun + `internal/infra/projectschema/` | 静态表 `users` / `sessions` / `identities` / `groups` / `memberships` / `buckets` / `files` + 账本 / Functions / OAuth 目录（文档目录已全局化迁出） |
| 业务文档面 `tw_<project>_<database>` | 两段式 | 原生 SQL（`documentdb`） | 每个 `database.id` 一个 schema，只放用户 collection 物理表（**表名 = collectionID**，小写）；每表带内嵌 `_acl` + GIN 索引 + RLS policy |

补充：

- `app` 是 CreateProject 缺省创建的首个业务库（普通库，可删可重建；显式透传 `FirstDatabaseID` 路径不变）。
- 系统静态表不再是文档集合：`internal/infra/projectschema/migrator.go` 在 `CreateProject` 同事务 `CREATE SCHEMA` + `Apply`（迁移模板建 `sys_*` staging 表后由 cut 迁移 rename 为最终名），进程启动 `EnsureAll` 自愈。
- **catalog 全局化**：catalog 是 cluster 内全局的两张 public 表——`catalog_databases` 简单行 + `catalog_collections` 把 attrs / indexes / permissions 以 JSONB 列合一（含 default / size / array 全量属性契约、`physical_name`、`schema_version`、`ddl_seq` 乐观锁）。GetCollection 热路径单查询读回全量契约；每项目四表模型与模板已退役。

## 2. 标识与 Schema 规则

`pkg/ident/ident.go`：`^[a-z][a-z0-9]{0,27}$`、`MaxSchemaResourceIDLen=28`。入口 `ValidateSchemaResourceID`；对外（app 用例层入口）再走 `internal/app/shared.RejectExternalDatabaseID` 显式拒绝 sentinel。

- `ProjectSchemaName(p)` → `tw_<p>`，匹配一段式正则；`SchemaName(p,db)` → `tw_<p>_<db>`，匹配两段式正则；两者不相交（`project.id` 不含 `_`，前缀后第一道 `_` 即分割点，`ParseSchemaName` 反解无歧义）。
- `IsTwoSegmentSchema(name)` 断言 DDL 目标必须两段式。
- `ident.ProjectDataPlaneID = "_"` 仅内部寻址：`documentSchema` 在 `databaseID=="_"` 时映射到 `ProjectSchemaName`；对外非法。
- `_tenant` 取 `projects.internal_id`（`postgres.go:resolveInternalID` + `sync.Map` 缓存，命中后每 30s 回库核验一次，漂移即 WARN 并切换新值），所有行查询强制 `d._tenant=?`；建表路径强制取实时值（`resolveInternalIDFresh`，防陈旧租户号烤进 `_tenant` 列默认值）。
- 字段 / 表名均 `quoteIdent` 转义（`"` → `""`），并经 `safeNameRe=^[a-zA-Z_][a-zA-Z0-9_]*$` 白名单。

## 3. 两段式 DDL（businessSchema）

只接受两段式，永不解析一段式：

```go
schema, err := ident.SchemaName(projectID, databaseID) // 非法直接 InvalidArgument
if databaseID == ident.ProjectDataPlaneID { /* sentinel 显式拒绝 */ }
if !ident.IsTwoSegmentSchema(schema) { return status.Error(codes.Internal, "refusing to DDL a non two-segment schema") }
// ensureSchema：pg_namespace 存在性检查后 CREATE SCHEMA；真正新建时顺带
// GRANT USAGE 给 tw_app / tw_system（已存在即跳过）
```

- `CreateDatabase` = schema 创建 + `catalog_databases` 行写入同一事务（任一失败整体回滚）；`DeleteDatabase` = `DROP SCHEMA ... CASCADE` + catalog 行清理同事务（绝不 DROP 一段式 `tw_<project>`）。
- catalog 无 `database_id='_'` 行（CreateDatabase 入口拒绝 sentinel，无需 List 时过滤）。
- 进程内 `projectschema.Apply` 带 `sync.Map` 就绪缓存（事务内不写缓存）。

## 4. 静态表 vs 动态表

**静态表**（`tw_<project>`，`internal/infra/bun/model/`）：`users` / `sessions` / `identities` / `groups` / `memberships` / `buckets` / `files`，bun 模型，无 `_id` / `_acl` / `_version`，经 Account / Groups / Storage 专用 RPC 读写。`SystemCollectionIDs` 仍在 `internal/domain/databases/system_collections.go`，仅用于 DocumentDB 跳过 `_version` / 写保护与测试重建。`users.DocumentData()` 投影**不含 `password_hash`**（密码校验走 `usersRepo`）。

**动态表**（`tw_<project>_<db>.<collectionID>`，每集合一张真实表）：

- **物理表名 = collectionID**；运维可读，psql / pg_dump 免引号直用。sentinel 系统集合同形（指向静态表）。
- `catalog_collections.physical_name` 保留为冗余投影。DDL 与行查询经 `resolvePhysicalTable` 单条 catalog 点查——现值即存在性判定（行缺失 → NotFound，物理表与 catalog 行同生共死；sentinel 直通零查询）；进程内存在性缓存让热路径命中后零额外往返，失效面 = catalog_collections 全部删除路径（DeleteCollection / DeleteDatabase / import 清位），CreateCollection 写穿覆盖；跨实例陈旧语义 fail-loud（表已删 → PG 42P01 显式报错，无静默错写）。
- **物理寻址字段不出现在任何 API 响应**；realtime 频道保持逻辑 collectionID。
- 跨项目 / 跨库同名集合合法（表名语义局限于 schema 内）。所有按 `physical_name` 反查 catalog 的 SQL（RLS policy 子查询、`tw_set_document_acl` 白名单）**必须按 (project, database) 收窄**。

建表形态：

```sql
CREATE TABLE tw_shop_app.posts (
  _id TEXT NOT NULL, _tenant BIGINT NOT NULL DEFAULT 1,
  _created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  _updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  _created_by TEXT, _updated_by TEXT,
  _acl TEXT[] NOT NULL DEFAULT '{}',  -- 内嵌 ACE（"type:role" 元素）
  _version BIGINT NOT NULL DEFAULT 1, -- 用户集合有，系统静态表无
  -- 每个 attribute 一列（pgTypeFor 映射）
  PRIMARY KEY (_tenant, _id)
);
CREATE INDEX idx_posts_tenant_created ON tw_shop_app.posts (_tenant, _created_at, _id);
CREATE INDEX idx_posts_acl ON tw_shop_app.posts USING gin (_acl);
-- 用户集合另建四条 RLS policy + ENABLE/FORCE ROW LEVEL SECURITY + 列级 GRANT
--（tw_app 的 INSERT 排除 _tenant；UPDATE 排除 _tenant/_acl），见 §7（rls_policy.go）。
```

目录位于 public 全局两表（attrs / indexes / permissions 为 JSONB 列，含 `physical_name` / `ddl_seq`）。`DeleteCollection` DROP 物理表即权限随行消亡（`_acl` 内嵌，无跨表清理）。

**ddl_seq 乐观锁**：元数据写路径（UpdateCollection / CreateAttribute / CreateIndex / DeleteAttribute / DeleteIndex / MigrateAttribute / RestoreAttribute / RetireAttribute）CAS 递增（`UPDATE ... WHERE ddl_seq = ?`；CreateIndex 两阶段各 +1），0 行命中 → `CATALOG.DDL_CONFLICT`（Aborted + retryable——CAS 冲突非参数错误，调用方重读 catalog 后重试）。`schema_version` 已被消费：copy 迁移与即时迁移 commit 各递增。

**索引名** `idx_<collectionID>_<索引ID>`：collectionID 与索引 ID 各自 ≤40，但拼接超 63 字节被 InvalidArgument 拒绝（app 入口 + infra 二道防线），运维以精确索引名直查。

**在线索引通道（CIC 两阶段状态机）**：用户集合的 `CreateIndex` 走 catalog 两阶段——

1. 事务 A：**纯 catalog DML**，条目 status=building + CAS；
2. 事务外：`CREATE INDEX CONCURRENTLY IF NOT EXISTS`（独立连接 `SET ROLE tw_owner` + `lock_timeout=2s`，55P03/40P01 重试 ≤3，重试前清理失败残留的 INVALID 索引）；
3. 事务 B：置 active | failed。

CIC 不能在事务块内运行，是两阶段状态机的结构性原因。事务 A 保持纯 catalog DML（非并发 CREATE INDEX 一律取 SHARE 锁，任何锁型 DDL 都阻塞并发读写），因此 DDL touch 自愈（默认索引 / RLS / 列授权）移交 `ReconcileSchemaDrift` 对账扫描（§5）。建集合时的既有索引（新表无并发读者）与 sentinel 系统集合维持事务内通道；SQL 表达式单源 `buildIndexStatement`（concurrently 开关）。索引条目 status：active（缺省省略）| building | failed（残留已清理，重入可恢复）。

## 5. Attribute / Index 动态管理

| 操作 | SQL 行为 |
|---|---|
| `CreateAttribute` | `ALTER TABLE ADD COLUMN IF NOT EXISTS` + attrs JSONB 追加（含 default）+ ddl_seq CAS；`required→NOT NULL`、`default→DEFAULT`；加列后重刷列级 GRANT（新列立即获得 INSERT/UPDATE 授权） |
| `DeleteAttribute` | **删列两段的段一**：attrs 条目置 `deprecated`（幂等；migrating 状态拒绝）。读投影屏蔽（Get / List / KNN 剥离）、查询白名单拒绝、写入拒收（create/update/upsert/bulk/execute-tx 的 data / increment / array_updates 三通道；bypass 主体豁免）。物理列与数据保留，`RestoreAttribute` 可回滚 |
| `RestoreAttribute` | 段一回滚：deprecated → active；migrating → 中止迁移（DROP 新列、任务置 failed）并恢复 active |
| `RetireAttribute` | **段二（不可逆）**：deprecated 属性 `ALTER TABLE DROP COLUMN CASCADE`（物理索引随消亡）+ 同事务清理引用该列的 catalog 索引条目 + attrs 条目移除（同 key 可重建）。swap 后迁移残留旧列的退役同入口 |
| `MigrateAttribute` | **copy 迁移任务**：收紧 / 改类型 = 新列 `<key>__v<seq>` → 后台批量回填（批 500、批间 5ms、游标存 `catalog_migrations`，重入续跑）→ ACCESS EXCLUSIVE 锁窗全量追平重算 + 行数校验 → 原子 swap（旧列 RENAME 为 deprecated 残留、新列接管逻辑名）→ schema_version++。迁移期间该属性**写拒收、读放行**（旧列）；validate 失败显式落账（failed + error）不静默回滚。放宽（varchar 扩宽 / required→optional）= 即时 ALTER（元数据级，无 copy 任务），schema_version 同样递增 |
| `CreateIndex` | 两阶段 CONCURRENTLY 通道（§4 末）；unique / fulltext / hnsw / 数组 GIN array_ops 形态同源 `buildIndexStatement`；deprecated 属性不可作索引目标 |
| `DeleteIndex` | `DROP INDEX IF EXISTS` + indexes JSONB 删除 + CAS（`RunInTx` 原子） |
| `UpdateCollection` | 权限替换与字段更新同一 UPDATE，统一刷 `updated_at` + ddl_seq CAS（空 patch no-op；并发冲突 → `CATALOG.DDL_CONFLICT`） |
| **schema 漂移对账** | `ReconcileSchemaDrift`：启动钩子（server 侧）+ `torchwood admin schema repair [--dry-run]` 共用入口，扫三类漂移自动修复 + 告警。详见下文 |

**schema 漂移对账的三类漂移**：

1. **缺列**：catalog active attr 物理表无列 → ADD COLUMN + 列授权重刷（required 无 default 且表非空时不自动回填）；
2. **索引漂移**：stale building（>30min）按 pg_index 分流——valid 补账 active / INVALID·缺失 DROP 后 CIC 重入 / 活 CIC 不动；failed 条目重入；active 条目物理缺失或 INVALID 重建；无主 INVALID 清理；默认时间索引与 `_acl` GIN 缺失 CIC 补齐；
3. **幽灵表**：业务 schema 内 catalog 无行的表 → DROP + 告警。反向（catalog 行有而物理表缺失）只报告——重建等于放弃存量数据。

dry-run 全部只报告。骨架对齐 grants_reconcile（ORDER BY 全键、单集合失败不中断、指标告警）。

**属性生命周期**（attrs JSONB 条目 `status` 字段，缺省 active 零迁移）：`active → migrating → active`（copy 迁移窗口）；`active ⇄ deprecated`（两段删列段一，可回滚）；`deprecated → retired`（段二，条目移除）。JSON Schema 导出对 deprecated / migrating 属性标 `deprecated: true` 且不进 required。

**类型映射**（`pgTypeFor`）：`string/email/url → VARCHAR(n)/TEXT`、`integer → BIGINT`、`float → DOUBLE PRECISION`、`boolean → BOOLEAN`、`datetime → TIMESTAMPTZ`、`json → JSONB`、`vector → VECTOR(dims)`。

**数组属性**：`array=true` 落地 PG 原生数组列（`pgArrayTypeFor` 单源 DDL 与参数 cast）——`string→TEXT[]`、`integer→BIGINT[]`、`float→DOUBLE PRECISION[]`、`boolean→BOOLEAN[]`、`datetime→TIMESTAMPTZ[]`。元素类型仅限该标量子集（email / url / json 拒绝）；数组列不带 DEFAULT（缺省 NULL）。数组列的 key 索引自动选 `GIN (col array_ops)`（`&&` / `@>` 可走索引）且仅支持单列；unique / fulltext 对数组列拒绝（PG 数组无唯一约束语义）。数组值编码为 PG 数组字面量时按"先双写 `\`、再以 `\"` 转义引号"的固定顺序转义（PG 数组字面量带引号元素内 `\` 是转义符，顺序颠倒或 CSV 式 `""` 双写都会数据失真），读回走 `to_jsonb` 服务端解码无需对称处理。

**vector 属性**：`type=vector` + `dims`（必填，2..2000 = pgvector HNSW 可索引上限；非 vector 类型设置 dims 拒绝），落地 pgvector 原生 `VECTOR(dims)` 列（扩展由迁移 000005 启用；基座镜像 `percona/percona-distribution-postgresql:18` 预装 pgvector）。`default_value` 与 `array=true` 对 vector 拒绝。

- 写入值 = JSON 浮点数组（编码为 pgvector 字面量 + `?::vector` 绑定，维度绑定前校验）；读回契约 = JSON 数组（`to_jsonb` 原生输出，投影逐列覆盖 `::text::jsonb`）。
- **维度变更 = 新列 + 数据重灌**（换模型即换列名，不走 schema 演进状态机）。
- **hnsw 索引**：`CreateIndex type=hnsw`（单列；`distance_metric∈{COSINE, L2, INNER_PRODUCT}` 缺省 COSINE，归一大写落 catalog；orders 拒绝），DDL `USING hnsw (col vector_cosine_ops|vector_l2_ops|vector_ip_ops)`；同列可建多 metric 索引。vector 列 × key/unique/fulltext、非 vector 列 × hnsw、数组列 × hnsw 全部拒绝。

## 6. 查询（单 typed AST）

**wire 形态唯一**：List / Count / Aggregate 的过滤 / 排序 / 投影一律走 `query`（`shared.v1.Query`：`filter` 树 + `orders` + `select` + `pageSize/pageToken`）。`queries` DSL 字符串字段已 reserved；GET 面保留 `page_size/page_token` 简单分页参数，过滤条件一律 POST body。绑定链：`BindListQuery`（proto codec，`pkg/query/proto.FromProto`）→ `ResolveQuery`（合并 GET 面分页字段 + 校验）→ infra `astFrom`（归一后再校验）。

**算子全集**（`Filter` oneof，`pkg/query` 常量同源）：`eq ne lt lte gt gte in between notBetween isNull isNotNull contains notContains startsWith notStartsWith endsWith notEndsWith search notSearch containsAny containsAll` + `and/or`。嵌套深度 ≤8；无通用 NOT——取反全部由 not* 变体承担（索引友好，德摩根展开可表达）。值数量约束：比较族 ≥1（eq / ne 多值自动进 IN / NOT IN 语义）、between / notBetween 恰 2、isNull / isNotNull 为 0、containsAny / containsAll ≥1（数组字面量）。

| 类 | 算子 | SQL |
|---|---|---|
| 过滤 | `eq` / `ne` / `in` | `=` / `IN` / `NOT IN` |
| | `lt` / `lte` / `gt` / `gte` / `between` / `notBetween` | `<` / `<=` / `>` / `>=` / `BETWEEN` / `NOT BETWEEN` |
| | `contains` / `startsWith` / `endsWith`（及 not* 变体） | `ILIKE '%v%' ESCAPE '\'`（`escapeLikePattern` 转义 `%_\'`） |
| | `search` / `notSearch` | `to_tsvector('simple',col::text) @@ plainto_tsquery('simple',?)` |
| | `isNull` / `isNotNull` | `IS NULL` / `IS NOT NULL` |
| | `containsAny` / `containsAll` | `col && ?::T[]`（交集非空）/ `col @> ?::T[]`（子集）。**仅 array=true 属性可用**（白名单，标量列 / 系统列拒绝）；参数按列元素类型 cast；NULL 列与空数组列不命中 |
| KNN | `vectorSearch`（**非 filter 节点**） | `SELECT …, (col <op> $vec) AS __dist … WHERE <policy+filters> ORDER BY col <op> $vec LIMIT k`；op：COSINE `<=>` / L2 `<->` / INNER_PRODUCT `<#>`（负内积） |
| 排序 | `orders[]`（attribute + desc） | `ORDER BY d.k1 dir1, …, d._id <首键方向>`（与 cursor 续页同构的 `_id` tiebreaker） |
| 分页 | `pageSize` / `pageToken` | LIMIT；**keyset-only**：`pageToken` 只认 `ka:/kb:` token。count / aggregate 对排序 / 分页算子显式拒绝（整集语义）。KNN 下 pageSize 即 k、pageToken 认 `kvc:` 距离游标（见下文） |
| 投影 | `select[]` | 返回后裁剪 `Data` |

别名：`$id→_id`、`$createdAt→_created_at`、`$updatedAt→_updated_at`、`$version→_version`（`mapQueryField`）。

**DSL 是客户端糖**：`pkg/query.Parse/ParseMany`（含 `ToWireJSON`——AST→protojson 形态，CLI 用）与 `sdk/go/query.FromDSL` 在客户端把 DSL 串解析为 AST 后发送；服务端零消费。程序化构造用 `pkg/query` 构造器（`query.Eq/Gt/Between/IsNull/And/Or…`）或 Go SDK 的链式 `Builder`。

**输入上限**（`internal/infra/documentdb/postgres.go` + `internal/app/documents`）：

- AST 叶数 ≤100（`pkg/query.MaxQueries`）；eq / in 多值 ≤1000；**跨 filter 绑定参数累计 ≤2000**（封死 PG 65535 语句参数上限）；页大小上限 100（clamp）。
- **写入载荷**：总量 ≤1 MiB、单属性值 ≤256 KiB，超限 `DOCUMENT.TOO_LARGE`（InvalidArgument，违规属性定位走 BadRequest violations）。
- `_acl` ≤64 ACE（`DOCUMENT.ACL_TOO_LARGE`；校验在 app 层写路径——create / update / upsert / bulk / execute-tx 全覆盖；种子 ≤3 条天然合法；RLS / adapter 不设防，防御纵深在列授权与函数通道）。
- 数组值 ≤1000 元素（data 通道数组值 + array_updates 的 values；DDL 通道无此面——array=true 拒绝 default_value）。
- 每集合列数软限 200（CreateCollection 一次性声明 / CreateAttribute 存量+1 前置拒绝，`CATALOG.COLUMN_LIMIT_EXCEEDED`；PG 1600 列硬限留余量）。
- object 嵌套 ≤8 层（`ValidateDocumentPayload` 内校验；map 计一层、数组透明不计层）。

**编译与校验**（`postgres_query_compile.go`）：`validateQueryFields` 白名单 = 系统列 + 已声明 attribute；`search` 需命中 fulltext 索引；`containsAny/containsAll` 需命中 array=true 属性；`_version` 缺列返回 `version_column_unavailable`；系统集合敏感列（`users.password_hash/prefs/labels` 等）黑名单仅按 `IsSystemCollection` 生效。

**数组写侧原子算子**：`UpdateDocumentRequest.array_updates`（client + server 双面；execute-tx op 同型字段仅 update 消费）编译为单语句 SET 子句，与 data / increment 可组合（同列冲突 → InvalidArgument）、OCC 不变。八算子：

| 算子 | 语义 |
|------|------|
| `APPEND` / `PREPEND` | 尾插 / 头插（`COALESCE(col,'{}') || ?::T[]` 及逆序） |
| `REMOVE` / `DIFF` / `FILTER` | 差集（三者同构受限形态：移除等于任一 values 的元素；不支持条件表达式；移空后为空数组非 NULL） |
| `UNIQUE` | 保首次出现序去重 |
| `INTERSECT` | 交集（**去重**并保 col 首次出现序；移空后空数组） |
| `INSERT` | 定点插入（`index` 0 基，其后元素顺移；越界 = 尾插；NULL 列视为空数组；要求 values 恰 1 且 index ≥0。PG 18 无 `array_insert` 内建，unnest WITH ORDINALITY + UNION ALL 等价实现） |

**NULL 列语义二分**：添加类（APPEND / PREPEND / INSERT）视为空数组归一；读改写类（REMOVE / UNIQUE / INTERSECT / DIFF / FILTER）保持 NULL。data 通道对数组列是整列替换；读回经 `to_jsonb` 自动投影为 JSON 数组。**查询侧不设对应算子**（裁决：业界的 arrayIntersect / arrayDiff 本就是写侧算子，查询侧无可对齐语义；布尔谓词由 containsAny / containsAll 承担——避免自创查询语义引发 proto / pkg / SDK / golden 全链扩张）。

**vector_search 查询语义**：`query.vectorSearch = {attribute, values[], metric, maxDistance?, efSearch?}`——**非 filter 树节点**（距离不可作布尔谓词，排序由距离承载）。要点：

- `pageSize` 即 k（top-k **可见**近邻，缺省 25、上限 100）；与普通 `filter` 可组合（AND），与 `orders` 互斥（InvalidArgument）；DSL 字符串不支持（SDK typed builder only——Go `sdk/go/query.VectorSearch`、TS `vectorSearch()`）。
- 前置校验（显式拒绝原则）：目标列必须是声明的 vector 属性；`values` 与 `dims` 等长；存在与 `metric` 匹配的 hnsw 索引（无索引 / metric 不符 → InvalidArgument）。vector 属性不得进普通 filter（白名单仅放行 isNull / isNotNull）与 order。
- **ef_search 调参**：`efSearch` 合法域 **[1,500]**（越界 InvalidArgument **显式拒绝，不静默 clamp**——静默改写让调用方误以为请求值生效）。设置后查询事务内 `SET LOCAL hnsw.ef_search = N`（事务级 GUC，零残留）；**缺省不注入任何语句**，行为与 pgvector 缺省 40 逐字节一致（集成测试锁定）。近重复簇边界召回不足时调大（代价：访存 / 延迟随 ef 增长）。
- **多页 KNN**：`pageToken` 携带服务端发放的 `kvc:` 距离游标（`kvc:<dist_hex16>:<docID>`，float8 比特定长 hex 精确往返，负距离原生支持）。同请求参数 + token 翻页；满页发放、空串收尾。
- **内积方向**：pgvector `<#>` 返回**负内积**（值域 (-inf,0]）——"越大越好"取负后"越小越近"，与 cosine / L2 统一为距离升序，续页阈值方向三 metric 一致，无需按 metric 翻转。
- **切页管道**：首页保持 HNSW + iterative scan（按"完整距离组"切页——第 k+1 行证明第 k 行距离组无越页 tie 时才满页发射；同距 tie 组整组顺延、游标落组起点，防"发射 tie 真子集 + 阈值游标"漏行）。续页为 **(dist,_id) 精确全序扫描**（HNSW 索引只承载距离单键序、同距组内跨查询不稳定，故续页放弃 HNSW 换结构化扫描，不重不漏）。`maxDistance` 在每一页独立后置过滤（发射集被滤空时直接收尾）。`distances` 与 `documents` 平行回传，不污染 `Document.Data`、不持久化、不进事件；count / aggregate 拒绝 KNN。
- **iterative scan 为契约**：查询事务内 `SET LOCAL hnsw.iterative_scan = 'strict_order'`（GUC 默认 off；off = "先取全局 k 再滤"，RLS×KNN 召回错误——开发期实证 1000 行 5 可见时 off 返回 0/5、on 返回 5/5）。RLS policy 作为 securityQuals 隐式参与过滤——**vector 与文档同事务、同 RLS 判定管辖**（集成测试锁定稀疏可见性召回）。
- 已知边界：近重复不可见簇 + 极低可见率下 HNSW 图导航可能饱和、返回 < k 行（近似索引固有边界）；调参手段即查询级 `efSearch`。

## 7. 权限模型（`_acl` 内嵌 + RLS 判定执行点）

ACE 条目形如 `type:role`，`type∈{read, create, update, delete}`（`write` 展开为三写）。角色词表唯一构造与解析源是 `internal/domain/databases/docrole.go`（生产代码禁止裸串拼接文档角色，测试守门）：

- **裸词表**：`any`（合成，仅 read 可授予）/ `users`（已认证端用户）/ `guests` / `keys`（API key scope 面）；
- **命名空间角色**：`user:<id>`（可带 `/verified` 复合后缀）、`group:<gid>`（可带 `/<role>` 职务后缀——组域裸角色只有经复合才进入文档角色集，与 console RBAC 撞名被 ParseDocRole 拒绝）、`key:<id>`、`member:<mid>`、`label:<label>`；
- **sentinel**：`__private__`（纯私有占位 ACE）/ `__system__`（内部旁路投影）；
- **模板占位符**：`user:{id}` / `group:{id}`（授予持久化前经 `ExpandPermissionTemplates` 展开为调用者自身首个匹配角色，无法借模板指名他人）。

裸 `admin` 不在词表（console admin 主体必为 PlatformAdmin 走 BYPASSRLS，该 ACE 是死语义）。`ExpandPermissionRoles` 无条件注入 `any`；已认证端用户注入 `users`。

**存储**：文档 ACE 内嵌 `_acl TEXT[]`（元素 `"type:role"`；空数组回退集合级权限）。集合级权限与 `documentSecurity` 存 catalog，policy 经 InitPlan 子查询**实时读取**——集合级权限变更零 DDL 即时生效。**读回免费**：`to_jsonb(d.*)` 载荷已含 `_acl`，解析为 `Document.Permissions`（List / Get 零额外查询）。

**per-key 私有**：API key 主体的角色集为 `keys` + `key:<自身id>`——`keys` 承载 scope / API 面（集合默认权限、特权授予判定），`key:<id>` 承载数据隔离身份。空 ACE 种子对 API key 主体绑 `read/update/delete:key:<自身id>`（与 user 主体 owner ACE 同构）——**默认私有**：keyA 建的文档 keyB 不可见（Get = NotFound 防枚举），跨 key 协作需显式授予 `key:<id>` ACE。集合默认权限里的 `keys` 四连不受影响。

### 7.1 判定执行点 = RLS policy

业务集合建表即生成四条 policy + `ENABLE/FORCE ROW LEVEL SECURITY`（`rls_policy.go`；DDL touch 由 reconcile 自愈）：

- **函数单源**（public 迁移 000004）：`tw_can(acl, roles, typ, coll_allows)`（= `AllowsDocumentAccess` 用户集合分支：write 展开 + 空回退 + 零角色 fail-closed）；`tw_coll_allows(perms, roles, typ)`（集合级 JSONB 判定）；`tw_visible`（可写即可读：read ∨ update ∨ delete 命中；docSec=false 纯集合级；空 `_acl` 快速路径）；`tw_roles()` / `tw_tenant()`（**SECURITY DEFINER 验签函数**：仅 tw_app 身份、`app.roles_sig` 未过期时解包 `app.roles` GUC 为 text[]、`app.tenant` GUC 为 bigint；sig 缺失 / 格式错 / 过期 / 验签失败 / 密钥缺失 → 空数组 / NULL = 零角色 fail-closed。密钥 = `HMAC-SHA256(security.jwt.secret, "tw-roles-guc-v1")`，Go 进程启动期派生（`bootkit.InitRolesSigSigning`）；落 `tw_secrets` 由部署期 owner 一次性作业 `torchwood admin sync-roles-sig` 完成（表不授予任何角色，运行 DSN 零权限）。**双钥轮换**：`tw_secrets` current / previous 槽位，`tw_sig_match` 任一钥命中即通过，换钥时旧 current 降级 previous 而非删除——滚动重启换钥窗口内旧 sig 验签通过，窗口外（exp 过期）依旧拒绝）。sig 消息覆盖 `tenant|roles|exp` 三元组——跨租户 / 跨项目伪造在验签层死锁。SQL golden 矩阵锁语义（`rls_policy_test.go`），禁止 Go 侧等价实现。
- **四条 policy**：SELECT USING = 空 `_acl` 快速路径 ∨ `tw_visible`；INSERT WITH CHECK = 集合级 create；UPDATE USING = CASE docsec → `tw_can(update)` ELSE 集合级，WITH CHECK = 恒真（`_acl` 实际不经主语句 UPDATE 写，见 7.3）；DELETE USING 同构。
- **连接模型**：文档面入口（读写同构，autocommit 退役）经 `withDocumentTx` 包进带身份事务——事务开启前解析项目 internal_id 填入身份（sig 消息覆盖 tenant），首条 `SET LOCAL ROLE`（`tw_app`；SystemPrincipal / PlatformAdmin → `tw_system` BYPASSRLS；DDL → `tw_owner`）+ `set_config('app.roles', …, true)` +（tw_app 且密钥已初始化）`set_config('app.roles_sig', …)` / `set_config('app.tenant', …, true)`，多语句合并单往返。漏注入 = 零角色 = policy 恒 false（fail-closed）。中段身份切换（尾随读回）退出前恢复外层身份。
- **应用层判定退役面**：业务集合的 `checkDocumentPermission` / `listPermissionFilter` / 批量预取校验全部退役——policy 隐式过滤即判定。**sentinel 系统集合保留应用层判定**（`AllowsDocumentAccess` + `_acl` 谓词过滤——静态平面独立授权）。`ensureCollectionAccessible`（disabled 拦截）与授予治理（`ValidateGrantablePermissions`）保留在用例 / 入口层。

### 7.2 各操作的检查点

| 操作 | 检查点（业务集合 = policy；sentinel = 应用层） |
|---|---|
| `CreateDocument` | INSERT WITH CHECK（集合级 create；拒绝 42501 → PERMISSION_DENIED）+ sentinel 写保护拦截 |
| `GetDocument` | SELECT policy；不可见 = 0 行 → NotFound（防枚举） |
| `UpdateDocument` | UPDATE USING（`tw_can(update)`）；0 行三态探测：不可见→NotFound / version 不符→VERSION_MISMATCH / 可见不可写→PERMISSION_DENIED |
| `DeleteDocument` | DELETE USING + DELETE 语句内 `_version` 守卫（compare-and-delete；无锁预读，防 FOR UPDATE 叠加 UPDATE policy 误拒 delete-only 用户） |
| `ListDocuments` / `CountDocuments` / `Aggregate` | SELECT policy 隐式过滤（聚合过滤先于 GROUP BY，securityQuals 机制保证） |
| `UpsertDocument` | 预查（经 SELECT policy）分支：纯插入 → INSERT WITH CHECK；命中 → UPDATE USING（upsert 需同时持有 create 与 update，语义有意收紧） |

### 7.3 `_acl` 写入路径（插入携带 + 替换函数通道）

PG 实证：UPDATE / ON CONFLICT 修改 SELECT policy 引用的列（`_acl`）会触发 SELECT policy 对**新行**的复检——`WITH CHECK(true)` 无法单独保自锁。因此 `_acl` 写入按行生命周期分两条治理通道：

- **插入通道**：create / upsert 插入支的 INSERT 直接携带 `_acl`（新行无旧行，SELECT policy 新行复检不适用；`_acl` 在 tw_app 的 INSERT 列授权内）。内容治理在 app 层授予校验（信任等价于"自己创建的内容"）；
- **替换通道**：既有行的 `_acl` 替换唯一走 **`tw_set_document_acl(p_schema, p_table, p_tenant, p_doc, p_acl)`**（迁移 000004，SECURITY DEFINER owner=`tw_system` BYPASSRLS 绕开新行复检；EXECUTE 仅授 `tw_app`）。update / upsert 更新支 / bulk / execute-tx update op 的替换全部经函数通道（同事务、当前 tw_app 身份）。函数内三道校验：`p_table` 经 catalog physical_name 白名单（按 `p_schema` 反解 (project, database) 三元组收窄，防注入防同名 21000）；`p_tenant` 必须等于验签 tenant（跨租户 / 跨项目在签名层死锁，不满足 RETURN 0）；目标行 `tw_visible` 可见性（堵"改他人 ACL 提权"，不可见 / 行缺失 RETURN 0）。
- tw_system 身份（SystemPrincipal / PlatformAdmin 的 `_acl` 替换）经表级 ALL 直写（tw_system 无函数 EXECUTE，BYPASSRLS 语义等价、无新增提权面）。

同理 ON CONFLICT 推测插入要求拟插入行过 SELECT policy——upsert 拆预查分支 + 普通 INSERT/UPDATE（advisory lock 保证同冲突键串行；与并发普通 Create 撞唯一键改报 DuplicateKey，可重试）。

**列级 GRANT**：`tw_app` SELECT 全列 + INSERT 数据列与除 `_tenant` 外系统列（含 `_acl`——插入通道）+ UPDATE 数据列与除 `_tenant`/`_acl` 外系统列（`_tenant` 锁死不可写；`_acl` 直改旁路从 UPDATE 列权限封死）；`tw_system` 表级 ALL。`_version` 不锁列（CAS 守卫 `WHERE _version=?` 已足）。

### 7.4 授予治理与可见范围归属

`ValidateGrantablePermissions`（`internal/domain/databases/permissions.go`）：普通用户不可授予未持有角色与 `any` 写权限（`keys` / System / PlatformAdmin 跳过）；覆盖 create / upsert / update / bulk 全部写路径（Client 侧 `WriteOptions.AllowPrivilegedGrant=false`）。`create` 类 ACE 例外放行——行级 create ACE 在 INSERT 后无判定语义，不构成提权面。

端用户可见范围归属的安全语义三问：

- **定向共享给特定用户 → 拒绝**。用户 A 不持有 `user:B`，无法给自己的文档挂 `read:user:B`；`group:` / `label:` 同理，只有自身也持有（属于该组）才能授——**组内共享是组语义本身，不是越权**。模板 `user:` / `group:` 只展开为调用者自身首个匹配角色，无法借模板指名他人。
- **广播公开 → 合法**。`read:any`（读类合成角色授予放行）= 凡走数据面判定的主体均可见；`read:users` = 所有登录端用户可见（API key 主体不含 `users` 角色）。写类对 `any` 一律拒绝。**默认收口**：Client 创建不带 permissions 时包装层盖 owner ACE（`read/update/delete:user:<自身>`）；key 主体种子见 per-key 私有——不显式公开就只有自己可见。
- **写到他人名下 → 不可能**。`_created_by` / `_updated_by` 由服务端从 Principal 戳入（端用户存裸 user id、API key 存 `key:<id>`，请求不可传）；`_tenant` 列锁死；改他人文档需目标行 `tw_can(update/delete)`；`_acl` 替换唯一通道 `tw_set_document_acl` 复核目标行 `tw_visible` + 验签 tenant（堵"改他人 ACL 提权"与跨项目伪造）。

**两条应用侧边界**：

1. `documentSecurity=false` 是总开关——集合级权限模式下文档级 `_acl` 完全不参与判定，可见范围由开发者配置的集合级权限决定，端用户请求中的 permissions 字段无效。
2. 权限内核管"谁能读写哪些行"，不校验行内数据字段的语义归属——`userId` 类自声明属性属不可信输入，应用须服务端派生（Me / JWT）或查询收口（`eq("userId", 当前用户)` + 集合级权限）。业务不允许用户私自公开内容时：关 `documentSecurity` 走集合级权限，或在入口层过滤请求中的 permissions。

## 8. OCC（`_version`）

用户集合 `_version BIGINT NOT NULL DEFAULT 1`：

- Create / Upsert / Bulk 盲写但 `_version+1`（Bulk `SkipVersion=true` 为 LWW 语义）；Update / Delete 必填且等于当前值（行锁下比较），成功 +1。
- 错误码：`version_required`（缺省）/ `version_mismatch` / `version_column_conflict`（FailedPrecondition）；`version_invalid`（显式 ≤0，InvalidArgument——与缺省态不同码）；`version_column_unavailable`（InvalidArgument）。OCC 冲突错误体携带探测读到的当前 `_version`（`VersionConflictError.CurrentVersion` → ErrorInfo metadata `current_version`，调用方零额外读回即可合并重试）。
- `_version` 可作过滤 / 排序 / 投影；系统表无此列。
- Upsert 的 `conflictColumns` 必须无序命中集合一个 unique 索引（非 Bypass 主体前置校验，否则 InvalidArgument；Bypass 主体靠 PG 42P10 兜底）。

### 8.1 事务内核 execute-tx

`DatabasesService/ExecuteTransactions`（Server 面）：单事务内顺序执行异构 op 批（`postgres_transactions.go`，Bulk 的泛化）。op 模型 `{type(create/update/upsert/delete), collection_id, document_id, data, permissions, increment, array_updates, expected_version, conflict_columns}`（array_updates 仅 update 消费，语义与单文档 API 同源），上限 1000（`databases.MaxTransactionOps`，与 Bulk 上限同值同源）。

- **锁纪律**：按 (collection, documentID) 排序预取 `pg_advisory_xact_lock` 防批间死锁；op 按请求序执行（事件序 = op 序）；各 op 复用单文档事务体（权限 / OCC / conflictColumns 校验同源）。
- **模式**：`ATOMIC`（默认）任一失败整批回滚（错误带 op index 域码定位）；`PARTIAL` 逐 op SAVEPOINT 容错、已成功不回滚、返回 per-op 结果（含失败域码）。
- create / upsert 空 ACE 种子与单文档 API 同语义。
- **Functions 的多写原子性**：函数代码运行在外部 Docker 容器，不做跨进程事务——函数内的多写原子批一律经本 RPC 表达（函数通过 API/SDK 调用 `documents:execute-tx`），批内事件序 = op 序，成功全提交、失败（ATOMIC）整批回滚。

### 8.2 域错误码

域码稳定 snake_case（`DOCUMENT.NOT_FOUND`、`DOCUMENT.VERSION_CONFLICT`、`IDEMPOTENCY.KEY_CONFLICT` 等，`internal/domain/databases/errors.go`）静态映射 gRPC code。消息格式 `CODE: message`；ErrorInfo detail 携带 `reason` / `retryable`（OCC 冲突、资源耗尽与幂等执行中可重试）与 `error_id`（DomainStatus 生成处统一注入——每个错误实例可唯一引用；限流拒绝为 `RATE_LIMIT.EXCEEDED` + RetryInfo 精确退避）。

字段级违规定位走 **google.rpc BadRequest 标准 detail（field_violations）**：execute-tx 的 op 定位为字段路径形态（`ops[3].expected_version`——域码映射子字段：VERSION_*→`expected_version`、PERMISSION_DENIED→`permissions`、NOT_FOUND/ALREADY_EXISTS→`document_id`、无映射→`ops[N]`）；载荷违规（`data.blob`）与 op 守卫同形态。裸 "document database error" 已消灭；infra 产出的域码 status 在 app 层经 `errors.As` 提取透传（防包装链丢 status）。

### 8.3 聚合 documents:aggregate

`DatabasesService/AggregateDocuments`（Server 面，scope `databases.read`）：`POST .../documents:aggregate`，支持 `sum/avg/min/max` + 可选单键 `group_by`（count 走独立 `:count` RPC）。

- **可见性**：聚合一律在 SELECT policy 过滤后的可见行集上执行（过滤先于 GROUP BY）——不可见行不进聚合、group 键不泄露；权限 golden 集成测试锁语义。最小桶 / k-匿名未实现（可选产品功能，默认关）；权限变更前后聚合结果不可比属固有属性。
- 聚合目标必须是集合声明的数值属性（integer / float，System 主体一视同仁防拼列）；`group_by` 须为已声明属性，键按 text 序列化（NULL 键 = 属性未设置的行，归入 `group_key` 未设置的组）。
- **空集语义**：`sum=0`（COALESCE，类型跟随属性）、`avg/min/max` 无值（proto oneof 未设置）；`group_by` 下空集返回空组列表；无 `group_by` 时恰返回一组。
- **结果类型化**：`AggregateValue.result` 是 oneof——integer 属性的 `sum/min/max` 走 `int64_value`（`>2^53` 精确）；`avg` 恒 `double_value`；float 属性恒 `double_value`。integer 聚合超出 int64 → InvalidArgument / `AGGREGATE.OVERFLOW`（PG 22003 翻译）。
- **Data 的 double 精度界**：文档 Data（JSON Struct）的 number 通道是 double——业务值可能超过 2^53 时请用 **string 属性承载**；聚合面经 `int64_value` 已绕开该界，读写两个通道各自精确。
- 过滤算子与 ListDocuments 同形；排序 / 分页算子在 count 与 aggregate 一律 InvalidArgument 显式拒绝（整集语义）。

### 8.4 写幂等 request_id

七类写操作（Create / Update / Upsert / Delete / BulkUpdate / BulkDelete / ExecuteTransactions——Server 与 Client 面共用同一 app 核；HTTP 面等价 `Idempotency-Key` 头，proto 字段优先）在 `internal/app/documents` 核层包裹：

- **键作用域** `(project_id, actor_id, request_id)`；`actor_id` 复用归因链（`Principal.StableActorID`：端用户 / console admin 存裸 id、API key 主体 `key:<id>`、内部 System `system`）；不同 actor 同 key 不冲突。
- **指纹** = method + 请求关键字段规范序列化 sha256（批 ID / conflict_columns 排序规范化——集合语义，重试乱序不判冲突）；同 key 不同指纹 → `IDEMPOTENCY.KEY_CONFLICT`（InvalidArgument，重试无意义）。
- **语义**：只缓存成功响应（失败释放占位，重试重新执行）；重放返回原响应 + `x-torchwood-replayed: true` 响应头（gRPC metadata，网关透传为 HTTP 头，零 proto 侵入）。并发同 key：短轮询（100ms）≤2s 后仍 in-flight → `IDEMPOTENCY.IN_PROGRESS`（Aborted，retryable）。Complete 失败不回滚写入（只损失重放能力）；store 故障时写请求失败（Unavailable，不静默降级）。
- **存储** `public.idempotency_keys`（PK 仲裁并发认领 + `claim_token` 防过期重认领串写 + `expires_at` 索引惰性清理）；done TTL 24h、in_flight 兜底 TTL 5min。execute-tx 幂等覆盖整批（PARTIAL 重放返回首次完整 per-op 结果）。

### 8.5 事件链（outbox seq + Stream 位点 + 补偿）

写路径同事务 `INSERT document_events_outbox`（`event_id` PK = 幂等去重键）；唤醒信号由 AFTER INSERT 行级触发器发 `pg_notify('tw_outbox','')`（空载荷纯信号，随 commit 投递、回滚即丢弃；零额外客户端语句；同事务多次相同 NOTIFY 被 PG 自动合并——execute-tx 100 op 批只投递一次唤醒）。

- **seq**：`BIGINT GENERATED ALWAYS AS IDENTITY` + UNIQUE。顺序承诺：**单文档全序**（行锁保证 seq 随提交序）；**集合内分配序**（跨文档不保证与提交序一致）；**seq 空洞 = 回滚事务消耗 identity，不丢事件**。seq 仅作续传游标与去重辅助，不承诺跨集合因果。表另带显式 `channel` 列（经济 / 系统行为事件落扇出频道，文档事件 NULL）与 `(project_id, topic, seq)` 复合索引（迁移 000012，把 `:changes` / WS 重放扫描收窄到项目内）。
- **投递**：worker `LISTEN tw_outbox`（专属连接自带重连）+ 5s 兜底轮询，SKIP LOCKED 批拉 256（按 seq 排序；XADD 失败指数退避重试，超限迁死信表 `document_events_outbox_dead`；领取后 2min 未 published 的行整进程挂死兜底重投）→ XADD `torchwood:events`（载荷 = 完整信封 JSON 含 acl + seq）→ **每 server 实例一个消费组**（组名 = `hostname:pid`）XREADGROUP → hub 扇出 → 批量 XACK → `published_at` 攒批回写（200ms / 32 条）。组从 `$` 起步（新实例不回放历史——断线窗口由客户端 last_seq 重放补齐）；PEL 挂起条目 idle>15min 由 XAUTOCLAIM 重投（重复经 hub 去重窗口 5min + 客户端幂等吸收）；闲置孤儿组（实例崩溃残留）由周期清理销毁。worker 清理周期 `XTRIM MAXLEN ~100000`（Stream 只是投递通道，重放窗口在 outbox 表——published 行 24h 清理覆盖 1h 承诺）。函数事件触发器以独立消费组 `functions-triggers` 消费同一 Stream，停机补投经 outbox 表按 seq 区间扫描。
- **信封**：载荷上限 1MiB（对齐文档写入上限；超限仅防御性截断 + `truncated=true`）；`transaction_id` 非空表示来自 execute-tx 原子批（批内事件顺序 = op 序）；`seq` 随帧下发。
- **`:changes` 补偿 API**（Server / Client 两面，scope `databases.read`）：`GET .../collections/{coll}/changes?since_seq=&limit=`（limit 缺省 / 上限 500）返回该集合 `seq > since_seq` 的**已提交**事件，seq 升序、按请求者可见性过滤（与 hub 扇出同语义）；`has_more=true` 以续传游标（优先服务端发放的扫描位置 `next_since_seq`，越过已判不可见的块）续传。delete 事件天然 tombstone（无 data，带 document_id + version）。`since_seq` 早于最老可用事件 → `EVENTS.RESUME_EXPIRED`（指引全量重拉后重新续传）；`since_seq=0` = 从最老可用事件起。
- **跨项目事件隔离**：topic / 频道名 `databases.<db>.collections.<coll>` 是**全局命名空间**（不含 project 维度），跨项目同名集合共享同一 topic。隔离由消费两端强制 project 过滤保证：`:changes` 按 `outbox.project_id` 等值过滤；Hub 扇出按**连接归属项目**等值过滤（`RealtimeConn.ProjectID` 来自 WS 握手 `hello.project_id`——门控已校验其与凭证一致，即信任锚；不等直接跳过，不做 ACL 评估；空归属 fail-closed）。platform admin 同样受此约束——要看他项目事件须以该项目开连接。若未来引入跨项目同名集合的合法共享场景，须重新评审本决策。
- **WS `last_seq` 重放**：subscribe 帧可选 `last_seq` → 门控订阅（补发 outbox 窗口内事件，单次上限 500，超出则 `subscribed` 帧带 `has_more=true` 指引 `:changes` 续传）——补发帧先于实时帧、无漏帧窗口；窗口外同样 `EVENTS.RESUME_EXPIRED` error 帧（订阅失败、连接保持）。仅 databases 频道支持（realtime 频道第二族 `accounts.<userId>` 为经济事件显式频道，按频道本身鉴权，不支持 last_seq 重放）。
- **慢消费者水位断开**：每连接 send buffer 1024 帧；满水位 → close reason `resync:<last_seq>` 主动断开（客户端重连带 last_seq 即天然重同步）。SDK 端：按频道跟踪 payload seq、重连带 `last_seq`、resync close 零退避立即重连、`EVENTS.RESUME_EXPIRED` 默认清游标（可注入回调）。

## 9. 事务与分页一致性

`BulkUpdate/BulkDelete` 与单文档写走 `withDocumentTx`（带执行身份的显式事务——每请求一事务，注入 `SET LOCAL ROLE` + `app.roles`，中段身份切换退出前恢复）；批量缺失行经 `missingRowsError` 探测区分 PERMISSION_DENIED（可见不可写）与 NotFound。

`ListDocuments` **keyset-only（多键）**：

- 首页（无 cursor）执行精确 COUNT 后主查询（非原子快照，`READ COMMITTED`），满页发 `ka:` token；续页跳过 COUNT（`total=0=unknown`），满页判定 has-more。
- 排序 = 全部排序键 + `_id` tiebreaker（方向随首键）。
- keyset 谓词：方向一致（全 ASC / 全 DESC）走行比较 `(k1,…,kn,_id) op (?,…,?)`；方向混合走逐键 OR 展开（`k1 OP1 ? OR (k1 = ? AND k2 OP2 ?) OR …`）——两种形态与 ORDER BY 全序严格一致（跨页不丢不重）。
- cursor 仍以 docID 定位：服务端按 docID 查行取全部排序键值（token 只编码 `ka:/kb:` + docID）。

**NULL 排序键的已知限制**：行比较谓词对 NULL 求值为 NULL（行被排除）——cursor 行的排序键含 NULL → InvalidArgument（消息明示先 isNull/isNotNull 过滤再分页）；数据行含 NULL 键在续页中被跳过。不做 NULLS LAST 谓词改写（正确性代价不成比例）。

`pkg/crud` 提供 `ParseListParams` / `BuildPaginationInfo`；offset token（base64 JSON，HMAC 签名可选启用，TTL 24h）仅供静态表 / 控制面列表使用，文档面不再接受。

## 10. 系统列与写入过滤

| 列 | 说明 |
|---|---|
| `_id` | 文档主键，`idgen.UUID()` 默认，`^[a-zA-Z0-9_.:-]{1,64}$` |
| `_created_at` / `_updated_at` | 自动维护（`NOW()`） |
| `_created_by` / `_updated_by` | 归因主体：端用户 / admin 存裸 id；API key 主体存 `key:<keyID>`；其余留空。归因身份与空 ACE 种子同一命名空间——文档属主可直接从 `_created_by` 读出协作授予目标 |
| `_acl` | 内嵌文档 ACE（TEXT[]，元素 `"type:role"`；空数组回退集合级）。插入通道随 INSERT 携带；既有行替换唯一通道 `tw_set_document_acl`；对 `tw_app` 的 UPDATE 列级授权排除 |
| `_tenant` | 租户标签；**对 `tw_app` 列级锁死不可写**（SELECT 可读——查询谓词需要） |
| 用户输入 `_` 前缀字段 | **fail-closed 显式拒绝**：app 层 `ValidateDocumentPayload` / `ValidateIncrement` 前置早拒，infra `validateDataKey` 二道防线——非法键（`_` 前缀 / 不匹配 `safeNameRe` / 超 63 字节）一律 InvalidArgument，不静默丢弃（静默丢字段使客户端误以为写入成功） |
| `documentSecurity` / `disabled` | 目录层控制：`disabled=true` 时非 Bypass 主体一律 PermissionDenied |

写保护：`isWriteProtectedSystemCollection` 仅对 sentinel 库（`databaseID=="_"`）的 `users/sessions/identities` 生效，业务库同名集合不受影响。

## 11. 常见用法示例

```go
// 创建集合后增属性
docDB.CreateCollection(ctx, pid, "app", "posts", perms, false)
docDB.CreateAttribute(ctx, pid, "app", "posts", Attribute{Key: "title", Type: "string", Size: 128, Required: true})

// 单 AST 查询：程序化构造器（pkg/query）+ Query 结构体字面量
ast := &query.Query{
  Filter:   query.And(query.Eq("status", "published"), query.Gt("views", "100")),
  Orders:   []query.Order{{Attribute: "$createdAt", Desc: true}},
  PageSize: 25,
}
docs, total, nextToken, _ := docDB.ListDocuments(ctx, pid, "app", "posts", databases.Query{AST: ast}, principal)

// 多键排序 + 游标：全部排序键 + _id tiebreaker；续页把上页 nextToken 填进 PageToken
ast = &query.Query{
  Orders:   []query.Order{{Attribute: "priority", Desc: true}, {Attribute: "title"}},
  PageSize: 25, PageToken: nextToken,
}

// DSL 串是客户端糖（SDK/CLI 解析为 AST 后发送，服务端零消费）
parsed, _ := query.ParseMany([]string{`equal("status","published")`, `limit(25)`})
wire := parsed.ToWireJSON() // CLI/工具直连 JSON 请求面

// 数组列：查询 containsAny/All + 写侧原子算子
ast = &query.Query{Filter: query.ContainsAny("tags", "go", "db")}
docDB.UpdateDocument(ctx, pid, "app", "posts", databases.DocumentUpdate{
  Document: databases.Document{ID: id},
  ArrayUpdates: map[string]databases.ArrayUpdate{
    "tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"new"}},
  },
  ExpectedVersion: version,
}, principal)
```

静态表 / 控制面列表用 `pkg/crud`（见 `09-api-guide.md`）：`ParseListParams` 校验 `page_size/page_token/filter/order_by`，`BuildPaginationInfo` 产出 `HasNext/NextOffset`，handler 用 `EncodePageToken` 编码 `next_page_token`。

## 12. 测试地图与参考

集成测试经 `internal/pkg/testutil/db.go:SetupTestDB` 按 `TORCHWOOD_TEST_DATABASE_SOURCE` 创建隔离库；`testing.Short` 跳过。**并行安全契约**：隔离库建库 / 迁移 / 删库段持集群级 advisory lock 跨进程互斥（`go test -p N` 每包一独立进程，进程内锁无效）；000004 三角色为集群级共享对象——up 原子幂等创建、down 仅清理本库不 DROP ROLE；契约全文见 `testutil/db.go` 包注释。

按子系统分组的测试锚点（各组内含 golden 矩阵、往返与行为级断言，函数名从略）：

| 子系统 | 测试文件 | 锁定内容 |
|--------|----------|----------|
| catalog / 物理表名 | `postgres_catalog_global_test.go`、`postgres_physical_name_test.go`、`physical_name_cache_test.go`、`internal_id_cache_test.go`、`catalog_resolve_bench_test.go`、`migrations_cycle_test.go` | codec 往返、GetCollection 单查询、physical_name = collection_id 投影、缓存失效面、迁移 up/down 对称 |
| `_acl` 内嵌 + 权限 | `permissions_test.go`、`outbox_test.go` | 空回退 / write 展开 / 租户隔离 / keys 收窄、List 权限回填零额外查询 |
| 角色分层 + GUC | `exec_identity_test.go`、`internal/infra/clients/roles_sig_test.go`、`roles_sig_dualkey_test.go`、`roles_sig_keyplane_test.go` | 注入正确性与事务外零残留、fail-closed、双钥轮换窗口、tenant 绑定 / 可见性门 / 注入面 |
| RLS 判定 | `rls_policy_test.go` | SQL golden 三层矩阵 + 行为级可见性矩阵 + EXPLAIN InitPlan 门禁 + 10 万行基准 |
| 事件链 | `outbox_seq_test.go`、realtime `subscriber_test.go` / `stream_test.go` / `hub_replay_test.go` / `hub_isolation_test.go`、`postgres_changes_test.go`、`postgres_changes_isolation_test.go`、api 层 realtime `handler_replay_test.go`、servergrpc `changes_test.go` | seq 单调与回滚空洞、双组消费 + ACL 过滤 + 项目隔离、重放门控顺序、resync 水位 |
| 查询 | `pkg/query/query_test.go`、`pkg/query/proto/proto_test.go`、`postgres_query_compile_test.go` | DSL 糖、每算子编解码往返、每算子 SQL 形态、多键游标不丢不重 |
| 多页 KNN | `vector_search_test.go`、`vector_ef_search_test.go`、`hnsw_index_test.go` | 三 metric 确定性拼接 == 单页大 k 全序、tie 组整组顺延、稀疏可见性召回、kvc 游标往返、ef_search 缺省零注入 |
| DSL 文法 parity | `pkg/query/testdata/dsl_ast_golden.json` | 根模块解析器与 `sdk/go/query.FromDSL` 的共同仲裁语料（52 条）；改语料须在 commit message 给出理由，禁止单方删条目 |
| 在线 DDL + 对账 | `postgres_online_ddl_test.go`、`schema_reconcile_test.go` | 持锁注入并发读写不阻塞、building 残留重入、三类漂移 dry-run→repair→幂等 |
| schema 演进 | `schema_evolution_test.go` | deprecated 两段删列、copy 迁移往返与 swap、迁移窗口写拒收 |
| 数组列 | `array_columns_test.go`、`array_escaping_test.go`、`postgres_transactions_test.go` | 五元素类型 DDL、算子语义矩阵、字面量转义保真、execute-tx 数组更新 |
| 数据键 fail-closed | `internal/app/documents/data_key_test.go`、`data_key_guard_test.go`、`data_plane_schema_test.go` | 非法键显式拒绝（app 前置 + infra 二道防线）、`_` 前缀保留 |
| 导入导出 | `export_import_test.go` | NDJSON 往返保真、snapshot_seq 与 `:changes` 续接闭合 |

参考：`internal/domain/databases/`（端口、Principal 与角色词表）、`internal/infra/documentdb/postgres*.go`、`pkg/query/proto/proto.go`（typed AST）、`db/migrations/` + `internal/infra/projectschema/`、`AGENTS.md` §数据库约定。

## 相关文档

- `16-document-modeling.md` — 应用侧建模指南（引用模式、删除卫生、查询陷阱）
- `05-authentication.md` — 认证期 Principal 与角色注入的上游
- `13-operations.md` — 双账号契约、superuser 豁免面 runbook 与规模预警


