# 06 数据库：三类库、标识与动态文档

> 面向后端开发者：围绕 Postgres 三层 schema、标识规则、DDL 约束、静态/动态表分工与查询/权限实现。
> 源码锚点：`pkg/ident/ident.go`、`internal/infra/documentdb/`、`pkg/query/query.go`、`pkg/crud/`、`internal/domain/databases/`。

## 0 子系统定义与边界

**DocumentDB 子系统 = torchwood 的文档数据存储整体方案**：以 `Databases → Collections → Documents` 三级资源模型为核心，端到端覆盖**元数据目录（catalog）、物理表管理（DDL 与项目数据面迁移）、文档 CRUD 与查询编译、权限集成（集合/文档两级 ACL + 认证期角色注入）、事件集成（事务性 outbox → realtime 按可见性扇出）**。七张系统静态表（`users/sessions/identities/groups/memberships/buckets/files`）是它的边界邻居而非组成部分——共用项目数据面 schema 与 `Principal`/角色语义，但不走动态文档路径（`IsSystemCollection` 名单仅用于 sentinel 写保护与测试重建）。

### 模块地图（范围内）

| 层 | 模块 | 职责 |
|---|---|---|
| 领域 | `internal/domain/databases/` | Document/Collection/Attribute/Index/Permission/Principal 模型，权限判定（`AllowsDocumentAccess`/`CollectionAllows` 等），`DocumentDB` 三端口（`repository.go`：Catalog / SchemaApplier / Documents） |
| 查询 | `pkg/query/`（客户端语法糖解析器 + 程序化构造器 + `ToWireJSON`）+ `pkg/query/proto/`（proto→AST 编解码） | 单 typed AST（C7）：`shared.v1.Query` 是服务端唯一消费形态；DSL 串仅作 SDK/CLI 客户端糖 |
| 适配器 | `internal/infra/documentdb/`（同包 8 文件，含 `catalog_codec.go` JSONB 编解码） | 全局 catalog 寻址（public 两表 + 物理名解析）、collection DDL、文档 CRUD（OCC/Upsert/Bulk/advisor lock）、查询编译与执行、权限 SQL 下推、SQLSTATE 翻译 |
| 应用 | `internal/app/documents/`（Client/Server 共用核）+ `internal/app/server|client` 的 Databases 用例 | 用例守卫（sentinel 拒绝/标识校验/系统集合拦截/disabled）、空 ACE 种子、grant 展开/校验、错误映射 |
| 数据面 | `internal/infra/projectschema/` + `pkg/ident/` | 项目 schema 生命周期（Apply/迁移/孤儿对账/缓存失效桥接）、两段式寻址与标识规则（≤28 字符） |
| 迁移 | `db/migrations/`（public 控制面，含全局 catalog 两表 000003）+ `internal/infra/projectschema/migrations/`（项目数据面模板，legacy 四表已随 000001 no-op + 000011 DROP 退役） | catalog/outbox 控制面演进；新项目一次性建面 + 存量 `EnsureAll` 自愈 |
| 事件 | `internal/infra/events/`（outbox + worker）→ `internal/infra/realtime/`（subscriber/hub/stream） | 写路径同事务落 `document_events_outbox`（全局 seq + `pg_notify` 唤醒）→ worker XADD Redis Stream `torchwood:events` → 每实例一消费组 XREADGROUP → hub 按快照 ACL 过滤扇出（`VisibleTo`），出站帧剥 ACL；补偿走 `:changes` / WS `last_seq` 重放（阶段④） |
| 分页 | `pkg/crud/pagination.go`（HMAC 签名 offset token，静态表/控制面列表用）+ documentdb `ka:/kb:` keyset token | 文档面 keyset-only（C2 阶段①）：`ListDocuments` 只发/只认 keyset token；offset token 族仅存于 `pkg/crud` 静态表路径 |
| 幂等 | `internal/domain/databases/idempotency.go`（端口）+ `internal/infra/bun/bunrepo/idempotency_repo.go` + `internal/app/documents/idempotency.go`（核层包裹） | `request_id` 写幂等（public.`idempotency_keys`）：只缓存成功响应、24h 重放、KEY_CONFLICT/IN_PROGRESS 域码 |
| 传输 | `internal/api/servergrpc|clientgrpc/databases.go` + `proto/server|client/v1/databases.proto` | 请求校验、authz 注解（`method_auth` + scope 表）、AST 参数绑定（`BindListQuery`）、OpenAPI 契约 |

### 范围外

Storage 对象本体（`files` 行只是元数据，对象在 S3/MinIO）、Functions 执行、账本/OAuth 目录（同在 `tw_<project>` 但独立演进）、billing 用量统计（消费 `files.SumSize`，不经过文档端口——`SumDocumentField` 已删除，`:aggregate` 的 sum 即其继任）。

### 关键不变量（变更评审锚点）

1. **租户隔离**：所有文档行访问强制 `d._tenant = ?`（`_tenant = projects.internal_id`，进程内缓存 + 删除失效桥接）；`_tenant` 列对 `tw_app` 经列级 GRANT 锁死不可写（阶段③包 C）。
2. **DDL 只走两段式**：`businessSchema` 显式拒绝 sentinel `_` 与一段式；`DROP SCHEMA` 永不指向 `tw_<project>`。
3. **同事务原子性**：文档数据行（含 `_acl`）、`_acl` 变更（`tw_set_document_acl` 函数，阶段③-b）、outbox 事件三者同事务提交，任一失败整体回滚（`_perms` 跨表已退役，阶段③包 A）。
4. **OCC**：用户集合强制 `_version`，`Update/Delete` 必填且匹配；列缺失/类型冲突 fail-closed（不落 PG 42703）。
5. **注入防御**：标识符 `safeNameRe` + `quoteIdent` 双重转义；查询值全程参数绑定；LIKE 走 `escapeLikePattern` + `ESCAPE`。
6. **判定单源（阶段③包 C）**：业务集合的权限判定执行点 = RLS policy（`tw_can`/`tw_visible` SQL 函数，public 000004；SQL golden 矩阵 `rls_policy_test.go` 锁语义，禁止 Go 侧等价实现下沉业务集合）；sentinel 系统集合保留应用层判定（`AllowsDocumentAccess`，静态平面独立授权）。policy 的 catalog 取值一律 `(SELECT ...)` InitPlan 化（EXPLAIN 门禁常驻），集合级权限变更零 DDL 实时生效。
7. **事件语义**：at-least-once；**同文档事件按 seq 全序，集合内为分配序（跨文档不保证与提交序一致）且 seq 有空洞（空洞 = 回滚事务，不丢事件）**——客户端按 `event_id` 幂等去重、以 `seq` 作续传游标（`last_seq`/`:changes?since_seq=`）；出站帧永不含 ACL 快照。投递通道（Redis Stream）只承担传输——正确性与重放窗口在 outbox 表（published 24h 清理 >> 1h 重放承诺）。
8. **默认私有**：`DefaultCollectionPermissions` 不含 `read:any`；空 ACE 文档按种子规则私有化（owner/创建者角色/`__private__`）。
9. **标识长度**：`project.id/database.id ≤ 28`（schema 名 ≤60 字节，`pkg/ident`）；**`collectionID ≤40`（`^[a-z_][a-z0-9_]*$` 小写，2026-09-06 收紧——集合 ID 同时作为物理表名，小写使 psql/pg_dump 等运维路径免引号直用）**、属性 key ≤63、索引 ID ≤40（app 层入口）。**物理表名 = collectionID（2026-09-06 勘误，redesign §4.2 阶段②随机物理名退役）**：DDL/行查询/索引名（`idx_<coll>_<id>`）走逻辑名，运维可直接可读；63 字节截断由组合校验把守（app 入口 `validateIndexNameLen` + infra 二道防线，各自合法但组合超限的索引 ID 被 InvalidArgument 拒绝）；`catalog_collections.physical_name` 列保留为 collectionID 冗余投影；sentinel 系统集合物理名 = 逻辑名（静态表）。
10. **查询单栈（C7）**：wire 只收 `query`（typed AST，`shared.v1.Query`）；`queries` DSL 字符串字段已 reserved，服务端文档查询栈零字符串解析（`ResolveQuery`/`astFrom` 无 ParseMany 回退）。算子全集 `eq ne lt lte gt gte in between notBetween isNull isNotNull contains notContains startsWith notStartsWith endsWith notEndsWith search notSearch containsAny containsAll + and/or`（嵌套深度 ≤8；无通用 NOT，取反由 not* 变体承担——索引友好；containsAny/containsAll 仅 array=true 属性可用，白名单校验）；`select` 投影。DSL 串是 SDK/CLI 的客户端糖（`pkg/query.Parse/ParseMany`、`sdk/go/query.FromDSL`），解析为 AST 后发送。跨 filter 绑定参数累计 ≤2000（封死 PG 65535 语句参数上限）。
11. **写幂等（redesign §4.1/§10.1）**：携带 `request_id` 的写请求键作用域 `(project_id, actor_id, request_id)`；只缓存成功响应（失败释放、重试重新执行）；同 key 异体 → `IDEMPOTENCY.KEY_CONFLICT`；并发同 key 短轮询 ≤2s 后仍 in-flight → `IDEMPOTENCY.IN_PROGRESS`；重放返回原响应 + `x-torchwood-replayed: true` 响应头；done TTL 24h、in_flight 兜底 TTL 5min、惰性清理。
12. **keyset-only（redesign C2 完成态）**：`ListDocuments` 只发/只认 `ka:/kb:` token；`offset()` 算子与非 keyset token 一律 `InvalidArgument`；ORDER BY = 全部排序键 + `_id` tiebreaker（方向随首键），keyset 谓词按方向行比较或逐键 OR 展开（多键游标完整支持，token 仍只编码 docID + 服务端查行取全部键值）。`ListCollections` 的 offset 分页维持到阶段②。
13. **聚合一律在可见行集上执行（redesign §11-J D1）**：`:aggregate` 的可见性由 SELECT policy（securityQuals）承载且过滤先于 GROUP BY——不可见行不进聚合、group 键不泄露；聚合目标必须是声明数值属性（integer/float）。
14. **连接模型与角色分层（阶段③包 B，redesign §3.2 A1/§4.3）**：单一变色龙 authenticator（DSN 用户，成员含 `tw_owner`/`tw_app`/`tw_system` 三角色，迁移 000004）+ 每请求一事务（含读，autocommit 退役）；事务首条 `SET LOCAL ROLE` + `set_config('app.roles', <\x1f 分隔展开角色>, true)`（漏注入 = policy 恒 false，fail-closed；`SET LOCAL` 事务结束自动失效）。SystemPrincipal/PlatformAdmin → `tw_system`（BYPASSRLS），DDL → `tw_owner`，其余 → `tw_app`；业务文档表 `ENABLE + FORCE ROW LEVEL SECURITY`（owner 亦受 policy，仅 BYPASSRLS 旁路）。**roles_sig 验签（阶段③-b，A2）**：tw_app 注入同时携带 `app.roles_sig = HMAC-SHA256(密钥, roles||'|'|exp)`（60s 窗口，密钥 = `HMAC-SHA256(jwt.secret, "tw-roles-guc-v1")`，进程派生 + 启动钩子落 `tw_secrets`；A4 双钥轮换：`tw_secrets` current/previous 槽位 + `tw_sig_match` 任一钥命中——换钥窗口内旧 sig 不降级，迁移 000004）；`tw_roles()` 为 SECURITY DEFINER 验签函数——GUC 伪造通道封死（无 sig/错 sig/过期 → 零角色 fail-closed）。已知豁免面：DSN 用户为 superuser 时绕过 policy（生产应配非 superuser 应用账号；A6 runbook 化）。
15. **可写即可读（阶段③包 C，§3.2 #10 产品语义）**：SELECT policy = `tw_visible`（read ∨ update ∨ delete 命中）；不可见行对 Get/List/Aggregate 一律"不存在"（防枚举，NotFound 取代 403）；写路径 0 行探测区分 NotFound（不可见）/PERMISSION_DENIED（可见不可写）/VERSION_MISMATCH。`_acl` 写入走 `tw_set_document_acl`（同事务函数调用，阶段③-b 唯一通道；自锁语义保留：UPDATE 修改 SELECT policy 引用列会触发 PG 新行复检，PG 18 实证；INSERT/UPDATE 列授权双向排除 `_acl`）；upsert 拆预查分支 + 普通 INSERT/UPDATE（ON CONFLICT 推测插入要求拟插入行过 SELECT policy，结构性冲突）。

