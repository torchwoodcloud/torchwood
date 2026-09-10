# E-5 系统表化（users / sessions / files 静态表）

> 日期：2026-08-21（同日修订：补 S15 系统表并发，不把文档 `_version` 搬进静态表）  
> 状态：**已落地（owner 批准后施工完毕）**  
> 对应：`docs/review/first-principles-design.md` §11 E-5（验收只含旧号 **1 / 7 / 8**）；`docs/review/first-principles-plan.md` §4 E-5；`docs/design/project-data-plane-schema.md` K2 临时文档形态 + 「与系统表化的衔接」  
> 载体：`internal/infra/projectschema`（go:embed SQL，每 `tw_<project>` 一份 `schema_migrations`）  
> 读者：实施 PR 与 owner。产品 RPC 面不重开。

本文只改**表形态**（系统资源从 DocumentDB 动态表变为 bun 静态表 + FK），不改三层 schema 布局。物理位置已经在 `tw_<project>`（K-5）；本方案把 K2 的临时文档形态还掉。

---

## 0. Owner 已锁定

| 项 | 锁定 |
|---|---|
| 载体 | **只**用 `projectschema.Apply`（K-14）。禁止另起 golang-migrate / 全局 `db/migrations/` 建项目表 |
| 结构保险 | **完成前**不拆 `pkg/ident`、`businessSchema`、`documentSchema` sentinel 分叉、`RejectExternalDatabaseID`、系统集合守卫、`projectschema.Apply`（K-13 / K-15） |
| FK | `sessions.user_id → users(id)` **必做**。同源：`identities.user_id`、`memberships.user_id`（可空）、`memberships.group_id`、`files.bucket_id` |
| sentinel | **表先存在，再**退役系统集合寻址。禁止「先删 `_` 再搬表」 |
| 双契约 | Client 认证字段黑名单、系统集合 `version=0` **随表化一起消失**（验收旧号 7 / 8） |
| `secret_hash` | Wave 0 已 SHA-256 hex。E-5 **只换存储**，不重做哈希语义；拷贝原样。遗留明文仍走会话适配器双读 |
| 不在范围 | 用户 collection `read:any`（D-9）；staged tx（D-6）；查询 DSL / Query AST（E-4）；删 public 幽灵 catalog（D-7，除非顺手且零风险） |

---

## 1. Overview

今日七张系统资源（`users` `sessions` `identities` `groups` `memberships` `buckets` `files`）是 `EnsureSystemCollections` 经 `CreateCollection` 建的**文档表**：`_id` + `_tenant` + `_perms` + Appwrite DSL，无 FK。Account / Storage / Groups 热路径用 `SystemPrincipal` 绕过文档 ACE。为防 Databases API 摸到它们，堆了 sentinel `"_"`、字段黑名单、`version=0` 两套契约。

目标：

```text
public                         不变（控制面 + outbox）
tw_<project>                   系统表（静态 DDL + FK）+ catalog + 账本 + Functions
tw_<project>_<database>        只装用户 collection（含 _perms / _version）
```

验收（评审 §11，**只这三条**）：

| 旧号 | 退役后的可观察结果 |
|---|---|
| **1** | 系统资源不再是 collection。catalog 无 `database_id='_'` 行；Account / Users / Storage / Groups / Auth **不再**传 `SystemDatabaseID`。业务库仍可自建同名 `users` 集合 |
| **7** | `clientDocumentUpdateProtectedFields` / `filterClientProtectedFields` 删除。Client Documents API 不再认识 `password_hash` 等认证字段 |
| **8** | `databases.Document.Version` 不再出现「系统集合恒为 0」分支。系统表无 `_version`；Documents OCC 只服务用户 collection |

---

## 2. Background（事实，对照源码）

| 现状 | 位置 |
|---|---|
| 七集合 spec（列 / 索引 / ACE） | `internal/infra/documentdb/system_collection_specs.go` |
| 建表：`_id`+`_tenant`，系统集合无 `_version` | `createCollectionTable(..., isSystem=true)` |
| 寻址：`databaseID == "_"` → `tw_<project>` | `documentSchema`；`databases.SystemDatabaseID` |
| DDL 分叉：Create/DeleteDatabase 永不映射一段式 | `businessSchema` |
| User 聚合已抽出，适配器仍走文档 | `domain/users` + `infra/users/document_repo.go`（GetByEmail / GetByID / GetByPhone / Insert） |
| 用户其余写路径仍 `UpdateDocument` | `app/server/users.go` UpdateUser / UpdateUserPassword / DeleteUser cascade |
| 会话整段 DocumentDB | `infra/auth/session_service.go`；`secret_hash` = `HashOTP`（SHA-256 hex）；查找按 session ID |
| Groups / Storage 仍 `*databases.Document` | `app/server/groups.go`、`app/storage/storage.go` |
| billing 扫文件大小 | `SumDocumentField(..., SystemDatabaseID, "files", "size")` |
| Client 认证字段黑名单 | `app/client/databases.go` `clientDocumentUpdateProtectedFields`；`transactions.go` 同函数 |
| Server Databases 脱敏 | `app/server/databases.go` `serverSensitiveCollectionFields` |
| Wire | `infra.ProviderSet`：`NewDocumentRepository` bind `users.Repository` |

文档表与目标静态表**同名**（都叫 `users`），不能 `CREATE TABLE users` 覆盖。迁移必须 **copy then cut**（§6）。

---

## 3. Goals & Non-Goals

### Goals

1. 七张物理表落在 `tw_<project>`，静态 DDL，PK = `id`，**无** `_tenant` / `_perms` / 文档引擎列 `_version`。行级并发见 S15，不是再加一列通用 version。
2. `sessions.user_id` 等 FK 成为数据库不变式；`DeleteUser` 不再手写 List+逐条删会话。
3. `users.Repository` 从 DocumentDB 换 bun（样板）；Session / Identity / Group / Membership / Bucket / File 同期换，避免半套双写。
4. 新建项目：Apply 一次出最终表名，**从不**再 `CreateCollection` 这七张。
5. 存量项目：staging 表拷贝 → 停写窗口切适配器 → 删文档表并 rename。
6. 表存在之后再退役 sentinel 寻址与 Databases 系统集合守卫。
7. Client 黑名单与系统集合 `version=0` 随 cut 删除。

