# 动态文档 Schema 命名方案

> 状态：已实施。不保留向后兼容：本地/测试库以重建为准，不做 `ALTER SCHEMA RENAME`，不识别旧名 `TORCHWOOD_<internal_id>_*`。
> 相关代码：`internal/infra/documentdb/`、`internal/app/server/projects.go`、`internal/app/server/databases.go`、`internal/api/realtime/`、`internal/testutil/db.go`、`docs/developer/06-databases.md`。

---

## 1. 目标

把动态文档层 PostgreSQL schema 的物理名从实例局部的 `internal_id` 换成 **可随项目导出/导入的公开身份**：

```text
旧：TORCHWOOD_{projects.internal_id}_{database.id}    →  TORCHWOOD_42_app
新：tw_{project.id}_{database.id}                     →  tw_shop_app
```

约束：

1. schema 名必须是合法 PostgreSQL 标识符（ASCII、未引用也可解析）。
2. `project.id` / `database.id` 只含小写英文字母和数字，且创建后不可变。
3. `_` 在 schema 名里只出现两次：前缀后一次、project/database 之间一次；id 自身不含 `_`、`-`。
4. 全名 ≤ PostgreSQL `NAMEDATALEN-1` = **63 字节**。
5. `_tenant`（行内租户键）本方案 **不改**，仍用 `projects.internal_id`。

非目标见 §8。

---

## 2. 关键决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 物理名用公开 id，不用 name | `project.id` + `database.id` | id 创建后不可变；name 是展示字段，`UpdateProject` 可改 |
| schema 前缀 | **`tw_`**（小写、3 字节） | 不用 `TORCHWOOD_`：未引用标识符在 PG 中折成小写，大写前缀在 psql 里必须加引号；10 字节也挤占 id 长度。不用零前缀：会撞 `pg_*` / `information_schema`。`tw_` 保留命名空间。**运维扫描需精确匹配**（见 §2.2 LIKE 陷阱），`LIKE 'tw_%'` 仅作「本实例全部 Torchwood 动态 schema」的整体扫描，不得当「某项目」过滤器 |
| 字母表 | `^[a-z][a-z0-9]{0,27}$` | 小写字母开头 + 小写字母/数字；无连字符、无下划线、无大写 |
| 两侧长度 | 各 **28** | `tw_`(3)+28+`_`(1)+28 = **60**，低于 63，留 3 字节余量 |
| 项目 id 来源 | 创建时 **显式传入**，不再从 name slug | 展示名含空格/中文，slug 会丢信息或重新引入 `-` |
| 集合/属性/索引 id | **不改** | 它们是表名/列名，仍走现有 `^[a-zA-Z_][a-zA-Z0-9_]*$` |
| `_tenant` | 仍为 `internal_id` | 本方案只解决 schema 物理名可移植；行内租户重写另案 |
| 从 schema 反解析 database id | **废除** `schemaDatabaseID` | 调用链显式传 `databaseID`，避免按 `_` 拆名 |
| 存量兼容 | 不做 | 开发期允许重建；旧 migration 000008 对空库是空操作，不改写 |
| 落地方式 | **单 PR** | charset 与 schema 名必须同提交，否则测试项目 id 带 `-` 会建出非法 schema |

## 2.1 LIKE 陷阱（运维必须精确匹配）

本仓库存在**一段式**项目数据面 schema `tw_<project>`（见 `docs/design/project-data-plane-schema.md`）与**两段式**业务文档面 schema `tw_<project>_<database>`。项目 id 不含 `_`（`^[a-z][a-z0-9]{0,27}$`），因此：

| 模式 | `tw_shop` | `tw_shop_app` | `tw_shop_default` | `tw_shopx` | `tw_shopx_default` |
|------|-----------|---------------|-------------------|------------|---------------------|
| `= 'tw_shop'` | ✓ | | | | |
| `LIKE 'tw_shop%'` | ✓ | ✓ | ✓ | ✓ **误伤** | ✓ **误伤** |
| `LIKE 'tw_shop\_%' ESCAPE '\'` | | ✓ | ✓ | | |
| `LIKE 'tw_%'` | ✓ | ✓ | ✓ | ✓ | ✓ |

规则：