## 1 三类库

| 层 | Schema 形态 | 技术 | 关键表 |
|---|---|---|---|
| `public` 控制面 + 事件脊柱 | 固定 `public` | `bun` + `golang-migrate`（`db/migrations/`） | `projects`/`admins`/`admin_projects`/`api_keys`/`audit_logs`/`provider_resource_index`/`document_events_outbox`+`_dead` + **全局 catalog 两表 `catalog_databases`/`catalog_collections`**（000003，阶段②） |
| 项目数据面 `tw_<project>` | 一段式 `tw_<p>` | `bun` + `internal/infra/projectschema/` | 静态表 `users`/`sessions`/`identities`/`groups`/`memberships`/`buckets`/`files` + 账本/Functions/OAuth（**文档目录已全局化迁出**，legacy 四表随 projectschema 000011 退役） |
| 业务文档面 `tw_<project>_<database>` | 两段式 `tw_<p>_<db>` | 原生 SQL（`documentdb`） | 每个 `database.id` 一个 schema，只放用户 collection 物理表（**表名 = collectionID**，2026-09-06 勘误；`^[a-z_]` 开头小写）；每表带 `_acl` 内嵌 ACE + GIN + RLS policy（阶段③），`_perms` 表已退役 |

`app` 是 CreateProject 缺省创建的首个业务库（普通库，可删可重建；2026-09 前缺省名为 `default`，改名不影响既有存量库）；系统静态表不再是文档集合（`internal/infra/projectschema/migrator.go` 在 `CreateProject` 同事务 `CREATE SCHEMA` + `Apply`，进程启动 `EnsureAll` 自愈）。**catalog 全局化（redesign §4.2/C1/G1）**：catalog 是 cluster 内全局的两张 public 表——`catalog_databases` 简单行 + `catalog_collections` 把 attrs/indexes/permissions 以 JSONB 列合一（含 default/size/array 全量属性契约、`physical_name`、`schema_version`、`ddl_seq` 乐观锁）；GetCollection 热路径从 3 查询（四表时代）收敛为 1；每项目四表模型与模板退役（projectschema 000001 no-op + 000011 DROP 存量，POC 无数据搬迁）。

## 2 标识与 Schema 规则

`pkg/ident/ident.go:27`：`^[a-z][a-z0-9]{0,27}$`、`MaxSchemaResourceIDLen=28`。入口 `ValidateSchemaResourceID`；对外再走 `RejectExternalDatabaseID` 显式拒绝 sentinel。

- `ProjectSchemaName(p)` → `tw_<p>`，匹配 `^tw_[a-z][a-z0-9]{0,27}$`（`projectSchemaNameRe`）——仅一段式。
- `SchemaName(p,db)` → `tw_<p>_<db>`，匹配 `^tw_[a-z][a-z0-9]{0,27}_[a-z][a-z0-9]{0,27}$`（`schemaNameRe`）——两段式。
- `IsTwoSegmentSchema(name)` 断言 DDL 目标必须两段式，与一段式不相交。
- `ident.ProjectDataPlaneID = "_"` 仅内部寻址：`documentSchema` 在 `databaseID=="_"` 时映射到 `ProjectSchemaName`；对外非法。
- `_tenant` 取 `projects.internal_id`（`postgres.go:resolveInternalID` + `sync.Map` 缓存），所有行查询强制 `d._tenant=?`。

字段/表名均 `quoteIdent` 转义（`"` → `""`），并经 `safeNameRe=^[a-zA-Z_][a-zA-Z0-9_]*$` 白名单。

## 3 两段式 DDL（`businessSchema`）

只接受两段式，永不解析一段式：

```go
schema, err := ident.SchemaName(projectID, databaseID) // 非法直接 InvalidArgument
if !ident.IsTwoSegmentSchema(schema) { return status.Error(codes.InvalidArgument, "...") }
conn.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoteIdent(schema)))
```

- `CreateDatabase` = `CREATE SCHEMA IF NOT EXISTS`；`DeleteDatabase` = `DROP SCHEMA ... CASCADE`（绝不 `DROP` 一段式 `tw_<project>`）。
- catalog 无 `database_id='_'` 行；`ListDatabases` 过滤 sentinel。
- 进程内 `projectschema.Apply` 带 `sync.Map` 就绪缓存（事务内不写缓存）。

## 4 静态表 vs 动态表

**静态表**（`tw_<project>`，`internal/infra/bun/model/*.go`）：`users`/`sessions`/`identities`/`groups`/`memberships`/`buckets`/`files`，`bun` 模型，无 `_id`/`_perms`/`_version`，经 Account / Groups / Storage 专用 RPC 读写。`SystemCollectionIDs` 仍在 `internal/domain/databases/system_collections.go` 仅用于 DocumentDB 跳过 `_version`/写保护与测试重建。`users.DocumentData()` 投影**不含 `password_hash`**（密码校验走 `usersRepo` 的 `User.PasswordHash`）。

**动态表**（`tw_<project>_<db>.<collectionID>`，每集合一张真实表）：**物理表名 = collectionID（2026-09-06 勘误，redesign §4.2 勘误登记；运维可读——psql/pg_dump 免引号直用；sentinel 系统集合同形，指向静态表）**。`catalog_collections.physical_name` 列保留为 collectionID 冗余投影，DDL 与行查询经 `resolvePhysicalTable` 单条 catalog 点查（现值=存在性判定：行缺失 → NotFound，物理表与 catalog 行同生共死；sentinel 直通零查询；bypass/System 聚合与列表路径同样可用；进程内缓存保留——点查实测占业务查询往返 ~26%）。**物理寻址字段不出现在任何 API 响应**；realtime 频道保持逻辑 collectionID。跨项目/跨库同名集合合法（表名语义局限于 schema 内），所有按 `physical_name` 反查 catalog 的 SQL（RLS policy 子查询、`tw_set_document_acl` 白名单）必须按 (project, database) 收窄。

```sql
CREATE TABLE tw_shop_app.c_ab12cd34 (
  _id TEXT NOT NULL, _tenant BIGINT NOT NULL DEFAULT 1,
  _created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  _updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  _created_by TEXT, _updated_by TEXT,
  _acl TEXT[] NOT NULL DEFAULT '{}',  -- 内嵌 ACE（"type:role" 元素，阶段③包 A；_perms 表退役）
  _version BIGINT NOT NULL DEFAULT 1, -- 用户集合有，系统静态表无
  -- 每个 attribute 一列（pgTypeFor 映射）
  PRIMARY KEY (_tenant, _id)
);
CREATE INDEX idx_c_ab12cd34_tenant_created ON tw_shop_app.c_ab12cd34 (_tenant, _created_at, _id);
CREATE INDEX idx_c_ab12cd34_acl ON tw_shop_app.c_ab12cd34 USING gin (_acl);
-- 用户集合另建四条 RLS policy + ENABLE/FORCE ROW LEVEL SECURITY + 列级 GRANT
--（tw_app 排除 _tenant 写），见 §7（阶段③包 C，rls_policy.go）。
```

目录位于 **public 全局两表**（`catalog_databases` → `catalog_collections` 合一行，attrs/indexes/permissions 为 JSONB 列，含 `physical_name`/`ddl_seq`）。`DeleteCollection` DROP 物理表即权限随行消亡（`_acl` 内嵌，无跨表清理）。**ddl_seq 乐观锁**（redesign §4.4）：元数据写路径（UpdateCollection/CreateAttribute/CreateIndex/DeleteAttribute/DeleteIndex/MigrateAttribute/RestoreAttribute/RetireAttribute）CAS 递增（`UPDATE ... WHERE ddl_seq = ?`；CreateIndex 两阶段各 +1），0 行 → `CATALOG.DDL_CONFLICT`（Aborted+retryable，R12 裁决：CAS 冲突非参数错误，对齐 IDEMPOTENCY.IN_PROGRESS 先例——调用方重读 catalog 后重试）。**schema_version 已被消费**（转出 POC B4）：copy 迁移与即时迁移 commit 各递增。**索引名** `idx_<collectionID>_<索引ID>`：物理表名 = collectionID 后（2026-09-06 勘误）组合长度重新由 `validateIndexNameLen` 把守——collectionID 与索引 ID 各自合法（≤40）但拼接超 63 字节的请求被 InvalidArgument 拒绝（app 入口 + infra 二道防线），运维仍以精确索引名直查。