### Non-Goals

- 不改三层 schema、`businessSchema`、`RejectExternalDatabaseID`（cut 之后 sentinel 分支可留作皮带）。
- 不把用户 collection 改成静态表，不删业务库 `_perms` / `_version` / `documentSecurity`。
- 不改用户 collection 默认 `read:any`（D-9）。
- 不砍 staged transactions API（D-6）。
- 不把 Appwrite 字符串升成 Query AST（E-4）；系统表 List 只用**列白名单**编译现有 `queries[]`。
- 不删 `public.document_*` 幽灵 catalog（D-7），除非该 PR 已在碰全局 migrate 且确认零读路径。
- 不重做 `secret_hash` 协议、不发明 secret-bearer。
- 不把 201 RPC 砍掉，不改 User / Session proto 字段布局（无 version 字段可删）。
- 不引入跨实例分库；不 `SET search_path`。
- 不在本方案做 MinIO 孤儿清理（DeleteProject 已知缺口）。
- 不要求生产零停机双写长共存（与数据面方案一致：开发期重建；生产给拷贝草图 + 停写窗口）。

---

## 4. Key Decisions

| ID | 决策 | 选择 | 理由 |
|----|------|------|------|
| S1 | DDL 载体 | `000008_system_tables.up.sql` 建 **staging** 名 `sys_*`；`000009_system_tables_cut.up.sql` DROP 文档表 + RENAME 为最终名 | 与现有 `users` 文档表同名冲突；expand 对旧进程安全 |
| S2 | 最终表名 | `users` `sessions` `identities` `groups` `memberships` `buckets` `files` | owner 锁定；API id 不改 |
| S3 | `_tenant` | **去掉** | 系统表化整表重写时自然消失（数据面 K12 / 退役清单） |
| S4 | `_perms`（系统路径） | **去掉**。授权改服务级：会话属主、组成员、bucket policy | D-3：系统路径本就 `SystemPrincipal` 绕过 |
| S5 | 文档 `_version` | 系统表**不加** `_version` 列；Documents OCC 只留用户 collection | 旧号 8 退役的是「系统集合恒 version=0」双契约，不是「身份行永不冲突」 |
| S15 | 系统行并发 | **不**给七张表加通用 `version` BIGINT，**不**给 User/Session/File/Group proto 加 If-Match。按写形状选工具（§5.10） | 今日系统集合本就无 OCC（`SimpleDocumentUpdate`）；复制文档 OCC 会重开产品 API。真正的丢更新用列级 UPDATE / SQL 增量 / 状态 CAS / `FOR UPDATE` |
| S6 | `project_id` 列 | **不加** | schema 已是租户容器；文档表也没有；拷贝 SQL / Go 不必填。仓储靠 `ProjectTable` 限定 schema |
| S7 | 拷贝 | Go 作业（`projectschema.CopySystemDocuments`），不是 SQL 文件里的 `INSERT…SELECT` | 需要 `projectID`、孤儿行、类型规整、`secret_hash` 原样；Apply 只有 `{{schema}}` |
| S8 | 双写 | **不做**长期双写。expand 只拷贝；cut 停写窗口 | 与数据面方案一致；半套双写比停写窗口更险 |
| S9 | User 端口 | cut 时扩展 `users.Repository`（Update / Delete / List），Wire 一把切到 bun | 今日只覆盖 Create/查邮箱；Update 仍走文档 |
| S10 | List | 继续吃现有 `ListRequest.queries`；适配器按**表列白名单**编译。禁止再走 DocumentDB | 不是 E-4；不发明第三套查询面 |
| S11 | 新项目 | CreateProject 同 Tx Apply 000008+000009 → 直接最终名；`EnsureSystemCollections` 不再建这七集合 | 空库无拷贝 |
| S12 | cut 启动 | cut 版本 **同步** `EnsureAll` 成功后再 listen；禁止只靠后台 `KickoffEnsureAll` | 否则 rename 与旧文档路径竞态 |
| S13 | 系统表 ACE | 不把 collection ACE 搬进静态表。`buckets.permissions` / `groups.permissions` **产品字段**保留为 JSONB | 那是 bucket/group 策略，不是文档 `_perms` |
| S14 | `groups.total` | 保留 BIGINT；cut 后写路径改为 SQL `total = total + $delta`（下限 0）；`DeleteUser` 后对受影响组 `COUNT(*)` 重数 | 今日 `adjustGroupTotal` 是读-改-写，并发会丢计数；静态表必须收口。不靠触发器当第一版 |

---

## 5. 物理表（`tw_<project>`）

占位符 `{{schema}}` 与现网 `projectschema` 一致，Apply 替换为 `quoteIdent(ProjectSchemaName(id))`。禁止未限定表名。索引必须普通 `CREATE INDEX`（Apply 在事务内，**禁止** `CONCURRENTLY`）。

Staging 名 = `sys_` 前缀；000009 再 rename。下文 DDL 写最终名，000008 文件里把表名换成 `sys_users` 等。

### 5.1 `users`

对应 spec：`email` `password_hash` `name` `status` `email_verified` `pending_email` `phone` `phone_verified` `labels` `prefs` `factors`。领域：`domain/users.User`（`factors` 暂无聚合字段，列仍建，避免 MFA 路径丢数据）。

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_users (
    id              TEXT PRIMARY KEY,
    email           VARCHAR(320) NOT NULL,
    password_hash   VARCHAR(512) NOT NULL DEFAULT '',
    name            VARCHAR(256) NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'inactive', 'blocked')),
    email_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    pending_email   VARCHAR(320) NOT NULL DEFAULT '',
    phone           VARCHAR(64) NOT NULL DEFAULT '',
    phone_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    labels          JSONB NOT NULL DEFAULT '[]'::jsonb,
    prefs           JSONB NOT NULL DEFAULT '{}'::jsonb,
    factors         JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS sys_users_email_unique
    ON {{schema}}.sys_users (email);
CREATE INDEX IF NOT EXISTS sys_users_phone
    ON {{schema}}.sys_users (phone)
    WHERE phone <> '';