1. **项目数据面**：`nspname = ident.ProjectSchemaName(id)`（等值，禁止 LIKE）。
2. **某项目的业务库**：权威是 catalog（`document_databases`），不是 namespace。运维若必须扫 PG：`nspname LIKE ident.ProjectSchemaName(id) || '\_%' ESCAPE '\'`。
3. **DeleteProject / 运维脚本禁止** `LIKE 'tw_' || id || '%'`。
4. **Worker / 业务代码禁止扫 `pg_namespace`。** 业务库列表的权威是 catalog；项目列表的权威是 `public.projects`。
5. **`LIKE 'tw_%'` 仅允许作为「本实例全部 Torchwood 动态 schema」的运维整体扫描**（会同时命中一段式与两段式），不得当「某项目」过滤器。
6. **SQL `LIKE` 里 `_` 是单字符通配。** catalog / AIP-160 filter 禁止把未转义的 sentinel `ProjectDataPlaneID`（`"_"`）当 LIKE 操作数。

---

## 3. 标识符规范

### 3.1 Schema 资源 id（project.id / database.id）

新增唯一实现：`pkg/ident`（app / documentdb / realtime / testutil 共用，禁止再复制一份正则）。

```go
package ident

const (
    MaxSchemaResourceIDLen = 28
    SchemaPrefix           = "tw_"
    // ProjectDataPlaneID 是项目数据面（tw_<project>）的内部 database sentinel。
    // 非法 SchemaResourceID（charset 拒绝 "_"），仅供系统集合寻址。
    ProjectDataPlaneID = "_"
)

// 小写字母开头，后接 0–27 个小写字母或数字。合计最长 28。
var schemaResourceIDRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,27}$`)

// 一段式项目数据面 schema。
var projectSchemaNameRe = regexp.MustCompile(`^tw_[a-z][a-z0-9]{0,27}$`)

func ValidateSchemaResourceID(id string) error
func SchemaName(projectID, databaseID string) (string, error)       // 两段式 tw_<p>_<db>
func ProjectSchemaName(projectID string) (string, error)           // 一段式 tw_<p>
func IsTwoSegmentSchema(name string) bool                          // 供 DDL 分叉硬断言
```

合法 / 非法样例：

| id | 结果 |
|---|---|
| `default` | 合法（用户自选 id；2026-09 前亦是 CreateProject 第一业务库的缺省名，现缺省名为 `app`） |
| `shop` / `app` / `cms` | 合法 |
| `acmeprodshop2026` | 合法（16 字符） |
| `Shop` / `my-shop` / `my_shop` | 非法 |
| `1shop` / `""` / 29 字符 | 非法 |
| 中文、空格、`.` | 非法 |

错误一律 `InvalidArgument`，文案固定：

```text
id must match ^[a-z][a-z0-9]{0,27}$
```

`ValidateSchemaResourceID` 不负责「是否保留字」；保留语义放在用例层（§3.3）。

### 3.2 Schema 名

本仓库存在两类动态 schema：

| 类别 | 形式 | 示例 | 生成函数 |
|------|------|------|----------|
| 项目数据面（一段式） | `tw_{project.id}` | `tw_shop` | `ident.ProjectSchemaName(projectID)` |
| 业务文档面（两段式） | `tw_{project.id}_{database.id}` | `tw_shop_app` | `ident.SchemaName(projectID, databaseID)` |

两类都由 `ident` 包生成，校验失败返回 error，禁止 silently concat。使用处继续 `quoteIdent`（纵深防御，双引号 + `"` → `""`）。全小写，psql 里未引用也可写 `tw_shop_app.posts`。

整串校验（测试与 adapter 防御）：

```text
一段式：^tw_[a-z][a-z0-9]{0,27}$            （projectSchemaNameRe）
两段式：^tw_[a-z][a-z0-9]{0,27}_[a-z][a-z0-9]{0,27}$  （schemaNameRe）
```

长度：一段式 ≤ 31（`tw_`+28）；两段式 ≤ 60，均无需截断。

因为 id 不含 `_`（`^[a-z][a-z0-9]{0,27}$`），`tw_` 之后**恰好一道** `_`（一段式：前缀后；两段式：前缀后一道 + project/database 之间一道）。两段式 `tw_` 之后有且仅有两道 `_`，左右两侧分别是 project.id、database.id。一段式与两段式不相交（`IsTwoSegmentSchema` 对一段式返回 false），这是 LIKE 陷阱防护的结构性基础（§2.2）。即便如此，运行时也不从 schema 名反推 id（§5.2）。