**在线索引通道（转出 POC B3，redesign §2-C5）**：用户集合的 `CreateIndex` 走 catalog 两阶段状态机——事务 A（**纯 catalog DML**，条目 status=building + CAS）→ 事务外 `CREATE INDEX CONCURRENTLY IF NOT EXISTS`（独立连接 `SET ROLE tw_owner` + `lock_timeout=2s`，55P03/40P01 重试 ≤3，重试前清理失败残留的 INVALID 索引）→ 事务 B（active|failed）。CIC 不能在事务块内运行是两阶段状态机的结构性原因。事务 A 纯 catalog DML——非并发 CREATE INDEX 一律取 SHARE 锁（IF NOT EXISTS 存在性检查在开锁后），任何锁型 DDL 都阻塞并发读写，故 DDL touch 自愈（默认索引/RLS/列授权）移交 `ReconcileSchemaDrift` 对账扫描（下文 §5）；建集合时的既有索引（新表无并发读者）与 sentinel 系统集合维持事务内通道，SQL 表达式单源 `buildIndexStatement`（concurrently 开关）。索引条目 status：active（缺省省略）| building | failed（残留已清理，重入 `CreateIndex` 或 reconcile 可恢复）。

## 5 Attribute / Index 动态管理

| 操作 | SQL |
|---|---|
| `CreateAttribute` | `ALTER TABLE <物理名> ADD COLUMN IF NOT EXISTS` + attrs JSONB 追加（含 default）+ ddl_seq CAS，`required→NOT NULL`、`default→DEFAULT` |
| `DeleteAttribute` | **删列两段段一（B4 §4.6）**：attrs 条目置 `deprecated`（幂等；migrating 拒绝）——读投影屏蔽（Get/List/KNN 剥离）、查询白名单拒绝、写入拒收（create/update/upsert/bulk/execute-tx 的 data/increment/array_updates 三通道，bypass 主体豁免）；物理列与数据保留（`RestoreAttribute` 可回滚） |
| `RestoreAttribute` | 段一回滚：deprecated → active；migrating → 中止迁移（DROP 新列、任务置 failed）并恢复 active |
| `RetireAttribute` | **段二（不可逆）**：deprecated 属性 `ALTER TABLE DROP COLUMN CASCADE`（物理索引随消亡）+ 同事务清理引用该列的 catalog 索引条目 + attrs 条目移除（可重建同 key）；swap 后迁移残留旧列（latest swapped 任务 `old_physical`）的退役同入口 |
| `MigrateAttribute` | **copy 迁移任务（B4）**：收紧/改类型 = 新列 `<key>__v<seq>`（seq=schema_version+1）→ 后台批量回填（批 500、批间 5ms、游标存 `catalog_migrations`，重入续跑）→ ACCESS EXCLUSIVE 锁窗全量追平重算 + 行数校验 → 原子 swap（旧列 RENAME `<key>__d<seq>` deprecated 残留、新列接管逻辑名）→ schema_version++；迁移期间该属性写拒收（`ErrAttributeMigrating`）读放行（旧列）；validate 失败显式落账（failed + error）不静默回滚。放宽（varchar 扩宽/required→optional）= 即时 ALTER（元数据级，无 copy 任务），schema_version 同样递增 |
| `CreateIndex` | **两阶段 CONCURRENTLY 通道（B3，见 §4 末）**：事务 A building → 事务外 `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_<物理名>_<idx> ON <物理名>(...)`（unique/fulltext/hnsw/数组 GIN array_ops 形态同源 `buildIndexStatement`）→ 事务 B active|failed；deprecated 属性不可作索引目标 |
| `DeleteIndex` | `DROP INDEX IF EXISTS` + indexes JSONB 删除 + CAS（`RunInTx` 原子，`DeleteIndex`） |
| `UpdateCollection` | 权限替换与字段更新同一 UPDATE，统一刷 `updated_at` + ddl_seq CAS（空 patch no-op；并发冲突 → `CATALOG.DDL_CONFLICT`） |
| **schema 漂移对账（B3，`ReconcileSchemaDrift`）** | 启动钩子（server 侧）+ `torchwood admin schema repair [--dry-run]` 共用入口：扫三类漂移自动修复 + 告警——① 缺列（catalog active attr 物理表无列 → ADD COLUMN + 列授权重刷；required 无 default 且表非空不自动回填）；② 索引漂移（stale building >30min 按 pg_index 分流：valid 补账 active / INVALID·缺失 DROP 后 CIC 重入；活 CIC 不动；failed 条目重入；active 条目物理缺失/INVALID 重建；无主 INVALID 清理；默认时间索引/_acl GIN 缺失 CIC 补齐）；③ 幽灵表（业务 schema 内 catalog 无行 → DROP + 告警）。catalog 行有而物理表缺失只报告（重建 = 放弃存量数据）。dry-run 全部只报告。骨架对齐 grants_reconcile（ORDER BY 全键、单集合失败不中断、指标告警） |

属性生命周期（attrs JSONB 条目 `status` 字段，缺省 active 零迁移）：`active → migrating → active`（copy 迁移窗口）/`active ⇄ deprecated`（两段删列段一，可回滚）/`deprecated → retired`（段二，条目移除）。JSON Schema 导出（B10）对 deprecated/migrating 属性标 `deprecated: true` 且不进 required。

类型映射（`pgTypeFor`）：`string/email/url→VARCHAR(n)/TEXT`、`integer→BIGINT`、`float→DOUBLE PRECISION`、`boolean→BOOLEAN`、`datetime→TIMESTAMPTZ`、`json→JSONB`、`vector→VECTOR(dims)`。**数组属性（阶段③-b，redesign §3.1/§10.5 P0）**：`array=true` 落地 PG 原生数组列（`pgArrayTypeFor` 单源 DDL 与参数 cast）：`string→TEXT[]`、`integer→BIGINT[]`、`float→DOUBLE PRECISION[]`、`boolean→BOOLEAN[]`、`datetime→TIMESTAMPTZ[]`；元素类型仅限该标量子集（email/url/json 拒绝），数组列不带 DEFAULT（缺省 NULL）。数组列的 key 索引自动选 `GIN (col array_ops)`（`&&`/`@>` 可走索引）且仅支持单列索引；unique/fulltext 对数组列拒绝（PG 数组无唯一约束语义）。

**vector 属性（会话 #10，§10.5 P0 最后一项）**：`type=vector` + `dims`（必填，2..2000 = pgvector HNSW 可索引上限；非 vector 类型设置 dims 拒绝），落地 pgvector 原生 `VECTOR(dims)` 列（扩展由迁移 000005 启用，镜像 `pgvector/pgvector:0.8.6-pg18`）。`default_value` 与 `array=true` 对 vector 拒绝。写入值 = JSON 浮点数组（编码为 pgvector 字面量 + `?::vector` 绑定，维度绑定前校验）；读回契约 = JSON 数组（`to_jsonb(vector)` 原生输出字符串——原型 3 实证，读回投影以 `to_jsonb(d.*) || jsonb_build_object(col::text::jsonb)` 逐列覆盖）。**维度变更 = 新列 + 数据重灌**（换模型即换列名，不走 schema 演进状态机）。**hnsw 索引**：`CreateIndex type=hnsw`（单列、`distance_metric∈{COSINE,L2,INNER_PRODUCT}` 缺省 COSINE、orders 拒绝、metric 归一大写落 catalog），DDL `USING hnsw (col vector_cosine_ops|vector_l2_ops|vector_ip_ops)`；同列可建多 metric 索引；vector 列×key/unique/fulltext、非 vector 列×hnsw、数组列×hnsw 全拒绝；vector 列×key/unique/fulltext 拒绝。

## 6 查询（单 typed AST，C7）

**wire 形态唯一**：List/Count/Aggregate 的过滤/排序/投影一律走 `query`（`shared.v1.Query`：`filter` 树 + `orders` + `select` + `pageSize/pageToken`）。`queries` DSL 字符串字段已 reserved（POC 无兼容期）；GET 面保留 `page_size/page_token` 简单分页参数，过滤条件一律 POST body（`:list` 的 body 即 Query JSON）。绑定链：`BindListQuery`（proto codec，`pkg/query/proto.FromProto`）→ `ResolveQuery`（合并 GET 面分页字段 + 校验）→ infra `astFrom`（归一后再校验）。

**算子全集**（`Filter` oneof，`pkg/query` 常量同源）：`eq ne lt lte gt gte in between notBetween isNull isNotNull contains notContains startsWith notStartsWith endsWith notEndsWith search notSearch containsAny containsAll + and/or`（嵌套深度 ≤8，`MaxDepth`）；无通用 NOT——取反全部由 not* 变体承担（索引友好，德摩根展开可表达）。值数量约束：比较族 ≥1（eq/ne 多值 → IN/NOT IN）、between/notBetween 恰 2、isNull/isNotNull 0、containsAny/containsAll ≥1（数组字面量）。