```

拷贝：`_id → id`，`_created_at/_updated_at` 原样；丢 `_tenant` `_created_by` `_updated_by`。`labels`/`prefs`/`factors` 经 JSONB 规整（NULL → 默认）。邮箱已有文档唯一索引，冲突则 **fail the copy**（不要静默丢行）。

### 5.2 `sessions`

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES {{schema}}.sys_users(id) ON DELETE CASCADE,
    secret_hash  TEXT NOT NULL,
    provider     VARCHAR(64) NOT NULL DEFAULT 'email',
    user_agent   VARCHAR(1024) NOT NULL DEFAULT '',
    ip           VARCHAR(64) NOT NULL DEFAULT '',
    country      VARCHAR(8) NOT NULL DEFAULT '',
    factors      JSONB NOT NULL DEFAULT '{}'::jsonb,
    expire_at    TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS sys_sessions_user_id
    ON {{schema}}.sys_sessions (user_id);
CREATE INDEX IF NOT EXISTS sys_sessions_user_expire
    ON {{schema}}.sys_sessions (user_id, expire_at);
```

- **FK 必做**（计划原文）。拷贝前检测孤儿 `user_id`（文档 sessions 行的 user 已不在 users）：**有则作业失败**，不写入 staging、不提供「默默丢掉算成功」开关。
- `secret_hash`：**原样拷贝**。新行仍 `HashOTP`（64 hex）。列用 TEXT，兼容 Wave 0 遗留明文 UUID（36）与哈希（64），会话适配器双读逻辑原样搬到 bun 路径。
- 查找仍按 **session id**，不按 secret。
- 上限驱逐（K-22）：`ORDER BY expire_at ASC` 删最旧，行为与 `evictOldestSessions` 一致。
- `country` spec 有、创建路径今日常空，列保留。