业务库列表的权威是 catalog（`document_databases`），**禁止**用 `LIKE 'tw_'||id||'%'` 扫 `pg_namespace` 枚举某项目的业务库。

### 3.3 保留 id

| 资源 | id | 规则 |
|---|---|---|
| database（业务库） | `app` | CreateProject 自动建的缺省第一库（2026-09 前为 `default`，存量库不受影响），`CreateDatabase`/`DeleteDatabase` 与其它业务库同等（见 `docs/design/project-data-plane-schema.md` §4 / PR7，已落地解禁） |
| database（内部 sentinel） | `_`（`ident.ProjectDataPlaneID`） | 非法 SchemaResourceID（charset 拒绝）。仅供系统集合寻址项目数据面；对外 database_id 走 `RejectExternalDatabaseID` 拒绝；DDL 分叉 `businessSchema` 显式拒绝。见 project-data-plane-schema.md §3.1 |
| project | `default` | bootstrap 显式使用；其它创建路径允许同名失败于 PK，不额外保留 |

不以 PG 关键字（`public`、`pg_*`）为由拒绝用户 id：前缀 `tw_` 已隔开（`tw_pg_default` 不以 `pg_` 开头，也不会拼出 `information_schema`）。

### 3.4 明确不改的标识符

| 对象 | 规则 | 原因 |
|---|---|---|
| collection id / attribute key / index id | 现有 `identifierRe` / `safeNameRe`：`^[a-zA-Z_][a-zA-Z0-9_]*$` | 表名、列名；系统列过滤仍靠 `_` 前缀黑名单 |
| document id | 现有 `docIDRe` | 可含 `.` `:` `-` |
| `projects.name` / `document_databases.name` | 展示文案，不进物理名 | `UpdateProject` 可改 name，改了不动 schema |

Realtime channel 里 `databaseId` 改走 `ValidateSchemaResourceID`；`collectionId` 仍走旧 `identifierRe`。

---

## 4. `_tenant` 边界

`projects.internal_id`（`BIGINT GENERATED BY DEFAULT AS IDENTITY`）继续作为：

- 所有动态表、`_perms` 的 `_tenant` 值
- `resolveInternalID` 缓存键的查询结果
- 表 `DEFAULT` 与全部 `WHERE d._tenant = ?`

`schemaName` **不再消费** `internalID`。`CreateDatabase` / `DeleteDatabase` 等路径若只需拼 schema，用 `projectID` 即可；凡写入/过滤文档行的路径仍 `resolveInternalID`。

跨环境「导出项目 → 导入即用」仍要重写 `_tenant`（源环境 42，目标环境 3）。这不在本方案范围。本方案只保证两边去找 **同一个 schema 名**。

---

## 5. 代码改造

### 5.1 `pkg/ident`（新包）

职责仅两件事：校验 schema 资源 id、拼 schema 名。不依赖 domain / infra。

```go
func ValidateSchemaResourceID(id string) error {
    if id == "" || !schemaResourceIDRe.MatchString(id) {
        return status.Error(codes.InvalidArgument, "id must match ^[a-z][a-z0-9]{0,27}$")
    }
    return nil
}

func SchemaName(projectID, databaseID string) (string, error) {
    if err := ValidateSchemaResourceID(projectID); err != nil {
        return "", err
    }
    if err := ValidateSchemaResourceID(databaseID); err != nil {
        return "", err
    }
    return SchemaPrefix + projectID + "_" + databaseID, nil
}
```

是否在 `pkg/ident` 里直接返回 gRPC `status`：本仓库校验函数已有此先例（`app/server/databases.go` 的 `ValidateIdentifier`）。保持一致，避免每层再包一次。

单测覆盖 §3.1 表格 + `SchemaName` 拼接 / 拒绝。

### 5.2 documentdb

`internal/infra/documentdb/postgres.go`：

```go
// 删除
func schemaName(internalID int64, databaseID string) string
func schemaDatabaseID(schema string) string

// 所有 schema := schemaName(internalID, databaseID)
// 改为：
schema, err := ident.SchemaName(projectID, databaseID)
```

`CreateDatabase` / `DeleteDatabase` 仍先 `resolveInternalID` 确认项目存在（或改为显式 GetProject）；拼 schema 用 `projectID`。

`checkDocumentPermission` 今日用 `schemaDatabaseID(schema)` 回查 collection。改为 **调用方传入 `databaseID`**（该函数的调用点都已持有 `databaseID`）。签名增加参数，删除反解析。