| 类 | 算子（proto oneof 分支） | SQL |
|---|---|---|
| 过滤 | `eq`/`ne`/`in` | `=` / `IN` / `NOT IN`（eq/ne 多值自动进集合语义） |
|  | `lt`/`lte`/`gt`/`gte`/`between` | `<`/`<=`/`>`/`>=`/`BETWEEN ? AND ?`；`notBetween` → `NOT BETWEEN` |
|  | `contains`/`startsWith`/`endsWith`（及 not* 变体） | `ILIKE '%v%' ESCAPE '\'`（`escapeLikePattern` 转义 `%_\'；not* → `NOT ILIKE`） |
|  | `search`/`notSearch` | `to_tsvector('simple',col::text) @@ plainto_tsquery('simple',?)`（not → `NOT (...)`） |
|  | `isNull`/`isNotNull` | `IS NULL` / `IS NOT NULL` |
|  | `containsAny`/`containsAll`（阶段③-b §10.5 P0） | `col && ?::T[]`（交集非空）/ `col @> ?::T[]`（子集）；**仅 array=true 属性可用**（白名单，标量列/系统列拒绝）；参数 pgTextArray 字面量 + 按列元素类型 cast；NULL 列与空数组列不命中 |
| KNN | `vectorSearch`（会话 #10 §10.5 P0，**非 filter 节点**） | `SELECT …, (col <op> $vec) AS __dist … WHERE <policy+filters> ORDER BY col <op> $vec LIMIT k`；op：COSINE `<=>` / L2 `<->` / INNER_PRODUCT `<#>`（负内积） |
| 排序 | `orders[]`（attribute+desc） | `ORDER BY d.k1 dir1, …, d._id <首键方向>`（与 cursor 续页路径同构的 `_id` tiebreaker） |
| 分页 | `pageSize`/`pageToken` | LIMIT；**keyset-only（C2 完成态，见 §9）**：`pageToken` 只认 `ka:/kb:` token；count/aggregate 对排序/分页算子（orders/pageSize/pageToken/cursor）显式拒绝（R9+R9b，整集语义）；KNN 下 pageSize 即 k、pageToken 认 `kvc:` 距离游标（B2 多页 KNN，见 vector_search 段） |
| 投影 | `select[]` | 返回后裁剪 `Data` |

别名 `$id→_id`、`$createdAt→_created_at`、`$updatedAt→_updated_at`、`$version→_version`（`mapQueryField`）。

**DSL 是客户端糖**：`pkg/query.Parse/ParseMany`（含 `BuildFilter`/`BuildEqual` 拼串助手与 `ToWireJSON`——AST→protojson 形态，CLI 用）与 `sdk/go/query.FromDSL` 在客户端把 Appwrite 风格串解析为 AST 后发送；服务端零消费。程序化构造用 `pkg/query` 构造器（`query.Eq/Gt/Between/IsNull/And/Or…`）或 SDK 的 `sdk/go/query`（链式 `Builder`）。

**输入上限**（`internal/infra/documentdb/postgres.go`）：AST 叶数 ≤100（`pkg/query.MaxQueries`，`Validate` 封顶）、eq/in 多值 ≤1000、**跨 filter 绑定参数累计 ≤2000**（`maxTotalFilterParams`，封死 PG 65535 语句参数上限）、`maxQueryLimit=100`（页大小上限 clamp）。DSL 串条数/长度上限随双栈退役移除（服务端不再收字符串）。**写入载荷上限**（`internal/app/documents`）：总量 ≤1 MiB、单属性值 ≤256 KiB，超限 `DOCUMENT.TOO_LARGE`（InvalidArgument，违规属性定位走 BadRequest violations）。**H2 上限族**（redesign §11-J，常量集中在 `internal/app/documents`）：`_acl` ≤64 ACE（`DOCUMENT.ACL_TOO_LARGE`；校验收敛 app 层写路径——create/update/upsert/bulk/execute-tx 全覆盖，与种子/授予治理同层，种子 ≤3 条天然合法；RLS/adapter 不设防，防御纵深已在列授权与 `tw_set_document_acl` 函数通道）；数组值 ≤1000 元素（data 通道数组值 + `array_updates` 的 values，超限 `DOCUMENT.TOO_LARGE`；DDL 通道无此面——array=true 拒绝 default_value）；每集合列数软限 200（CreateCollection 一次性声明 / CreateAttribute 存量+1 前置拒绝，`CATALOG.COLUMN_LIMIT_EXCEEDED`；PG 1600 列硬限留余量）；object 嵌套 ≤8 层（`ValidateDocumentPayload` 内校验，`DOCUMENT.TOO_LARGE`；map 计一层、数组透明不计层）。

**编译与校验**（`postgres_query_compile.go`）：`astFrom` 收归一后的 AST（无字符串回退）；`validateQueryFields` 白名单=系统列+已声明 attribute，`search` 需命中 `fulltext` 索引，`containsAny/containsAll` 需命中 array=true 属性（阶段③-b 白名单），`_version` 缺列返回 `version_column_unavailable`（`InvalidArgument`），系统集合敏感列（`users.password_hash/prefs/labels` 等）黑名单仅按 `IsSystemCollection` 生效。

**数组写侧原子算子（阶段③-b §10.5 P0；Intersect/Diff/Insert/Filter 补齐于转出 POC B1）**：`UpdateDocumentRequest.array_updates`（`map<string, ArrayUpdate>`，client+server 双面，execute-tx op 同型字段仅 update 消费）编译为单语句 SET 子句，与 data/increment 可组合（同列冲突 → InvalidArgument）、OCC 不变。八算子：`APPEND` → `COALESCE(col,'{}') || ?::T[]`、`PREPEND` → `?::T[] || COALESCE(col,'{}')`、`REMOVE`/`DIFF`/`FILTER` → 差集（三者同构：`DIFF` 对齐 Appwrite arrayDiff 独立命名，`FILTER` 仅支持"移除等于任一 values"受限形态、条件表达式形态不支持；移空后为空数组非 NULL，NULL 列保持 NULL）、`UNIQUE` → 保首次出现序去重（WITH ORDINALITY）、`INTERSECT` → `col ∩ values`（**去重**并保 col 首次出现序——与 Appwrite arrayIntersect 一致产生唯一元素；移空后空数组，NULL 列保持 NULL）、`INSERT` → 定点插入（`index` 0 基对齐 Appwrite，其后元素顺移；越界 `index ≥ len` = 尾插；`NULL` 列视为空数组；要求 values 恰 1 且 index 已设置且 ≥ 0；PG 18 无 `array_insert` 内建——unnest WITH ORDINALITY + UNION ALL 重排等价实现，position 越界自然退化尾插）。**NULL 列语义二分**：添加类（APPEND/PREPEND/INSERT）视为空数组归一，读改写类（REMOVE/UNIQUE/INTERSECT/DIFF/FILTER）保持 NULL。data 通道对数组列是整列替换（值编码为数组字面量，目标列类型推断）；读回经 `to_jsonb` 自动投影为 JSON 数组。**查询侧不设对应算子**（B1 裁决）：Appwrite 的 arrayIntersect/arrayDiff 本就是写侧算子、查询侧无可对齐语义，布尔谓词由 containsAny（交集非空）/containsAll（子集）承担——避免自创查询语义引发 proto/pkg/query/SDK/golden 全链扩张。

**vector_search 查询语义（会话 #10，redesign §10.5 P0 / §11-J D2；多页 B2；ef_search B7）**：`query.vectorSearch = {attribute, values[], metric, maxDistance?, efSearch?}`（**非 filter 树节点**——距离不可作布尔谓词，排序由距离承载）；`pageSize` 即 k（top-k **可见**近邻，缺省 25、上限 100）；与普通 `filter` 可组合（AND），与 `orders` 互斥（InvalidArgument）；DSL 字符串不支持（向量不该手写，SDK typed builder only——`sdk/go/query.VectorSearch` / TS `vectorSearch()`）。前置校验（显式拒绝原则）：目标列必须是声明的 vector 属性、`values` 与 `dims` 等长、且存在与 `metric` 匹配的 hnsw 索引（无索引/metric 不符 → InvalidArgument——search 需 fulltext 索引的同款纪律）；vector 属性不得进普通 filter（白名单仅放行 isNull/isNotNull）与 order。**ef_search 调参（B7）**：`efSearch` 合法域 **[1,500]**（≤0 / >500 → InvalidArgument **显式拒绝，不静默 clamp**——静默改写让调用方误以为请求值生效；上限防滥用），设置后查询事务内 `SET LOCAL hnsw.ef_search = N`（与 iterative_scan 同款 GUC 事务级注入，零残留）；**缺省不注入任何语句**——行为与 pgvector 缺省 40 逐字节一致（集成测试 `TestVectorSearch_EfSearch_DefaultByteIdentical` 锁定 unset ≡ 显式 40 同果）；近重复簇边界召回不足时调大（代价：访存/延迟随 ef 增长；`TestVectorSearch_EfSearch_RecallAndLatency` 以 B2 的近重复簇数据形态锁定聚合召回弱单调 recall(200) ≥ recall(40) 并记录对比数字）。**多页 KNN（B2）**：`pageToken` 携带服务端发放的 `kvc:` 距离游标（`kvc:<dist_hex16>:<docID>`，float8 比特定长 hex 精确往返，负距离原生支持；docID 空的特形 = "该距离起点"），同请求参数 + token 翻页，`next_page_token` 满/组页发放、空串收尾（同普通 list 满页判定，取尽时可能有一个 0 行空页）。**内积方向**：pgvector `<#>` 返回**负内积**（值域 (-inf,0]），"越大越好"的内积取负后"越小越近"——与 cosine/L2 统一为距离升序，续页阈值方向三 metric 一致（`(dist = $d AND _id > $id) OR (dist > $d)`），无需按 metric 翻转。**切页管道**：首页保持 HNSW + iterative scan（ORDER BY 距离，取 k+1 行按"完整距离组"切页——第 k+1 行证明第 k 行距离组无越页 tie 时满页发射；HNSW 对同距 tie 组取舍任意，组不完整即整组顺延、游标落组起点 `kvc:<d>:`，防"发射 tie 真子集 + 阈值游标"漏行）；续页为 **(dist,_id) 精确全序扫描**（`ORDER BY dist, _id` + 阈值谓词——HNSW 索引只承载距离单键序、同距组内跨查询不稳定，仅阈值谓词会丢 tie 行，故续页放弃 HNSW 换结构化不重不漏；Seq Scan 精确求值，单页 KNN 不受影响）。`maxDistance` 在每一页独立后置过滤（续页仍受 max_distance 约束；发射集被滤空时后续必然全超阈，直接收尾）。`distances` 与 `documents` 平行回传（server/client 双面），不污染 `Document.Data`、不持久化、不进事件；count/aggregate 拒绝 KNN（整集语义）。**iterative scan 为契约**（D2）：查询事务内 `SET LOCAL hnsw.iterative_scan = 'strict_order'`（GUC 默认 off；off = "先取全局 k 再滤"，RLS×KNN 召回错误——开发期实证 1000 行 5 可见时 off 返回 0/5、on 返回 5/5；strict_order 保序，`distances` 回传与文档顺序一致）；RLS policy（`tw_visible`）作为 securityQuals 隐式参与过滤——**vector 与文档同事务、同 RLS 判定管辖**（对 Appwrite VectorsDB 独立产品线的核心差异，集成测试 `TestVectorSearch_SparseVisibilityRecall` 锁定）。已知边界：近重复不可见簇 + 极低可见率（如 99% 行向量几乎相同且不可见）下 HNSW 图导航可能饱和、返回 < k 行（近似索引固有边界）——该边界的调参手段 = B7 的查询级 `efSearch`（iterative_scan 保持 strict_order 契约不暴露）。