### 5.3 `identities`

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_identities (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES {{schema}}.sys_users(id) ON DELETE CASCADE,
    provider        VARCHAR(64) NOT NULL,
    provider_uid    VARCHAR(256) NOT NULL,
    provider_email  VARCHAR(320) NOT NULL DEFAULT '',
    provider_data   JSONB NOT NULL DEFAULT '{}'::jsonb,
    expire_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS sys_identities_user_id
    ON {{schema}}.sys_identities (user_id);
CREATE UNIQUE INDEX IF NOT EXISTS sys_identities_provider_uid
    ON {{schema}}.sys_identities (provider, provider_uid);
```

领域：`domain/auth.Identity`。拷贝遇 `(provider, provider_uid)` 冲突 fail-closed。

### 5.4 `groups`

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_groups (
    id           TEXT PRIMARY KEY,
    name         VARCHAR(256) NOT NULL,
    permissions  JSONB NOT NULL DEFAULT '[]'::jsonb,
    total        BIGINT NOT NULL DEFAULT 0 CHECK (total >= 0),
    prefs        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS sys_groups_name
    ON {{schema}}.sys_groups (name);
```

`permissions` 是 **产品字段**（组级授权列表），不是 `_perms` 行。`total` = accepted 成员数；cut 后用 SQL `UPDATE … SET total = GREATEST(total + $delta, 0)`，禁止再读出后回写（S14）。

### 5.5 `memberships`

今日 app 层按 group+user / 规范化 email 查重，文档 spec **没有** unique。静态表把不变式收进库：

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_memberships (
    id          TEXT PRIMARY KEY,
    group_id    TEXT NOT NULL REFERENCES {{schema}}.sys_groups(id) ON DELETE CASCADE,
    user_id     TEXT REFERENCES {{schema}}.sys_users(id) ON DELETE CASCADE,
    email       VARCHAR(320) NOT NULL DEFAULT '',
    name        VARCHAR(256) NOT NULL DEFAULT '',
    roles       JSONB NOT NULL DEFAULT '[]'::jsonb,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'accepted', 'rejected')),
    invited_at  TIMESTAMPTZ,
    joined_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (user_id IS NOT NULL OR email <> ''),
    CHECK (status <> 'accepted' OR user_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS sys_memberships_group_id
    ON {{schema}}.sys_memberships (group_id);
CREATE INDEX IF NOT EXISTS sys_memberships_user_id
    ON {{schema}}.sys_memberships (user_id)
    WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sys_memberships_email
    ON {{schema}}.sys_memberships (email)
    WHERE email <> '';
CREATE UNIQUE INDEX IF NOT EXISTS sys_memberships_group_user
    ON {{schema}}.sys_memberships (group_id, user_id)
    WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS sys_memberships_group_email
    ON {{schema}}.sys_memberships (group_id, email)
    WHERE email <> '';
```

- 待邀请：`user_id` 可空（今日 CreateMembership 允许 email-only pending）。
- `DeleteUser`：FK CASCADE 删成员行；随后对受影响 `group_id` `SET total = COUNT(*) FILTER (status='accepted')`。
- `DeleteGroup`：FK CASCADE 删成员，不必再循环 `DeleteDocument`。
- 拷贝前先跑查重；重复行 fail-closed（与现网 `ensureMembershipUnique` 一致，重复本就是缺陷）。

### 5.6 `buckets`

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_buckets (
    id           TEXT PRIMARY KEY,
    name         VARCHAR(256) NOT NULL,
    permissions  JSONB NOT NULL DEFAULT '[]'::jsonb,
    public       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS sys_buckets_name
    ON {{schema}}.sys_buckets (name);
```

`permissions` = bucket policy 产品字段（谁可读文件），继续给 Storage use-case 用，不映射成 `_perms`。

### 5.7 `files`

```sql
CREATE TABLE IF NOT EXISTS {{schema}}.sys_files (
    id             TEXT PRIMARY KEY,
    bucket_id      TEXT NOT NULL REFERENCES {{schema}}.sys_buckets(id) ON DELETE CASCADE,
    name           VARCHAR(256) NOT NULL,
    mime_type      VARCHAR(128) NOT NULL DEFAULT '',
    size           BIGINT NOT NULL DEFAULT 0 CHECK (size >= 0),
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    owner_user_id  TEXT REFERENCES {{schema}}.sys_users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS sys_files_bucket_id
    ON {{schema}}.sys_files (bucket_id);
CREATE INDEX IF NOT EXISTS sys_files_owner
    ON {{schema}}.sys_files (owner_user_id)
    WHERE owner_user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sys_files_name_fts
    ON {{schema}}.sys_files USING gin (to_tsvector('simple', name));
```

- `owner_user_id` 从文档 `_created_by` 回填（今日 owner ACE 来自创建者）。`DeleteUser` **不**删文件（与现 cascade 一致）→ `ON DELETE SET NULL`。
- 孤儿 `bucket_id`：拷贝 fail-closed（与 sessions 孤儿同一策略：有则失败，不丢行）。
- billing：`SELECT COALESCE(SUM(size),0) FROM tw_<p>.files` 替代 `SumDocumentField`。
- 对象字节仍在 MinIO；本表只是元数据。

### 5.8 系统表 **没有** 的东西

| 不要 | 原因 |
|---|---|
| `_id` `_tenant` `_created_by` `_updated_by` `_version` | 文档引擎列（S5）。系统行并发走 §5.10，不加同名列 |
| `tw_<project>._perms` 里这七个 collection 的行 | ACL 改服务级 |
| catalog `document_collections is_system=true` | 它们不再是 collection |
| 出站 `databases.documents.*` / 文档 `_version` OCC | 维持 v2：系统资源不进文档 outbox |

业务库 `tw_<project>_<database>._perms` **不动**。

### 5.9 授权替换（系统路径，非 D-9）

| 资源 | 今日 | cut 之后 |
|---|---|---|
| users | 文档 ACE `user:{id}` + keys 只读 + admin；热路径 SystemPrincipal | Users / Account RPC 拦截器；行 = 该用户。端用户只打 Account，不打行级 ACE |
| sessions | ACE owner/admin；`EnsureActiveSession` 比 `user_id` | `sessions.user_id` + 会话 RPC。keys 不得改删（保持 C1） |
| identities | ACE | `user_id` 属主 + admin/keys 读 |
| groups / memberships | 文档 ACE `group:{id}` + 产品 `permissions` JSON | 拦截器 + membership 角色；JSON 字段保留 |
| buckets / files | collection `read:any` + 文档 ACE + bucket JSON | Server/Client Storage 拦截器 + `buckets.permissions` + `files.owner_user_id` + `public` |

GetStorageUsage：不再 `CountDocuments`/`SumDocumentField` 走 `_perms` 可见性；改为 SQL 计全部（Server）或 use-case 按 bucket policy 过滤（若 Client 有用量 API）。行为与「collection read:any」对齐的路径保持全量可计。

### 5.10 系统行并发（S15）

#### 事实（今日，不是目标）

系统集合**没有** `_version` 列（`createCollectionTable(..., isSystem=true)`）。出站 `Document.Version` 被 `zeroSystemDocumentVersion` 置 0。Account / Users / Groups / Storage / Session 写路径一律 `SimpleDocumentUpdate`（`ExpectedVersion=0`），`updateDocument` 对 `isSystem` 走 `UPDATE … WHERE _id AND _tenant`，**不做 OCC**。

旧号 8 要退役的是这套「假装是文档、version 恒 0」的双契约，不是给身份行补一套 If-Match。

用户 collection 的 `_version` 仍是产品功能（Client/Server Documents 强制 OCC）。经济持有 `asset_holdings.version` 是账本聚合自己的锁，与系统表无关，不当样板。

User / Session / File / Group proto **没有** `version` 字段。给七张表加 `version BIGINT` 却不暴露 If-Match，只保护「仓储把整行 Get 后再全列 Update」这一种写法；正确做法是禁止那种写法。

#### 丢更新真正发生在哪

标量列的**分列** `UPDATE` 本来就不会互相覆盖：`UpdateUserPassword` 只 SET `password_hash`，`UpdateAccount` 只 SET `name`/`email`，`UpdatePrefs` 只 SET `prefs`。危险的是同一 JSON/计数/状态的读-改-写：

| 路径 | 今日 | cut 后锁定 |
|---|---|---|
| `users.email` / `phone` | 唯一索引兜底 | 保持；冲突 → AlreadyExists |
| `users.password_hash` / `name` / `status` / 验证位 | 分列 SET | 仓储 **必须** `UPDATE` 点名列，禁止 bun `Model(user)` 无 `Column` 列表的整行写 |
| `users.factors`（`saveFactors` 读-改-写整段 JSON） | 无锁，并发 enroll 会丢因子 | 同一 Tx：`SELECT … FOR UPDATE` 该用户行，再 SET `factors`。失败原样返回，调用方不得继续用内存副本 |
| `users.prefs` / `groups.prefs` | 客户端 PUT 整对象 | **保持 last-write-wins**（产品就是替换整份 prefs）。不要因此给 User/Group 加 version |
| `users.labels` | 管理端整段替换 | 同 prefs |
| `groups.total` | `adjustGroupTotal` 读出 +1 再写 | SQL `total = GREATEST(total + $delta, 0)`；`DeleteUser` 后 `COUNT(*) FILTER (status='accepted')` 重数（S14） |
| `memberships.status` | 内存看 pending 再写 | `UPDATE … SET status=$new WHERE id=$id AND status='pending'`，0 行 → FailedPrecondition（与今日「不是 pending」同码） |
| `memberships.roles` | 整段替换 + last-owner 预检 | 预检与写在同一 Tx，对 membership 行 `FOR UPDATE`；last-owner 扫描同一组其它行 |
| `sessions` | 几乎只有 Insert / Delete | 不需要 version。遗留 `secret_hash` 明文重哈希若仍要写，CAS：`UPDATE … SET secret_hash=$new WHERE id=$id AND secret_hash=$old` |
| `identities` | Insert + unique `(provider, provider_uid)` | 冲突 fail-closed；无行内 OCC |
| `buckets` 标量 / `files` 标量 | 分列 SET | 同 users 标量 |
| `buckets.permissions` / `files.metadata` | 整段 JSON 替换 | 与 prefs 相同：产品 PUT，last-write-wins。若日后 Storage 要 If-Match，另开产品单，**不进 E-5** |

#### 明确不选

- **七张表都加 `version` + proto If-Match**：把 Documents OCC 搬到 Account/Users，breaking，且今日调用方没有 version 可传。
- **只在表上加内部 `version`、API 不暴露**：治不好 JSON RMW（调用方仍整段覆盖）；只会让仓储每次 UPDATE 多带一个调用方不知道的谓词。`FOR UPDATE` / 分列 SET / 状态 CAS 更贴写形状。
- **用 `updated_at` 当 OCC 令牌**：时钟回拨与同毫秒碰撞；仍要 API 表面。否决。
- **给系统表加文档 `_version` 或文档 outbox**：旧号 8 + 数据面「系统资源不进 documents.*」。

cut 适配器的单测至少覆盖：并发两次 `saveFactors` 不丢因子（或第二次事务失败可重试）；并发两次 accept membership 只有一次 `total+1`；`UpdateUserPassword` 与 `UpdatePrefs` 交错不互相覆盖。

---

## 6. 迁移：新建 vs 存量（copy then cut）

```mermaid
sequenceDiagram
    participant Dev as 空库 / 新项目
    participant Exist as 存量 tw_shop（已有文档 users）
    participant Apply as projectschema.Apply
    participant Copy as CopySystemDocuments
    participant Code as 进程

    Dev->>Apply: 000008 建 sys_* 空表
    Dev->>Apply: 000009 DROP IF EXISTS 文档表; RENAME sys_* → 最终名
    Note over Dev,Apply: 同一次 CreateProject Tx，无拷贝

    Exist->>Apply: 000008 建 sys_*（文档 users 仍在）
    Exist->>Copy: INSERT 文档列 → sys_*
    Note over Exist,Code: 旧进程仍读写文档表
    Exist->>Code: 停写 / drain
    Exist->>Copy: 再拷一次增量（见 §6.3）
    Exist->>Code: 部署 cut 二进制（bun + 最终名常量仍指向 sys_* 或 rename 后）
    Exist->>Apply: 000009 DROP 文档表与系统 _perms; RENAME
```

### 6.1 新建项目（无文档表）

CreateProject 已有顺序（数据面 §4.1）不变，只改第 3–4 步：

```text
CreateProject(id=shop)  -- 同一 bun.Tx
  1. INSERT public.projects
  2. CREATE SCHEMA tw_shop
  3. projectschema.Apply          -- 含 000008 + 000009 → 最终名 users/…
  4. EnsureSystemCollections      -- 不再 CreateCollection 七名单
  5. CreateDatabase(firstDBID)    -- 两段式业务库
```

000009 必须幂等：`DROP TABLE IF EXISTS {{schema}}.users` 在空库是 no-op（000008 建的是 `sys_users`，尚未有最终名）；然后 `ALTER TABLE sys_users RENAME TO users`。约束/索引名一并 rename（或 DROP INDEX + 按最终名重建；空表秒级）。

**禁止** 000008 直接建最终名 `users`：存量项目会撞文档表。新项目靠 000008+000009 同一 Tx 收口。

### 6.2 存量：expand（文档表继续服务）

PR-E5-1（只 DDL + 拷贝作业，**不切** Wire）：

1. 合入 `000008_system_tables.up.sql`（仅 `sys_*`）。
2. 启动 `EnsureAll` / CreateProject 建空 staging。
3. `CopySystemDocuments(ctx, db, projectID)`（Go）：
   - 探测：`information_schema` 同时存在 `users`（有 `_id`）与 `sys_users`（有 `id`）。
   - 无文档表 → return（新项目 cut 后不会走这里）。
   - 按 §5 列映射 `INSERT INTO sys_users SELECT … FROM users`（可 `TRUNCATE sys_*` 再全量，expand 阶段 staging 无生产读）。
   - 顺序：`users → groups → buckets → sessions/identities/memberships/files`（FK 依赖）。
   - 孤儿 / unique 冲突：返回 error，staging 视为不可用，**不进入 cut**。000008 是 DDL 版本，拷贝失败不标 `schema_migrations` dirty（那只给 Apply DDL 用）。拷贝失败 = 指标 + 阻断 E5-4。
4. 旧适配器仍写文档表。staging 会陈旧，这是允许的——cut 前会再拷一次。

探测「是文档表」：`users` 存在且有列 `_id`。禁止用 catalog `is_system` 当唯一依据（cut 后 catalog 行会删）。

### 6.3 存量：cut（停写窗口）

PR-E5-4 与适配器切换 **同一发布**（或紧挨的窗口，禁止「库已 rename、二进制仍 DocumentDB」）：

```text
1. drain / 停写（新连接拒绝写系统资源；或整体维护页）
2. CopySystemDocuments 再跑全量（staging TRUNCATE + 拷贝；停写后无增量）
3. 校验：`sys_users` 行数 = 文档 `users` 行数（sessions/identities/… 同理）。孤儿在拷贝阶段已 fail-closed，此处不相等即中止 cut
4. 部署 cut 二进制：
   - bun ModelTableExpr 指向最终名 users（见 §6.4 时序）
   - 同步 EnsureAll（含 000009）成功才 listen
5. 000009：
   DELETE _perms WHERE _collection IN (七名单);
   DROP TABLE users, sessions, identities, groups, memberships, buckets, files;
   ALTER TABLE sys_users RENAME TO users;  -- 其余六张同理
   -- 索引/约束改名到无 sys_ 前缀
   DELETE catalog 七集合行 + document_databases id='_'
6. EnsureSystemCollections 跳过七名单（见 §8）
```

开发期：**重建**（`task down && task up`）。内测无存量不必跑拷贝。

### 6.4 rename 与二进制表名（避免滚动窗口撕表）

旧进程读写 `users`（文档列 `_id`）。000009 rename 后 `users` 是静态列 `id`。因此：

- **expand 二进制**永远碰文档 `users`，绝不读 `sys_users`。
- **cut 二进制**在 000009 **之后**才服务流量，只碰最终名。
- 禁止滚动升级期间「部分 pod 已 Apply 000009、部分 pod 仍是 expand 代码」。cut = 停写 → 全进程替换 → Apply → 再放流量。

单进程 dev：启动时同步 Apply 即可。

### 6.5 `EnsureSystemCollections` 在两阶段的行为

| 阶段 | 行为 |
|---|---|
| expand（000008 已应用，000009 未） | **仍** CreateCollection 七名单（旧路径活着）。不得因 `sys_users` 存在而跳过 |
| cut（000009 已应用） | **禁止** `CreateCollection` 七名单。探测最终表 `users` 有列 `id` 且无 `_id` → skip。catalog sentinel 已删则 `GetCollection(_, users)` 为 nil，不得重建 |
| 之后 | 调用点改为 no-op 或删除（§8 清单） |

实现建议：`EnsureSystemCollections` **一进来**就 `systemTablesReady(schema)`（最终 `users` 有列 `id` 且无 `_id`）→ 直接 return。必须发生在 `ensureSchemaAndPerms` / `CreateCollection` 之前，否则会把已 `DROP` 的 `tw_<project>._perms` 建回来。调用点删除放 contract。

---

## 7. UserRepository：DocumentDB → bun

这是样板。Session / Identity / Group / Membership / Bucket / File 照抄（端口 + bunrepo + Wire）。

### 7.1 今日缝

```text
app/server.Users.CreateUser
  → users.Register + users.Repository.Insert
  → infra/users.DocumentRepository
      CreateDocument(project, "_", "users", …, SystemPrincipal)

app/server.Users.UpdateUser / UpdateUserPassword / GetUser / ListUsers / DeleteUser
  → 仍直接 docDB.*（端口未覆盖）
```

Wire：`infrausers.NewDocumentRepository` bind `domainusers.Repository`。

### 7.2 端口（cut 时一次扩完）

```go
// internal/domain/users/repository.go
type ListFilter struct {
    Queries   []string // 现有 ListRequest.queries，白名单编译
    PageSize  int32
    PageToken string
}

type ListResult struct {
    Users         []*User
    TotalCount    int64
    NextPageToken string
}

type Repository interface {
    GetByEmail(ctx context.Context, projectID, email string) (*User, error)
    GetByID(ctx context.Context, projectID, id string) (*User, error)
    GetByPhone(ctx context.Context, projectID, phone string) (*User, error)
    Insert(ctx context.Context, projectID string, user *User) error
    Update(ctx context.Context, projectID string, user *User) error
    Delete(ctx context.Context, projectID, id string) error
    List(ctx context.Context, projectID string, f ListFilter) (*ListResult, error)
}
```

- `Insert` 邮箱冲突 → `ErrEmailAlreadyRegistered`（今日从 PG `users_email_unique` 文案解析，改认 `sys_users_email_unique` / 最终 `users_email_unique`）。
- `Update` 替代 `UpdateDocument` 字段袋；use-case 先 GetByID 再改聚合再 Update。
- `Delete` 只删 `users` 行；会话/身份/成员靠 FK。use-case 仍要重数 `groups.total`。
- `List`：`pkg/query.Parse` 后只允许列 `email,name,status,phone,created_at,updated_at`；`equal/greaterThan/lessThan/orderAsc/orderDesc/limit/offset`。未知属性 → InvalidArgument。**不要**调用 DocumentDB，**不要**新公开查询语言。
- `DocumentData()` / `DocumentPermissions()`：cut 后无调用方则删（contract）。测试 fake 实现本接口，禁止再拿 `fakeDocDB` 冒充 User 深度（E-1 已有此验收）。

### 7.3 bun 适配器

落点：`internal/infra/bun/model/users.go` + `internal/infra/bun/bunrepo/users_repo.go`（与 functions 同构）。

```go
func (r *userRepo) GetByEmail(ctx context.Context, projectID, email string) (*domainusers.User, error) {
    conn, sch, expr, err := Scoped(ctx, r.db, projectID, "users", "u")
    // ...
    err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
        Where("u.email = ?", domainusers.NormalizeEmail(email)).
        Scan(ctx)
}
```

- `Scoped` 仍 `projectschema.Apply` + `ProjectTable`（functions 已如此）。cut 后 Apply 是 no-op。
- **禁止** `WHERE project_id=?`（S6 无该列）。租户边界 = schema。
- 空 projectID → 现有 `errEmptyProjectID`（禁止扫全实例）。
- 单测：`fake` 实现端口；集成测走 `pkg/testutil` PG。

### 7.4 Wire 切换（一把，不要双 bind）

`internal/infra/provides.go`：

```go
// 删：
infrausers.NewDocumentRepository,
wire.Bind(new(domainusers.Repository), new(*infrausers.DocumentRepository)),

// 加：
bunrepo.NewUserRepository, // 放入 bun.ProviderSet 亦可
wire.Bind(new(domainusers.Repository), new(*bunrepo.UserRepository)),
```

`task wire-all`。测试夹具（`NewUsers(..., infrausers.NewDocumentRepository(docDB))`）改 bun repo 或 fake。

**不要** feature flag 双实现长期共存。expand PR 不切 Wire；cut PR 切。

### 7.5 同批必须切的调用方（否则 cut 后立刻坏）

| 调用方 | 今日 | cut |
|---|---|---|
| `Users.CreateUser` / Account SignUp | 已走 `users.Repository` | 自动吃 bun |
| `Users.GetUser` `ListUsers` `UpdateUser` `UpdateUserPassword` `DeleteUser` | `docDB` | 改端口 |
| `Users.ListUserSessions` `DeleteUserSession` `CreateUserToken` | `docDB` sessions | `SessionRepository` |
| `auth.SessionService` | `docDB` | 注入 `SessionRepository`；驱逐/Ensure/DeleteByUser 改 SQL |
| `auth.Validator` 读 session/user | `GetDocument` | 端口 |
| Account identities / MFA factors | `docDB` | Identity repo + users.factors 列 |
| `server.Groups` | `docDB` + `users.Repository` 解析邮箱 | Group/Membership bun；`users.GetByEmail` 已在 |
| `storage.Storage` | `docDB` buckets/files | Bucket/File bun |
| `billing.sampleStorage` | `SumDocumentField` | `SUM(files.size)` |
| Client Account | 已部分走 `users.Repository` | 剩余文档读写清掉 |

`EnsureSystemCollections` 从上述 `resolveProject` / `loadProject` **可以暂时留着**（cut 后 skip），contract 再删调用。

### 7.6 Session 端口（与 User 同 PR 或紧后）

K-22 行为保持：上限驱逐、rotation key=`project+session`、reuse→删 session。只换存储：

```go
type SessionRepository interface {
    Insert(ctx, projectID string, s *Session) error
    GetByID(ctx, projectID, id string) (*Session, error)
    ListByUser(ctx, projectID, userID string) ([]Session, error)
    Delete(ctx, projectID, id string) error
    DeleteByUser(ctx, projectID, userID string) error
    DeleteOldestByUser(ctx, projectID, userID string, keep int) error
}
```

`secret_hash` 写入仍 `HashOTP`；`EnsureActiveSession` 比 `user_id` + `expire_at`。明文遗留双读从 `session_service.go` 原样搬来。

---

## 8. Sentinel 退役清单（**表存在之后**）

顺序锁定：000009 已把最终表改好 **并且** bun 适配器在生产路径上 → 才能删寻址。expand 阶段一条都不拆。

对照数据面「与系统表化的衔接」+ 验收 1/7/8：

| # | 资产 | 何时 | 做法 |
|---|---|---|---|
| 1 | `sys_*` 有数据 / 新库最终表已建 | expand / 新项目 Apply | — |
| 2 | bun 适配器切完 | cut | §7 |
| 3 | 文档表 DROP + rename | cut 000009 | §6.3 |
| 4 | `tw_<project>._perms` 系统行 | cut 000009 | `DELETE WHERE _collection IN (七名单)`；随后 **`DROP TABLE tw_<project>._perms`**（该 schema 的 `_perms` 只服务系统集合；业务库另有自己的 `_perms`） |
| 5 | `cleanupKeysWritePerms` | cut | 删除函数与 `keysPermsCleaned` |
| 6 | catalog `document_databases(id='_')` + 七条 `document_collections/attributes/indexes` | cut 000009 | DELETE。`RejectExternalDatabaseID` **保留** |
| 7 | `EnsureSystemCollections` 建七集合 + `system_collection_specs.go` | cut 后 contract | skip 已落地则删 spec / 循环 |
| 8 | 生产路径 `SystemDatabaseID` / `"_"` 实参 | contract | grep 数据面 §8.2 清单（account / storage / groups / auth / billing）必须为零 |
| 9 | `documentSchema` sentinel 分叉 | contract | 无调用后删除分支；**保留 `businessSchema`** |
| 10 | `IsSystemCollection` / `IsSystemCollectionID` / `isWriteProtectedSystemCollection` | contract | Databases API 不再需要。业务库同名 `users` 仍是普通集合（K6） |
| 11 | Client `clientDocumentUpdateProtectedFields` + `filterClientProtectedFields`（databases **与** transactions） | cut | **整表删除**（验收 7）。用户 collection 可以有叫 `password_hash` 的普通字段 |
| 12 | Server `serverSensitiveCollectionFields` + Client 读路径拒 `SensitiveSystemCollectionIDs` | cut | 删除。敏感数据只经 Users/Account |
| 13 | `databases.Document.Version`「系统集合恒 0」+ `createCollectionTable isSystem` 跳过 `_version` + query 拒 `$version` on system | cut | 删系统分支（验收 8）。用户集合 OCC 不动 |
| 14 | `User.DocumentData` / `DocumentPermissions` | contract | 无引用则删 |
| 15 | `infra/users/document_repo.go` | cut | 删除；测试改 fake/bun |
| 16 | billing `SumDocumentField(files)` | cut | 直查 `files.size` |
| 17 | Console Databases 页系统徽章 / `is_system` | 若仍显示 sentinel | 应已无行；回归一眼 |

**明确不删（cut 之后仍留）：**

- `pkg/ident.ProjectDataPlaneID`、`ValidateSchemaResourceID` 拒 `_`（K-13）
- `businessSchema` / `IsTwoSegmentSchema`（防 `DeleteDatabase("_")` DROP `tw_shop`）
- `RejectExternalDatabaseID`
- `projectschema.Apply` / `EnsureAll`
- 用户 collection `_perms` `_version` `_tenant`（业务库）
- Redis 旋转 / 限流 / 上传会话（K-16）

### 8.1 grep 验收（cut 完成后，生产代码排除 `_test.go`）

```text
CreateDocument|GetDocument|ListDocuments|UpdateDocument|DeleteDocument|SumDocumentField|CountDocuments|BulkDeleteDocuments
(..., SystemDatabaseID | ident.ProjectDataPlaneID | "_", "users"|"sessions"|"identities"|"groups"|"memberships"|"buckets"|"files")
```

命中必须为零。另：

- `rg clientDocumentUpdateProtectedFields` → 0
- `rg serverSensitiveCollectionFields` → 0
- `rg system_collection_specs` → 0（contract）
- `document_databases` 无 `id='_'`
- `information_schema.columns`：`tw_<p>.users` 有 `id` 无 `_id`、无 `_version`

---

## 9. Rollback

`projectschema` **没有**生产 down 文件（数据面 K8）。回滚靠阶段，不靠 `000008.down.sql`。

| 阶段 | 已落地 | 回滚 |
|---|---|---|
| 仅 000008 + 拷贝 | 文档表仍是权威 | 二进制回 expand 前；`DROP TABLE sys_*`；`DELETE FROM schema_migrations WHERE version=8`。**禁止** drop 文档表 |
| 停写已开始、000009 未跑 | staging 可能新于文档（若曾切错过） | 取消 cut，恢复写文档表；丢弃 staging |
| 000009 已 rename | 文档表已 DROP | **不能**靠旧二进制。从窗口前快照恢复 schema，或 `task down && task up` 重建。cut 前必做 `pg_dump -n tw_<project>` |
| Wire 已切 bun、库仍文档表 | 不会发生（发布纪律） | — |

开发期回滚 = 重建。生产 cut 清单必须包含：快照、行数校验、同步 Apply、失败则停在维护页而不是半新半旧进程。

---

## 10. 验收对照（只 1 / 7 / 8）

| 旧号 | 测什么 | 不测什么 |
|---|---|---|
| 1 | 新项目无 `document_databases(_)`；`GetUser` 不碰 DocumentDB；业务库 `CreateCollection("users")` 仍成功且有 `_version`；`DeleteDatabase("default")` 不影响 `tw_<p>.users` | 用户 collection ACE 语法、read:any |
| 7 | Client `UpdateDocument` / `UpsertDocument` / staged op 可写名为 `password_hash` 的**用户集合**字段；Account 改密仍走专用 RPC | 系统集合再经 Documents 写（应 NotFound/InvalidArgument） |
| 8 | `GetDocument` 用户集合仍 OCC；代码无「isSystem → version=0」；系统表 `information_schema` 无 `_version` | 给系统表 / User proto 补 Documents 式 If-Match（S15 明确不选） |

S15 回归（随 E5-2/E5-3 适配器，不是旧号 8 的验收定义）：并发 `saveFactors` 不丢因子；并发 accept membership 的 `groups.total` 不丢加；分列 UPDATE 交错不整行覆盖。

回归（必须绿，但不是 E-5 完工定义）：SignUp / CreateUser（V-1）、会话登录与上限驱逐、OAuth identity 唯一、Groups 邀请查重、Storage 上传下载、billing storage sample、DeleteUser 后会话/身份/成员不残留、DeleteGroup 成员干净。

---

## 11. 建议 PR 切片

未批准本设计前 **零施工**。批准后建议四张 PR，不把未完成波次混进同一提交。

| PR | 内容 | 不做什么 |
|---|---|---|
| **E5-1 expand** | `000008_system_tables.up.sql`；`CopySystemDocuments`；单测探测/孤儿/顺序；文档注释 | 不切 Wire；不删 sentinel；不改 Ensure 行为 |
| **E5-2 适配器** | bun model/repo：User + Session（含 HashOTP 双读）；扩展端口；**分列 UPDATE**；`factors` 的 `FOR UPDATE`；集成测。**仍不 bind 生产 Wire**（或仅测试） | 不 000009；不加 `users.version` 列 |
| **E5-3 其余表** | Identity / Group / Membership / Bucket / File repo；`groups.total` SQL 增量；membership 状态 CAS；billing SUM；Groups/Storage use-case 改端口。仍写文档 **或** 仅测 bun | 生产仍可用文档直到 E5-4；File/Group proto 不加 version |
| **E5-4 cut** | Wire 切 bun；`000009`；同步 EnsureAll；Ensure skip；删黑名单与 version=0 分支；删 `document_repo.go`；§8 grep 清单 | 不拆 `businessSchema` / `RejectExternalDatabaseID` |
| **E5-5 contract**（可同 cut 若 grep 已零） | 删 `EnsureSystemCollections` 调用点与 spec；删 `documentSchema` sentinel 分叉；`DROP _perms` 已在 000009 则本 PR 只删 Go | 不碰用户 collection 引擎 |

每张 PR：`go vet ./...`、触及路径测试绿。改 Wire 后 `task wire-all`。禁止手改 `genproto/**`。

---

## 12. Alternatives Considered

### A. 000008 直接建最终名 `users`

否决。与文档表同名；旧进程会把静态表当成 `_id` 文档表写爆。

### B. 长期双写文档 + 静态表

否决。Account 热路径两处写，部分失败比停写窗口更糟。数据面方案已排除长期双写。

### C. 只表化 users/sessions，Groups/Storage 仍文档

否决。sentinel / `_perms` / Ensure 为七名单服务；半套无法退役旧号 1。

### D. 系统表留 `_perms` / `_tenant`

否决。D-3 与 K12：整表重写时一并去掉，否则三连迁移。

### E. 另起 golang-migrate 管系统表

否决。K-14 / 计划：载体继续 `projectschema`。

### F. List 改 AIP-160、丢掉 `queries[]`

否决。breaking，且撞 E-4 范围。本方案白名单编译现有 DSL。

### G. 先拆 sentinel 再搬表

否决。owner：表存在之后再退役寻址。先拆会使 Account 无物理容器。

### H. 七张系统表加 `version BIGINT` + proto If-Match

否决（S15）。今日系统集合无 OCC，调用方也没有 version 可传。Documents 式 If-Match 会变成 Account/Users/Storage 的新产品字段，超出「换表形态」。经济 `asset_holdings.version` 是账本聚合，不当样板。

### I. 只加内部 `version`、API 不暴露

否决。治不好 `factors` / `prefs` 整段 JSON 覆盖；仓储若整行 Update 才用得上，而整行 Update 本身禁止。JSON RMW 用 `FOR UPDATE`，计数用 SQL 增量，状态机用 CAS。

---

## 13. 与相邻方案

| 文档 | 关系 |
|---|---|
| `project-data-plane-schema.md` | 本方案兑现「与系统表化的衔接」。三层布局 / `businessSchema` / Apply 引擎原样用 |
| E-1 User 聚合 | 已合；本方案换适配器，不重做 `Register` / `GetByEmail` |
| E-4 Query AST | 不在范围；系统表 List 是窄编译器 |
| E-6 拆 DocumentDB | 接口拆可先于本方案；`EnsureSystemCollections` 仍留在 SchemaApplier 直到 E5-5 |
| D-9 `read:any` | 用户 collection 产品默认，**不要**写进本退役清单 |

---

## 14. 实施时禁止

- 在 E5-4 前删除 `ident` / `businessSchema` / sentinel 守卫 / `RejectExternalDatabaseID`。
- `SET search_path`。
- `CREATE INDEX CONCURRENTLY` 进 projectschema 文件。
- 给系统表加文档 `_version`、通用 `version` 列、或文档 outbox；给 User/Session/File/Group proto 加 If-Match（S15）。
- bun 仓储对系统行做无 `Column` 列表的整行 `Update`（会把 Get 到的陈旧列写回去）。
- 用旧 8 条 Appwrite 清单当完工（2–6 不属于 E-5）。
- 把 `Functions.Execute`、经济锁接口、`uow.Run` 顺手塞进本波。