`quoteIdent` / `tableName` / `permsTableName` / `safeNameRe` 不动。

`CREATE SCHEMA` / `DROP SCHEMA` 仍 `quoteIdent(schema)`。

### 5.3 项目创建：显式 id

**Proto** `proto/server/v1/projects.proto`：

```protobuf
message CreateProjectRequest {
  string name = 1;
  string description = 2;
  string id = 3;  // 必填；^[a-z][a-z0-9]{0,27}$
}
```

不改 field 1/2 的编号。用例层把 `id` 当必填：空或格式错 → `InvalidArgument`。执行 `task generate-proto`。

**用例** `CreateProjectCommand` 增加 `ID string`。删除这段 slug：

```go
id := strings.ToLower(strings.ReplaceAll(cmd.Name, " ", "-"))
```

改为：

```go
if err := ident.ValidateSchemaResourceID(cmd.ID); err != nil { return nil, err }
if cmd.Name == "" { return nil, status.Error(codes.InvalidArgument, "name is required") }
// 插入时 p.ID = cmd.ID
```

`UpdateProject` 仍然不能改 id（现有即如此）。

**Bootstrap** `internal/app/console/setup.go`：

```go
CreateProjectCommand{ID: cmd.ProjectID, Name: cmd.ProjectID, Description: "Bootstrap project"}
```

注释改为「bootstrap 使用注册时填写的 project id」，删除「从名称派生」的说法。`default` 仍命中新正则（普通业务库 id；系统集合已不寄居任何业务库，见 project-data-plane-schema.md）。

**gRPC handler / SDK / Console**：创建项目表单增加必填 `ID` 字段（placeholder `shop`，hint 写明规则）。`console/src/api/projects.ts` 的 `createProject` 增加 `id`。

### 5.4 数据库创建

`CreateDatabase` 把 `ValidateIdentifier(id)` 换成 `ident.ValidateSchemaResourceID(id)`。`default` 是普通业务库 id，允许创建与删除。

`name` 仍为展示字段，不校验 charset。

Realtime `parseDatabasesChannel`：`dbID` 改 `ident.ValidateSchemaResourceID`；`collID` 仍 `identifierRe`。

### 5.5 测试夹具

`internal/testutil.CreateTestProject` 今日生成 `test-%d`（含 `-`，且可能超过 28）。改为合法 id，例如：

```go
ID: fmt.Sprintf("t%x", time.Now().UnixNano()) // t + hex(nano) ≤ 16 字符量级
```

若集成测试手写 `my-project` / `proj-1` 等，一并改成 `myproject` / `proj1`。全仓 grep：

```text
test-%d
my-project
proj-1
project id must match
^[a-z0-9-]{1,64}$
schemaName(
TORCHWOOD_%d
TORCHWOOD_[0-9]
```

### 5.6 旧 migration

`db/migrations/000008_remove_keys_write_perms.up.sql` 用 `nspname ~ '^TORCHWOOD_[0-9]+_default$'` 扫存量系统库。

- **不改写** 已发布的 migration 文件。
- 全新库执行到 000008 时动态 schema 尚不存在，该段是空操作。
- 本方案之后系统库名为 `tw_<projectId>_default`，keys 写权限收窄已由运行时 `EnsureSystemCollections` / `cleanupKeysWritePerms` 承担。

本地有旧数据：`task down && task up`（或等价重建），不做在线 rename。

---

## 6. 调用链（改造后）

创建项目：

```text
CreateProject(id=shop, name="Shop")
  → INSERT projects (id=shop, internal_id=序列)
  → EnsureSystemCollections(shop, internal_id)
       CREATE SCHEMA tw_shop_default
       建系统集合表，列默认 / 行写入 _tenant = internal_id
```

创建业务库：

```text
CreateDatabase(project=shop, id=app, name="Application DB")
  → CREATE SCHEMA tw_shop_app
  → INSERT document_databases (project_id=shop, id=app, name=...)
```

读写文档：

```text
ListDocuments(project=shop, database=app, collection=posts)
  → schema = tw_shop_app
  → SELECT ... FROM tw_shop_app.posts d WHERE d._tenant = $internal_id
```

跨环境搬 schema 时，psql 里看到的名字与 API id 一致；`_tenant` 数字仍可能不同，导入方需另做映射（非本方案）。