## 7 权限模型（`_acl` 内嵌 + RLS 判定执行点，阶段③）

条目 `type:role`，`type∈{read,create,update,delete}`（`write` 展开为三写）。角色：`any`（合成，仅 read 可授予）/`users`/`user:{id}`/`group:{id}`/`keys`/`key:{id}`（B14 一等可授予角色——per-key 数据隔离身份，API key 主体自带）/`admin`/`guests`/`__system__`。`ExpandPermissionRoles` 无条件注入 `any`；`ExpandPermissionTemplates` 展开 `user:`/`group:` 模板。**存储（包 A）**：文档 ACE 内嵌 `_acl TEXT[]`（元素 `"type:role"`，空数组回退集合级——B1），`_perms` 表退役（不再创建/读写，存量死表不迁移）；集合级权限与 `documentSecurity` 存 catalog（policy 经 InitPlan 子查询**实时读取**——集合级权限变更零 DDL 即时生效）。**读回免费**：`to_jsonb(d.*)` 载荷已含 `_acl`，`parseDocumentJSON` 顺带解析为 `Document.Permissions`（List/Get 零额外查询）。

**per-key 私有（B14，C6 决议：不接受"项目内全体 API key 互通"）**：API key 主体的角色集为 `keys` + `key:<自身id>`——`keys` 承载 scope/API 面（集合默认权限、特权授予判定），`key:<id>` 承载数据隔离身份。空 ACE 种子（`seedDocumentPermissions`）对 API key 主体绑 `read/update/delete:key:<自身id>`（与 user 主体 owner ACE 同构）——**默认私有**：keyA 建的文档 keyB 不可见（Get = NotFound，防枚举），跨 key 协作需显式授予 `key:<id>` ACE。存量共享语义兼容：catalog 与文档中**既有 `keys` ACE 保留有效**（显式授予过的继续共享，不做数据迁移），但默认种子不再产生 `keys` ACE；`DefaultCollectionPermissions` 的集合级 `keys` 四连为集合默认授权，不受本收敛影响。

**判定执行点（包 C）= RLS policy**（业务集合 c_\* 表，建表生成 + DDL touch reconcile，`rls_policy.go`）：

- 函数单源（public 迁移 000004，阶段③/③-b 终态）：`tw_can(acl, roles, typ, coll_allows)`（= `AllowsDocumentAccess` 用户集合分支：write 展开 + 空回退 + 零角色 fail-closed）；`tw_coll_allows(perms, roles, typ)`（集合级 JSONB 判定）；`tw_visible`（可写即可读：read ∨ update ∨ delete 命中；docSec=false 纯集合级；空 `_acl` 快速路径）；`tw_roles()`（**SECURITY DEFINER 验签函数，阶段③-b**：仅 tw_app 身份、`app.roles_sig = HMAC-SHA256(密钥, roles||'|'||exp)` 未过期时解包 `app.roles` GUC 为 text[]；sig 缺失/格式错/过期/验签失败/密钥缺失 → 空数组 = 零角色 fail-closed——app.roles GUC 可被任何持 SQL 会话者 set_config 伪造，验签后通道封死；密钥 = `HMAC-SHA256(security.jwt.secret, "tw-roles-guc-v1")`，Go 进程派生 + 启动钩子落 `tw_secrets`（表不授予任何角色）；A4 双钥轮换（迁移 000004）：`tw_secrets` current/previous 槽位（每 purpose 至多一把 current 的部分唯一索引约束），`tw_sig_match` 任一钥命中即通过，Go 启动钩子换钥时旧 current 降级 previous 而非删除（third 条直接删，行数 ≤2）——滚动重启换钥窗口内旧 sig 验签通过、窗口外（exp 过期）依旧拒绝）。SQL golden 矩阵锁语义（`rls_policy_test.go`），禁止 Go 侧等价实现。
- 四条 policy：SELECT USING = 空 `_acl` 快速路径 ∨ `tw_visible`；INSERT WITH CHECK = 集合级 create；UPDATE USING = CASE docsec → `tw_can(update)` ELSE 集合级，WITH CHECK = 恒真（自锁放行——但 `_acl` 实际经 tw_system 第二语句写，见下）；DELETE USING 同构。全部 `ENABLE + FORCE ROW LEVEL SECURITY`。
- **连接模型（包 B + 阶段③-b）**：文档面入口（读写同构，autocommit 退役）经 `withDocumentTx` 包进带身份事务——首条 `SET LOCAL ROLE`（`tw_app`；SystemPrincipal/PlatformAdmin → `tw_system` BYPASSRLS；DDL → `tw_owner`）+ `set_config('app.roles', …, true)` +（tw_app）`set_config('app.roles_sig', …, true)` 签名注入（多语句合并单往返）；漏注入 = 零角色 = policy 恒 false（fail-closed）。中段身份切换（尾随读回）退出前恢复外层身份。
- **应用层判定退役面**：业务集合的 `checkDocumentPermission`/`listPermissionFilter`/批量预取校验全部退役——policy 隐式过滤即判定；**sentinel 系统集合保留应用层**（`AllowsDocumentAccess` + `_acl` 谓词过滤——静态平面独立授权，预决策 9）。`ensureCollectionAccessible`（disabled）与授予治理（`ValidateGrantablePermissions`）两模式都保留在用例/入口层。

| 操作 | 检查点（业务集合 = policy；sentinel = 应用层） |
|---|---|
| `CreateDocument` | INSERT WITH CHECK（集合 create；拒绝 → 42501 → PERMISSION_DENIED）+ `isWriteProtectedSystemCollection` 拦截（sentinel） |
| `GetDocument` | SELECT policy（`tw_visible`）；不可见 = 0 行 = nil → NotFound（防枚举） |
| `UpdateDocument` | UPDATE USING（`tw_can(update)`）；0 行三态探测：不可见→NotFound / version 不符→VERSION_MISMATCH / 可见不可写→PERMISSION_DENIED |
| `DeleteDocument` | DELETE USING（`tw_can(delete)`）+ DELETE 语句内 `_version` 守卫（compare-and-delete；无锁预读，防 FOR UPDATE 叠加 UPDATE policy 误拒 delete-only 用户） |
| `ListDocuments`/`CountDocuments`/`Aggregate` | SELECT policy 隐式过滤（单源；聚合过滤先于 GROUP BY——D1 securityQuals 机制保证） |
| `UpsertDocument` | 预查（经 SELECT policy）分支：纯插入 → INSERT WITH CHECK；命中 → UPDATE USING（§3.2 #7：upsert 需同时持有 create 与 update，语义有意收紧） |

**`_acl` 写入路径（PG 18 实证修正；阶段③-b 收口为唯一函数通道）**：UPDATE/ON CONFLICT 修改 SELECT policy 引用的列（`_acl`）会触发 SELECT policy 对**新行**的复检——`WITH CHECK(true)` 无法单独保自锁；`_acl` 变更通道唯一化为 **`tw_set_document_acl(p_schema, p_table, p_tenant, p_doc, p_acl)`**（迁移 000004，SECURITY DEFINER owner=`tw_system` BYPASSRLS 绕开新行复检，语义承袭原 tw_system 第二语句）：create/upsert 插入支的 INSERT 不再携带 `_acl`（行内 DEFAULT '{}' 兜底，非空权限集同事务函数补设），update/upsert 更新支/bulk 的替换改调函数（同事务、当前 tw_app 身份，EXECUTE 仅授 tw_app）；`p_table` 经 catalog physical_name 白名单校验（防注入）。同理 ON CONFLICT 推测插入要求拟插入行过 SELECT policy——upsert 拆预查分支 + 普通 INSERT/UPDATE（advisory lock 保证同冲突键串行；与并发普通 Create 撞唯一键改报 DuplicateKey，可重试）。**列级 GRANT**：`tw_app` SELECT 全列 + INSERT/UPDATE 数据列与除 `_tenant`/`_acl` 外系统列（`_tenant` 锁死不可写；`_acl` 双向锁死——应用身份直改的旁路从列权限封死，R13a 的 UPDATE 移除 + 阶段③-b 的 INSERT 移除）；`tw_system` 表级 ALL。`_version` 不锁列（CAS 守卫 `WHERE _version=?` 已足，写错只会让自己失败）。

**授予治理与可见范围归属（`ValidateGrantablePermissions`，`internal/domain/databases/permissions.go`）**：普通用户不可授予未持有角色与 `any` 写权限（`keys`/`System`/`PlatformAdmin` 跳过）；覆盖 create/upsert/update/bulk 全部写路径（`internal/app/documents/documents.go` 各写路径统一过 `applyGrant`，Client 侧 `WriteOptions.AllowPrivilegedGrant=false`）。`create` 类 ACE 例外放行——行级 create ACE 在 INSERT 后无判定语义（INSERT WITH CHECK 仅集合级），不构成提权面。端用户可见范围归属的安全语义三问：