---

## 7. 测试计划

### 7.1 `pkg/ident`

- 合法：`a`、`default`、`shop`、28 个 `a`
- 非法：空、`A`、`1a`、`my-shop`、`my_shop`、29 字符、`shop.app`、中文
- `SchemaName("shop","app") == "tw_shop_app"`
- 任一侧非法则 error 且返回空串

### 7.2 项目 / 数据库用例

> 下表前两条为本方案（schema 命名）当时的验收；后三条描述的「default 特殊禁令
> 与系统库寄居」已被 project-data-plane-schema.md PR2/PR7 取代（`default` 可建可删，
> 系统集合在 `tw_<project>`），现行断言以 `default_no_system_test.go` /
> `data_plane_schema_test.go` 为准。

- `CreateProject` 缺 id / 非法 id → `InvalidArgument`；合法 id 落库且 `GetProject` 一致
- 撞 id → 唯一约束映射为 `AlreadyExists`（沿用现有 duplicate 处理）
- `CreateDatabase` 非法 id、`default`、合法 `app`（**已过时**：`default` 今与普通库同等）
- 删除 `default` 仍拒绝（**已过时**：PR7 已解禁，只掉两段式业务 schema）
- bootstrap `SignUp` 使用调用方指定的 project id / database id；系统库 schema 为 `tw_<projectID>_default`（**已过时**：系统集合物理表在 `tw_<project>`）

### 7.3 documentdb 集成

- `CreateDatabase` 后 `to_regnamespace('tw_<pid>_app')` 非空
- `DeleteDatabase` 后该 namespace 为空
- 文档 CRUD 的 SQL 打到新 schema，`_tenant` 仍等于 `internal_id`
- 跨项目同 `database.id=app`：`tw_shop_app` 与 `tw_acme_app` 互不干扰

### 7.4 Realtime

- channel `databases.app.collections.posts` 仍解析成功
- `databases.my_app.collections.posts` 拒绝（下划线）

### 7.5 回归

`task test`。Console 新建项目页：填 id+name，创建后详情页 id 与列表一致（有浏览器工具则点选提交；否则依赖前端表单单测 / 手工清单）。

---

## 8. 非目标

- 不把 `_tenant` 改成 `project.id`，不做导入时的 tenant remap。
- 不改 collection / attribute / index 的 charset，不处理表名 63 字节上限。
- 不从 `name` 自动生成 id，不做「输入中文名转拼音」。
- 不提供 `ALTER SCHEMA RENAME` 迁移脚本。
- 不改 metadata 表（`projects` / `document_databases`）的 PK 结构。
- 不把 schema 做成 UUID 别名表。
- 不用全称 `torchwood_` 前缀（与 `TORCHWOOD_` 同为 10 字节，不省长度）。
- 不省略前缀（见 §2：`pg_*` / `information_schema`）。

---

## 9. 文档同步（同 PR）

| 文件 | 改什么 |
|---|---|
| `docs/developer/06-databases.md` §1.1 | 命名表改为 `tw_<projectID>_<databaseID>`，示例 `tw_shop_default` |
| `docs/developer/01-overview.md` | 「每个项目对应独立 schema」改为「每个 `(project.id, database.id)` 一个 schema」 |
| `docs/developer/09-api-guide.md` | CreateProject 必填 id 与正则 |
| `docs/roadmap.md` 运维 SQL 示例 | `tw_shop_app.posts` |
| `AGENTS.md` 数据库约定 | 一句指向 06-databases 新规则 |
| Console 新建项目 / 新建数据库 | 字段 hint |

OpenAPI 由 proto 注释生成，改 proto 后 `task generate-proto`。

---

## 10. 实施顺序（单 PR 内）

1. 加 `pkg/ident` 与单测（红：先写测试）。
2. Proto `CreateProjectRequest.id` + `task generate-proto`。
3. `CreateProject` / bootstrap / Console / SDK 显式 id。
4. `CreateDatabase` 与 realtime 改校验。
5. documentdb 换 `ident.SchemaName`，删除 `schemaName` / `schemaDatabaseID`，权限路径显式传 `databaseID`。
6. 修正 `CreateTestProject` 与全仓测试夹具。
7. 文档。
8. `task test`；Console 有改动则 `task console-build`。

`internal_id` 列、`resolveInternalID`、系统集合只读、`default` 库不可删，全部保持。