- **定向共享给特定用户 → 拒绝**：用户 A 不持有 `user:B`，无法给自己的文档挂 `read:user:B`；`label:`/`group:` 同理，只有自身也持有（属于该组/标签）才能授——**组内共享是组语义本身，不是越权**。模板 `user:`/`group:` 只展开为调用者自身首个匹配角色，无法借模板指名他人。
- **广播公开 → 合法（Appwrite 同款）**：`read:any`（读类合成角色授予放行；`any` 经 `ExpandPermissionRoles` 无条件注入数据面角色集，`postgres_exec.go`）= 凡走数据面判定的主体均可见；`read:users` = 所有登录端用户可见（API key 主体不含 `users` 角色）。写类对 `any` 一律拒绝。**默认收口**：Client 创建不带 permissions 时包装层盖 owner ACE `read/update/delete:user:<自身>`（`internal/app/client/databases.go` `ownerDocumentPermissions`），key 主体种子见 B14——不显式公开就只有自己可见。
- **写到他人名下 → 不可能**：`_created_by`/`_updated_by` 由服务端从 Principal 戳入（`userIDFromPrincipal`：端用户存裸 user id、API key 存 `key:<id>`，请求不可传）；`_tenant` 列锁死不可写；改他人文档需目标行 `tw_can(update/delete)`；`_acl` 替换唯一通道 `tw_set_document_acl` 复核目标行 `tw_visible`（堵"改他人 ACL 提权"）。

**两条应用侧边界**：① `documentSecurity=false` 是总开关——集合级权限模式下文档级 `_acl` 完全不参与判定（`tw_visible` ELSE 分支），可见范围由开发者配置的集合级权限决定，端用户请求中的 permissions 字段无效。② 权限内核管"谁能读写哪些行"，不校验行内数据字段的语义归属——`userId` 类自声明属性属不可信输入，应用须服务端派生（`Me`/JWT）或查询收口（`eq("userId", 当前用户)` + 集合级权限）。业务不允许用户私自公开内容时：关 `documentSecurity` 走集合级权限，或在入口层过滤请求中的 permissions。

## 8 OCC（`_version`）

用户集合 `BIGINT NOT NULL DEFAULT 1`，`Create/Upsert/Bulk` 盲写但 `_version+1`（`Bulk` `SkipVersion=true` LWW），`Update/Delete` 必填且等于当前值（行锁下比较，`versionColumnReady` 校验 `bigint`），成功 `+1`。错误 `version_required`（缺省）/`version_mismatch`/`version_column_conflict`（`FailedPrecondition`），`version_invalid`（**显式 ≤0**，`InvalidArgument`——与缺省态不同码，Phase 1 裁决②），`version_column_unavailable`（`InvalidArgument`）。`_version` 可作过滤/排序/投影；系统表无此列。`Upsert` 的 `conflictColumns` 必须无序命中集合一个 unique 索引（非 Bypass 主体前置校验 `validateConflictColumns`，否则 InvalidArgument；Bypass 主体靠 PG 42P10 兜底）。

## 8.1 事务内核 execute-tx（redesign §4.8 Phase 1）

`DatabasesService/ExecuteTransactions`（Server 面）：单事务内顺序执行异构 op 批（`internal/infra/documentdb/postgres_transactions.go`，Bulk 的泛化）。op 模型 `{type(create/update/upsert/delete), collection_id, document_id, data, permissions, increment, array_updates, expected_version, conflict_columns}`（`array_updates` 仅 update 消费——转出 POC B1，语义与单文档 UpdateDocumentRequest.array_updates 同源 `buildArrayParts`），上限 1000（`MaxBulkOperations`）。锁纪律：按 `(collection, documentID)` 排序预取 `pg_advisory_xact_lock` 防批间死锁，op 按请求序执行（事件序 = op 序）；各 op 复用单文档事务体（权限/OCC/conflictColumns 校验同源）。`ATOMIC`（默认）任一失败整批回滚（错误带 op index 域码定位）；`PARTIAL` 逐 op SAVEPOINT 容错、已成功不回滚、返回 per-op 结果（含失败域码）。create/upsert 空 ACE 种子与单文档 API 同语义。

**Functions 的多写原子性（Phase 2 形态乙，阶段③-b 定稿）**：函数代码运行在外部 Docker 容器（进程隔离，§4.8 Phase 2 的"同进程共享 ctx"前提不成立），不做跨进程事务——函数内的多写原子批一律经本 RPC 表达（函数通过 API/SDK 调用 `documents:execute-tx`），批内事件序 = op 序，成功全提交、失败（ATOMIC）整批回滚。

## 8.2 域错误码（redesign §4.1）

域码稳定 snake_case（`DOCUMENT.NOT_FOUND`、`DOCUMENT.VERSION_CONFLICT`、`DOCUMENT.ATTRIBUTE_UNSERIALIZABLE`、`IDEMPOTENCY.KEY_CONFLICT` 等，`internal/domain/databases/errors.go`）静态映射 gRPC code；消息格式 `CODE: message`，ErrorInfo detail 携带 `reason`/`retryable`（OCC 冲突、资源耗尽与幂等执行中可重试）与 `error_id`（**DomainStatus 生成处统一注入**，与 infra SQLSTATE 路径对齐——每个错误实例可唯一引用；限流拒绝为 `RATE_LIMIT.EXCEEDED` / `torchwood.platform` + RetryInfo 精确退避）。字段级违规定位走 **google.rpc BadRequest 标准 detail（field_violations）**：execute-tx 的 op 定位迁移为字段路径形态（`ops[3].expected_version`——域码映射子字段：VERSION_\*→`expected_version`、PERMISSION_DENIED→`permissions`、NOT_FOUND/ALREADY_EXISTS→`document_id`、无映射→`ops[N]`）；载荷违规（`data.blob`）与 op 守卫（`ops[i].collection_id` 等）同形态。裸 "document database error" 已消灭；infra 产出的域码 status 在 app 层经 `errors.As` 提取透传（防包装链丢 status）。

## 8.3 聚合 `documents:aggregate`（redesign §4.1 + §10.5 P1）

`DatabasesService/AggregateDocuments`（Server 面，scope `databases.read`）：`POST .../documents:aggregate`，`sum/avg/min/max` + 可选单键 `group_by`（count 已有独立 `:count` RPC 不并入）。语义：

- **D1（§11-J 已裁决）**：聚合一律在 `listPermissionFilter` 过滤后的可见行集上执行（过滤先于 GROUP BY）——不可见行不进聚合、group 键不泄露；权限 golden 集成测试锁语义（`aggregate_integration_test.go`）；最小桶/k-匿名未实现（可选产品功能，默认关）；权限变更前后聚合结果不可比属固有属性。
- 聚合目标必须是集合声明的数值属性（`integer`/`float`，System 主体一视同仁防拼列）；`group_by` 须为已声明属性，键按 text 序列化（NULL 键=属性未设置的行，归入 `group_key` 未设置的组）。
- 空集语义：`sum=0`（COALESCE，类型跟随属性）、`avg/min/max` 无值（proto oneof 未设置）；`group_by` 下空集返回空组列表。无 `group_by` 时恰返回一组。
- **结果类型化（预决策 5）**：`AggregateValue.result` 是 oneof——integer 属性的 `sum/min/max` 走 `int64_value`（`SUM(bigint)::int8` 显式收敛、`MIN/MAX` 原生 bigint，`>2^53` 精确）；`avg` 恒 `double_value`（`AVG(bigint)::float8`）；float 属性恒 `double_value`。integer 聚合超出 int64 → `InvalidArgument` / `AGGREGATE.OVERFLOW`（PG 22003 翻译，域码见 `internal/domain/databases/errors.go`）。
- **Data 的 double 精度界（维持不变）**：文档 Data（JSON `Struct`）的 number 通道是 double——业务值可能超过 2^53 时请用 **string 属性承载**（换 Struct 形态收益不成比例，redesign §4.1 类型系统既定取舍）；聚合面经 `int64_value` 已绕开该界，读写两个通道各自精确。
- 过滤算子与 ListDocuments 同形（单 typed AST）；排序/分页算子（orders、page_size、page_token、cursor、token）在 count 与 aggregate 一律 `InvalidArgument` 显式拒绝（整集语义，R9 + R9b——分页字段归一进 AST 后统一拦截）。

## 8.4 写幂等 `request_id`（redesign §4.1/§10.1）

七个写入口（Create/Update/Upsert/Delete/BulkUpdate/BulkDelete/ExecuteTransactions，Server 面 proto 字段 `request_id` + Client 面 Create/Update/Upsert/Delete；HTTP 面等价 `Idempotency-Key` 头，proto 字段优先）在 `internal/app/documents` 核层包裹：

- **键作用域** `(project_id, actor_id, request_id)`；`actor_id` 复用归因链（`Principal.StableActorID`：`user:<id>` 角色存裸 id（end user / console admin）、API key 主体 `key:<id>`、内部 System `system`）；不同 actor 同 key 不冲突。
- **指纹** = method + 请求关键字段规范序列化 sha256（批 ID/conflict_columns 排序规范化——集合语义，重试乱序不判冲突）；同 key 不同指纹 → `IDEMPOTENCY.KEY_CONFLICT`（InvalidArgument，重试无意义）。
- **语义**：只缓存成功响应（失败 Release 释放占位，重试重新执行）；重放返回原响应 + `x-torchwood-replayed: true` 响应头（gRPC response metadata，网关 outgoing matcher 透传为 HTTP 头；零 proto 响应侵入）。并发同 key：短轮询（100ms 间隔）≤2s 后仍 in-flight → `IDEMPOTENCY.IN_PROGRESS`（Aborted，retryable）。`Complete` 失败不回滚写入（best-effort 缓存，只损失重放能力）；store 故障时写请求失败（Unavailable，不静默降级为 at-least-once）。
- **存储** `public.idempotency_keys`（PK `(project_id, actor_id, request_id)` 仲裁并发认领 + `claim_token` 防过期重认领串写 + `expires_at` 索引惰性清理）；done TTL 24h（完成时刻起算）、in_flight 兜底 TTL 5min（崩溃残留行到期可重认领——期间同键重试收到 IN_PROGRESS）。execute-tx 幂等覆盖整批（PARTIAL 重放返回首次完整 per-op 结果，§11-E2）。

## 8.5 事件链（outbox seq + Stream 位点 + 补偿，阶段④）

写路径同事务 `INSERT document_events_outbox`（`event_id` PK = 幂等去重键）；唤醒信号由 000002 的 **AFTER INSERT 行级触发器**发 `pg_notify('tw_outbox','')`（空载荷纯信号，随 commit 投递、回滚即丢弃；零额外客户端语句——应用侧逐条 NOTIFY 会使 Bulk 语句数翻倍，违反 R5-P2-6；同事务多次相同 NOTIFY 被 PG 自动合并，execute-tx 100 op 批只投递一次唤醒）。

- **seq（000002）**：`seq BIGINT GENERATED ALWAYS AS IDENTITY` + UNIQUE 索引。顺序承诺（B1 定稿）：**单文档全序**（行锁保证 seq 随提交序）；**集合内分配序**（跨文档不保证与提交序一致）；**seq 空洞 = 回滚事务消耗 identity，不丢事件**。seq 仅作续传游标与去重辅助，不承诺跨集合因果——客户端缓冲重排因此不可行（已否决），不引入排序器。
- **投递（B3）**：worker `LISTEN tw_outbox`（pgdriver.Listener 专属连接，自带重连；原型数字：commit→唤醒平均 7ms / 最大 21ms）+ 5s 兜底轮询，SKIP LOCKED 批拉 256（按 seq 排序）→ XADD `torchwood:events`（载荷 = 完整信封 JSON 含 acl + seq，不带 MAXLEN）→ **每 server 实例一个消费组**（组名 = 实例 ID `hostname:pid`）XREADGROUP → hub 扇出 → 批量 XACK。组从 `$` 起步（新实例不回放历史——断线窗口由客户端 last_seq 重放补齐）；PEL 挂起条目 idle>15min 由 XAUTOCLAIM 重投（重复经 hub event_id 去重窗口 5min + 客户端幂等吸收）。worker 清理周期 `XTRIM MAXLEN ~100000`（Stream 只是投递通道，重放窗口在 outbox 表——published 行 24h 清理覆盖 1h 承诺）。`published_at` 攒批回写保留（XACK 后 200ms/32 条，幂等 UPDATE）。
- **信封**：载荷上限 1MiB（对齐 H1 文档写入上限——写入面已拒超限，事件面正常路径不截断；超限仅防御性截断 + `truncated=true`）；`transaction_id` 非空表示来自 execute-tx 原子批（批内事件顺序 = op 序，Agent 端可分组消费）；`seq` 随帧下发。
- **`:changes` 补偿 API**（Server/Client 两面，scope `databases.read`）：`GET .../collections/{coll}/changes?since_seq=&limit=`（limit 缺省/上限 500）返回该集合 `seq > since_seq` 的**已提交**事件，seq 升序、按请求者可见性过滤（快照 ACL + 当前 principal，与 hub 扇出同语义）；`has_more=true` 以末条 seq 续传。delete 事件天然 tombstone（无 data 带 document_id + version）。`since_seq` 早于该集合最老可用事件 → `FailedPrecondition` / `EVENTS.RESUME_EXPIRED`（指引全量重拉后重新续传）；`since_seq=0` = 从最老可用事件起（不判过期）。
- **跨项目事件隔离（B-1）**：topic/频道名 `databases.<db>.collections.<coll>` 是**全局命名空间**（不含 project 维度），跨项目同名集合（各自缺省第一库 `app` 的同名集合）共享同一 topic/频道。事件面隔离由**消费两端强制 project 过滤**保证：`:changes` 扫描/游标判定（`MIN(seq)`）按 `outbox.project_id` 等值过滤（`has_more`/游标推进在过滤后的项目行集上，R15 两级游标不变）；Hub 扇出对文档事件按**连接归属项目**等值过滤（`RealtimeConn.ProjectID` 来自 WS 握手 `hello.project_id`——门控已校验其与凭证一致，即信任锚；不等直接跳过，不做 ACL 评估；空归属 fail-closed）。platform admin 同样受此约束——要看他项目事件须以该项目开连接。WS `last_seq` 重放（补发走 `ListChanges`）与实时扇出同源，天然同收口。锚点注释见 `envelope.CollectionChannel/DocumentChannel` 与 outbox topic 构造处；若未来引入跨项目同名集合的合法共享场景，须重新评审本决策。
- **WS `last_seq` 重放**：subscribe 帧可选 `last_seq` → 门控订阅（`BeginReplay` → 补发 outbox 窗口内事件（单次上限 500，超出则 `subscribed` 帧带 `has_more=true` 指引 `:changes` 续传）→ `EndReplay` 刷入去重后的实时帧）——补发帧先于实时帧、无漏帧窗口；窗口外同样 `EVENTS.RESUME_EXPIRED` error 帧（订阅失败、连接保持）。仅 databases 频道支持。
- **慢消费者水位断开**：每连接 send buffer 1024 帧；满水位 → close reason `resync:<last_seq>` 主动断开（不做 RESYNC 帧——客户端重连带 last_seq 即天然重同步，语义等价、协议更简）。SDK 端：按频道跟踪 payload seq、重连带 `last_seq`、resync close 零退避立即重连、`EVENTS.RESUME_EXPIRED` 默认清游标（可注入回调）。

## 9 事务与分页一致性

`BulkUpdate/BulkDelete` 与单文档写走 `withDocumentTx`（带执行身份的显式事务——A1 每请求一事务，`clients.Database.RunInTx` 注入 `SET LOCAL ROLE` + `app.roles`，`postgres_exec.go`；已在事务内复用并保持身份一致，中段切换退出前恢复）；批量缺失行经 `missingRowsError` 探测区分 PERMISSION_DENIED（可见不可写）与 NotFound。`ListDocuments` **keyset-only（C2 完成态，多键）**：首页（无 cursor）执行精确 COUNT 后主查询（非原子快照，`READ COMMITTED`），满页发 `ka:` token；续页（keyset token）跳过 COUNT（`total=0=unknown`），满页判定 has-more。**排序 = 全部排序键 + `_id` tiebreaker（方向随首键）**；keyset 谓词：方向一致（全 ASC/全 DESC）走行比较 `(k1,…,kn,_id) op (?,…,?)`，方向混合走逐键 OR 展开（`k1 OP1 ? OR (k1 = ? AND k2 OP2 ?) OR …`）——两种形态与 ORDER BY 全序严格一致（跨页不丢不重的机制保证，确定性用例锁行为）。cursor 仍以 docID 定位：服务端按 docID 查行取全部排序键值（token 只编码 `ka:/kb:`+docID）。

**NULL 排序键的已知限制**（预决策 4）：行比较谓词对 NULL 求值为 NULL（行被排除）——cursor 行的排序键含 NULL → `InvalidArgument`（消息明示"filter them out first"，先 isNull/isNotNull 过滤再分页）；数据行含 NULL 键在续页中被跳过。不做 NULLS LAST 谓词改写（行比较谓词与 NULL 的组合正确性代价不成比例）。`pkg/crud` 提供 `ParseListParams`/`BuildPaginationInfo`（`pkg/crud/list.go:57`/`pagination.go:360`），offset token（`EncodePageToken`/`DecodePageToken`，`v1` base64 JSON，TTL 24h）仅供静态表/控制面列表使用，文档面不再接受。

## 10 系统列与写入过滤

| 列 | 说明 |
|---|---|
| `_id` | 文档主键，`idgen.UUID()` 默认，`^[a-zA-Z0-9_.:-]{1,64}$`（`docIDRe`） |
| `_created_at/_updated_at` | 自动维护（`NOW()`） |
| `_created_by/_updated_by` | 归因主体：`user:<id>` 角色存裸 id；API key 主体存 `key:<keyID>`（`databases.Principal.KeyID` 由 `DocPrincipal` 投影，`userIDFromPrincipal`）；其余留空。B14 后该归因身份与空 ACE 种子（`key:<id>` 私有）同一命名空间——文档属主可直接从 `_created_by` 读出协作授予目标 |
| `_acl` | 内嵌文档 ACE（`TEXT[]`，元素 `"type:role"`；空数组回退集合级）——变更通道唯一化为 `tw_set_document_acl`（000004，SECURITY DEFINER；create/upsert 插入支经函数补设，update/upsert 更新支/bulk 经函数替换）；对 `tw_app` 的 INSERT/UPDATE 列级授权**双向排除**（`_version/_created_at/_updated_at/_created_by/_updated_by` 为合法写路径所需，`_tenant`/`_acl` 锁死） |
| `_tenant` | 租户标签；**对 `tw_app` 列级锁死不可写**（GRANT 排除；SELECT 可读——查询谓词需要） |
| 用户输入 `_` 前缀字段 | `buildInsertParts`/`buildUpdateParts` 直接过滤，防伪造系统列 |
| `documentSecurity/disabled` | 目录层控制：`disabled=true` 时非 `BypassesDocumentACL` 一律 `PermissionDenied`（`ensureCollectionAccessible`） |

写保护：`isWriteProtectedSystemCollection` 仅对 `databaseID=="_"` 的 `users/sessions/identities` 生效，业务库同名集合不受影响。

## 11 常见用法与示例

```go
// 创建集合后增属性
docDB.CreateCollection(ctx, pid, "app", "posts", perms, false)
docDB.CreateAttribute(ctx, pid, "app", "posts", Attribute{Key:"title", Type:"string", Size:128, Required:true})

// 单 AST 查询（C7）：程序化构造器（pkg/query）+ Query 结构体字面量
ast := &query.Query{
  Filter: query.And(query.Eq("status", "published"), query.Gt("views", "100")),
  Orders: []query.Order{{Attribute: "$createdAt", Desc: true}},
  PageSize: 25,
}
docs, total, nextToken, _ := docDB.ListDocuments(ctx, pid, "app", "posts", databases.Query{AST: ast}, principal)

// 多键排序 + 游标（C2 完成态）：全部排序键 + _id tiebreaker；
// 续页把上页 nextToken 填进 PageToken
ast = &query.Query{
  Orders: []query.Order{{Attribute: "priority", Desc: true}, {Attribute: "title"}},
  PageSize: 25, PageToken: nextToken,
}

// DSL 串是客户端糖（SDK/CLI 解析为 AST 后发送，服务端零消费）
parsed, _ := query.ParseMany([]string{`equal("status","published")`, `limit(25)`})
wire := parsed.ToWireJSON() // CLI/工具直连 JSON 请求面

// 数组列（阶段③-b）：查询 containsAny/All + 写侧原子算子
ast = &query.Query{Filter: query.ContainsAny("tags", "go", "db")}
docDB.UpdateDocument(ctx, pid, "app", "posts", databases.DocumentUpdate{
  Document:     databases.Document{ID: id},
  ArrayUpdates: map[string]databases.ArrayUpdate{
    "tags": {Op: databases.ArrayUpdateOpAppend, Values: []string{"new"}},
  },
  ExpectedVersion: version,
}, principal)
```

分页用 `pkg/crud`（见 `09-api-guide.md` §3）：`ParseListParams` 校验 `page_size/page_token/filter/order_by`，`BuildPaginationInfo` 产出 `HasNext/NextOffset`，handler 用 `EncodePageToken` 编码 `next_page_token`。

## 12 测试与参考

- 集成 `internal/infra/documentdb/postgres_test.go`（`testing.Short` 跳过），`internal/testutil/db.go:SetupTestDB` 按 `TORCHWOOD_TEST_DATABASE_SOURCE` 创建隔离库（`pg_terminate_backend` + `DROP DATABASE`）。**并行安全契约（A6）**：隔离库建库/迁移/删库段持集群级 advisory lock（`pg_try_advisory_lock` 轮询）跨进程互斥（`go test -p N` 每包一独立进程，进程内锁无效），admin 连接 10s 读超时 + 瞬时过载指数退避重试；000004 三角色为集群级共享对象——up 原子幂等创建（duplicate 容错）、down 仅清理本库（REASSIGN/DROP OWNED + REVOKE membership）**不 DROP ROLE**（角色名下对象恒属于某个测试库、随该库 `DROP DATABASE` 消亡），迁移循环 down 与并行库无集群对象竞争；契约全文见 `internal/testutil/db.go` 包注释。
- **全局 catalog / 物理表名（2026-09-06 勘误后形态）**：`postgres_catalog_global_test.go`（codec 全字段往返、public 两表 CRUD 落点、GetCollection 单查询 QueryHook 计数、并发建集 AlreadyExists、physical_name = collection_id 冗余投影）；`postgres_physical_name_test.go`（两集合物理表隔离与删除清理、表名 = 逻辑名直接可见、物理寻址字段不出现在 API 响应、bypass/System 路径、`_acl` 内嵌物理表、ddl_seq 五路径递增与并发冲突 `CATALOG.DDL_CONFLICT`、索引名组合校验把守）；`migrations_cycle_test.go` 锁 public 迁移 up/down 对称（含 000003/000004）。
- **`_acl` 内嵌 + 权限路径（阶段③包 A）**：`permissions_test.go`（B1 空回退/write 展开/租户隔离/keys 收窄/掩码语义 + List 权限回填零额外查询断言——查询数不随页内文档数增长）；`outbox_test.go`（事件 ACL 快照改道 `_acl` 点查/批查）；`TestDeleteCollection_CleansPerms` 退化为结构断言（物理表消亡）。
- **角色分层 + GUC/每请求一事务（阶段③包 B）**：`exec_identity_test.go`——注入正确性与事务外零残留、中段切换恢复、角色分层（tw_app 拒 DDL/tw_owner 可建）、fail-closed（空 roles 解包零角色）、**BYPASSRLS 经 SET LOCAL ROLE 生效（A1 原型①）**、**每请求一事务 vs autocommit 开销计时（A1 原型②：loopback 290µs vs 1.37ms，注入多语句合并单往返；未合并形态 3.33ms）**。
- **RLS 判定执行点（阶段③包 C）**：`rls_policy_test.go`——SQL golden 矩阵三层（tw_can/tw_coll_allows/tw_visible：角色×ACE 型×空回退×update-only 可见×write 展开×fail-closed）+ 行为级（可见性矩阵/update-only 可见可改/自锁+事件写前快照/Get 不可见=NotFound/create 拒绝 42501/`_tenant` 列锁死/tw_system 旁路）+ **EXPLAIN InitPlan 门禁（I1：catalog 子查询每语句一次）** + **10 万行 RLS 开/关相对基准（4.9x，阈值 30x；绝对 P99 门禁转出 POC 后上 CI 机器基准）**。
- **事件链（阶段④）**：`outbox_seq_test.go`（seq 单调、回滚空洞不丢事件、NOTIFY 唤醒延迟数字、transaction_id 落发与信封往返、dispatch 回填 seq）；`outbox_test.go`（execute-tx 批共带 transaction_id 且顺序 = op 序、1MiB 防御性截断）；realtime `subscriber_test.go`（两组各自消费全量 + ACL 过滤、XACK 后同组重启不重投、PEL 认领重投、新组 `$` 起步不回放）、`stream_test.go`（XADD 全信封含 seq、Trim 水位）、`hub_replay_test.go`（门控顺序/去重刷入、满水位 OnSlow 恰一次带 lastSeq）；`postgres_changes_test.go`（ListChanges 升序/since/可见性过滤/limit+has_more 续传链/document 过滤/RESUME_EXPIRED/tombstone）；api/realtime `handler_replay_test.go`（补发帧先于实时帧与确认帧、has_more 确认、过期 error 帧、非 databases 频道拒 last_seq、resync close 接线）；servergrpc/clientgrpc `changes_test.go`（双面映射与域码透传）。
- `pkg/query/query_test.go`（DSL 糖 + 构造器）、`pkg/query/proto/proto_test.go`（每算子编解码往返）、`postgres_query_compile_test.go`（每算子 SQL + keyset 谓词形态）、`permissions_test.go` 覆盖算子/转义/白名单/敏感列/权限分支；`TestPostgresDocumentDocuments_MultiKeyCursor` 锁多键跨页不丢不重。
- **多页 KNN（B2）**：`vector_search_test.go`——`TestVectorSearch_Pagination_DeterministicStitch`（三 metric 确定性几何：多页拼接 == 单页大 k 全序、距离严格递增且与 one-shot 逐一相等、内积负值方向锁定）、`TestVectorSearch_Pagination_TieGroup`（同距 tie 组整组顺延不重不漏——"仅阈值谓词"朴素形态的杀手用例）、`TestVectorSearch_Pagination_FilterSparseVisibility`（filter 组合 + RLS 稀疏可见行跨页 6/6，首页 HNSW×iterative scan、续页精确全序×RLS 两段覆盖）、`TestVectorSearch_Pagination_MaxDistance`（续页仍受阈值约束、滤空收尾）、`TestVectorSearch_Pagination_RejectedTokens`（ka:/kb: 异族/垃圾/NaN/非法 docID 拒绝）+ `TestKNNCursorTokenRoundtrip`（kvc: 编解码往返与异常形态）；codec 面 `TestFromProto_VectorSearchCombinations`（orders 互斥、pageToken 放开透传）。
- **DSL 文法 parity 锁**：`pkg/query/testdata/dsl_ast_golden.json`（52 条，含错误条目）是根模块解析器与 `sdk/go/query.FromDSL` 的**共同仲裁语料**——两侧 golden 测试以中立 JSON 形态比对；`root`/`sdk` override 块表达设计内的单侧契约差异（如 offset：根侧通用解析器支持、SDK 糖按文档面 keyset-only 拒绝）。改语料须在 commit message 给出理由，禁止单方删条目。
- **在线索引通道 + schema 漂移对账（转出 POC B3）**：`postgres_online_ddl_test.go`（持锁注入并发读写不阻塞——对照组非并发通道 lock_timeout 触发证注入生效；building 残留双中断点重入；unique 撞存量重复 failed→清理→重入；两通道 DDL 表达式逐字同源）；`schema_reconcile_test.go`（三类漂移注入 dry-run 全检出零修复 → repair 全修复功能级生效 → 二次扫描幂等零增量；building 残留三态恢复：valid 补账/缺失重建/活 CIC 不动；failed 条目重入）。
- **schema 演进状态机（转出 POC B4）**：`schema_evolution_test.go`（deprecated 读写屏蔽 + 查询拒绝 + 写拒收 + 回滚往返 + retire 两段；copy 迁移往返 integer→float 数据保真/swap 换名/retire 退役；validate 失败 → 修数 → 重入续跑；即时放宽零任务行；schema_version 消费递增；迁移窗口写拒收确定性锁定）。
- **数组列与 roles_sig（阶段③-b 包 A/C）**：`array_columns_test.go`（五元素类型 DDL 形态与读写往返、containsAny/All 语义矩阵（交集/子集/空数组/NULL 列/数值 cast）、写算子端到端（append/prepend/remove/unique 含 increment 组合、OCC 拒绝不落变更、remove 至空、unique 保序）+ **B1 集合族四算子**（intersect 去重保原序、diff 保序差集、filter=remove 受限等价、insert 0 基定点/越界尾插/NULL 归一，NULL 列三态与校验拒绝矩阵、BIGINT[] 路径、OCC 不变）、索引形态（GIN/unique 拒绝/多列拒绝））；`postgres_transactions_test.go` `TestExecuteTransactions_ArrayUpdates`（op 数组更新 + increment 同批、ATOMIC 回滚、PARTIAL per-op 失败、OCC 不变）；`roles_sig_test.go`（验签三态 fail-closed + 合法注入全链路回归 + tw_set_document_acl 注入面拒绝）；R13a 测试更新为 INSERT/UPDATE 双向锁死口径；`internal/infra/clients/roles_sig_test.go`（签名格式与主密钥派生、注入语句按角色分流）。
- `pkg/crud/`：AIP-132/158/160 抽象，`filter.go`/`order.go`/`pagination.go` 供静态表列表复用，动态文档优先 `pkg/query`。
- 参考：`internal/domain/databases/` 端口与 `Principal`；`internal/infra/documentdb/postgres*.go`；`pkg/query/proto/proto.go` typed AST；`db/migrations/` + `internal/infra/projectschema/`；`AGENTS.md` §数据库约定。
