# P0 底座详细设计（Appwrite Go + PostgreSQL 迁移）

> [!WARNING] 已作废归档（ARCHIVED）
> 归档日期：2026-08-09
> 归档原因：P0 底座已建成并远超该阶段（动态文档层、认证、三套 API、Console 均已实现），本文档为历史设计，接口签名与目录结构与当前代码已有差异。
> 后续信息源：`AGENTS.md`（架构约定）、`docs/roadmap.md`（当前规划）、代码本身。

> 本文档在 `tech-decision.md` 已确认选型基础上，给出 P0 底座的详细设计。已通过 `docs/p0-design-review.md` 评审并确认关键决策。  
> 源码目标目录：`D:\\Codes\\qiulin\\Torchwood`

---

## 1. P0 范围与目标

P0 不是完整业务实现，而是搭建可支撑 P1 核心模块持续迭代的工程底座。目标：

1. 项目骨架与运行时（Lynx + Wire + config + logging/telemetry）。
2. 数据库访问基础设施：
   - bun 负责少量元数据静态表（projects、api_keys、document_* 元数据、console_admins）。
   - 动态文档数据库（Utopia-style Postgres adapter）核心接口与实现，**并用于承载 users/sessions/files 等系统资源**。
3. 认证与授权基础设施（JWT / session cookie / API key / AuthContext）。
4. API 传输层：grpc-gateway + gRPC + 独立 HTTP handler 共存。
5. 最小可运行业务闭环：Account sign-up / sign-in / me、Project 创建、Health。
6. 可复用的领域端口：Queue / Cache / BlobStore / FunctionExecutor。

---

## 2. 目录结构

```text
D:\\Codes\\qiulin\\Torchwood
├── cmd/
│   ├── server/
│   │   ├── main.go
│   │   ├── provides.go          # Wire provider set
│   │   └── wire_gen.go          # generated
│   └── worker/
│       ├── main.go
│       └── provides.go
├── configs/
│   └── config.yaml.template
├── db/
│   └── migrations/              # golang-migrate SQL
├── docs/
├── genproto/                    # generated protobuf
├── proto/
│   ├── shared/v1/
│   │   ├── authz.proto
│   │   ├── error.proto
│   │   └── common.proto
│   ├── client/v1/               # Client SDK API
│   ├── server/v1/               # Server SDK / 管理后台 API
│   └── console/v1/              # Console 前端专属 API
├── internal/
│   ├── api/
│   │   ├── clientgrpc/          # Client API gRPC implementations
│   │   ├── servergrpc/          # Server API gRPC implementations
│   │   ├── consolegrpc/         # Console API gRPC implementations
│   │   ├── gateway/             # grpc-gateway custom marshaler / error handler
│   │   └── http/                # 独立 HTTP handlers（文件、OAuth、WS）
│   ├── app/
│   │   ├── client/
│   │   ├── server/
│   │   ├── console/
│   │   └── shared/
│   ├── domain/
│   │   ├── shared/              # AuthContext、EventPublisher、Queue、BlobStore ports
│   │   ├── projects/
│   │   ├── users/
│   │   ├── sessions/
│   │   ├── teams/
│   │   └── databases/           # DocumentDatabase port
│   └── infra/
│       ├── bun/                 # bun models/repos for static metadata tables
│       ├── documentdb/          # Postgres dynamic adapter
│       ├── storage/             # S3/MinIO adapter
│       ├── functions/           # Docker executor adapter (port impl stub)
│       ├── queue/               # Redis queue adapter (port impl stub)
│       ├── cache/               # Redis cache adapter
│       └── server/              # Lynx components (gRPC, gateway, HTTP, metrics)
├── pkg/
│   ├── crud/                    # list/filter/order/pagination（参考 fleetwork，可改进）
│   ├── idgen/                   # ULID/Snowflake
│   ├── jwtparser/               # JWT claims
│   └── password/                # hash/verify utilities
├── buf.yaml
├── buf.gen.yaml
├── go.mod
└── Taskfile.yml
```

---

## 3. 模块路径

```go
module github.com/torchwoodcloud/torchwood
```

---

## 4. 配置与环境

### 4.1 配置 Schema

参考 fleetwork，使用 protobuf 定义配置，生成 `internal/pkg/config/config.pb.go`。关键配置项：

```yaml
server:
  grpc:
    addr: ":8088"
    timeout: "30s"
  http:
    addr: ":9099"
    timeout: "60s"
    cors:
      allowed_origins: ["*"]
      allowed_methods: ["GET", "POST", "PUT", "PATCH", "DELETE"]
  metrics:
    addr: ":9100"

security:
  jwt:
    secret: ""          # env: TORCHWOOD_SECURITY_JWT_SECRET
    access_ttl: "15m"
    refresh_ttl: "7d"
  api_key:
    header: "x-api-key"

data:
  database:
    source: ""          # env: TORCHWOOD_DATA_DATABASE_SOURCE (postgres)
  redis:
    addr: "localhost:6379"
    password: ""        # env: TORCHWOOD_DATA_REDIS_PASSWORD
    db: 0

storage:
  provider: "s3"        # or "minio" / "local"
  s3:
    endpoint: ""        # env
    region: "us-east-1"
    bucket: "Torchwood-storage"
    access_key: ""      # env
    secret_key: ""      # env
  local:
    path: "./data/files"

functions:
  executor: "docker"
  docker:
    host: "unix:///var/run/docker.sock"
    network: "Torchwood-functions"
    registry: "Torchwood-funcs"

telemetry:
  enabled: false
  otlp_endpoint: ""
```

### 4.2 环境变量

统一前缀 `TORCHWOOD_`。敏感项必须走环境变量：

- `TORCHWOOD_SECURITY_JWT_SECRET`
- `TORCHWOOD_DATA_DATABASE_SOURCE`
- `TORCHWOOD_DATA_REDIS_PASSWORD`
- `TORCHWOOD_STORAGE_S3_ACCESS_KEY`
- `TORCHWOOD_STORAGE_S3_SECRET_KEY`

---

## 5. 数据库设计

### 5.1 总体策略

| 数据类型 | 存储方式 | 工具 |
|---------|---------|------|
| 元数据静态表（projects、api_keys、document_*、console_admins） | 关系表，固定 schema | bun + golang-migrate |
| 系统资源表（users/sessions/files/buckets/teams/...） | 动态文档集合 + `_perms` | 原生 SQL adapter |
| 用户动态 Collection/Document | 动态文档集合 + `_perms` | 原生 SQL adapter |

动态表使用 PostgreSQL **schema-per-database** 做 database 隔离，以 `project.internal_id` 作为 `_tenant` 值做项目隔离。

### 5.2 元数据静态表

#### `projects`

```sql
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,          -- 公开 project id，如 "my-project"
    internal_id     BIGINT GENERATED BY DEFAULT AS IDENTITY UNIQUE,
    name            TEXT NOT NULL,
    description     TEXT,
    status          TEXT NOT NULL DEFAULT 'active',
    settings        JSONB DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_projects_name ON projects(name);
```

#### `api_keys`

```sql
CREATE TABLE api_keys (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    scopes      TEXT[] NOT NULL DEFAULT '{}',
    expire_at   TIMESTAMPTZ,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_api_keys_project ON api_keys(project_id);
```

#### `console_admins`（引导账户，仅用于 P0 登录后台；后续可迁移到 console project 的动态 users）

```sql
CREATE TABLE console_admins (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'owner',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

#### `document_databases`

```sql
CREATE TABLE document_databases (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(project_id, name)
);
```

#### `document_collections`

```sql
CREATE TABLE document_collections (
    id                 TEXT PRIMARY KEY,
    database_id        TEXT NOT NULL REFERENCES document_databases(id) ON DELETE CASCADE,
    project_id         TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    document_security  BOOLEAN NOT NULL DEFAULT TRUE,
    permissions        TEXT[] NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(database_id, name)
);
```

`permissions` 格式：`["read:any", "create:users", "update:keys"]`。

#### `document_attributes`

```sql
CREATE TABLE document_attributes (
    id            TEXT PRIMARY KEY,
    collection_id TEXT NOT NULL REFERENCES document_collections(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    type          TEXT NOT NULL,
    size          INT,
    required      BOOLEAN NOT NULL DEFAULT FALSE,
    array         BOOLEAN NOT NULL DEFAULT FALSE,
    default_value TEXT,
    options       JSONB DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(collection_id, key)
);
```

#### `document_indexes`

```sql
CREATE TABLE document_indexes (
    id            TEXT PRIMARY KEY,
    collection_id TEXT NOT NULL REFERENCES document_collections(id) ON DELETE CASCADE,
    type          TEXT NOT NULL,
    attributes    TEXT[] NOT NULL,
    orders        TEXT[] NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 5.3 bun 模型约定

- 模型文件：`internal/infra/bun/model/*.go`
- 统一使用 `bun.BaseModel` + `bun:"table:name,alias:x"`。
- JSONB 字段使用 `map[string]any`。
- 时间字段使用 `time.Time`。

---

## 6. 动态文档数据库（DocumentDatabase）设计

### 6.1 设计原则

- 对齐 Appwrite Utopia Postgres adapter：每个 collection 对应一张真实表 + 一个 schema 级 `_perms` 表。
- 使用 PostgreSQL **schema-per-database** 做 database 隔离。
- 使用 `_tenant` 列做项目隔离（schema 内默认 `DEFAULT <project_internal_id>`）。
- 使用原生 SQL 实现动态 DDL 和 CRUD；bun 仅提供连接池与事务。
- **系统资源（users、sessions、files、buckets、teams、memberships）统一作为预定义动态集合管理**，从而在 P0 就拥有完整的 `_perms` 权限模型。

### 6.2 核心端口

```go
package databases

type DocumentDatabase interface {
    // Database / schema
    CreateDatabase(ctx context.Context, projectID, id, name string) error
    DeleteDatabase(ctx context.Context, projectID, id string) error

    // Collection
    CreateCollection(ctx context.Context, projectID, databaseID, collectionID, name string, cfg CollectionConfig) error
    GetCollection(ctx context.Context, projectID, databaseID, collectionID string) (*Collection, error)
    ListCollections(ctx context.Context, projectID, databaseID string) ([]Collection, error)
    DeleteCollection(ctx context.Context, projectID, databaseID, collectionID string) error

    // Attribute / Index
    CreateAttribute(ctx context.Context, projectID, databaseID, collectionID string, attr Attribute) error
    CreateIndex(ctx context.Context, projectID, databaseID, collectionID string, idx Index) error

    // Document
    CreateDocument(ctx context.Context, projectID, databaseID, collectionID string, doc Document, perms []Permission) (Document, error)
    GetDocument(ctx context.Context, projectID, databaseID, collectionID, docID string) (*Document, error)
    UpdateDocument(ctx context.Context, projectID, databaseID, collectionID string, doc Document, perms []Permission) (Document, error)
    DeleteDocument(ctx context.Context, projectID, databaseID, collectionID, docID string) error
    ListDocuments(ctx context.Context, projectID, databaseID, collectionID string, q Query) (*DocumentList, error)
    CountDocuments(ctx context.Context, projectID, databaseID, collectionID string, q Query) (int64, error)
}
```

### 6.3 实体定义

```go
type Attribute struct {
    ID       string
    Key      string
    Type     string   // string, integer, float, boolean, datetime, email, url, json
    Size     int
    Required bool
    Default  any
    Array    bool
    Options  map[string]any
}

type Index struct {
    ID         string
    Type       string   // key, unique, fulltext
    Attributes []string
    Orders     []string
}

type Permission struct {
    Type       string // read, create, update, delete
    Role       string // any, users, user:{id}, keys, admin, team:{id}, ...
}

type Document struct {
    ID        string
    Tenant    int64
    Data      map[string]any
    CreatedAt time.Time
    UpdatedAt time.Time
    CreatedBy string
    UpdatedBy string
}

type Query struct {
    Filter   string // AIP-160 / Appwrite DSL 子集
    OrderBy  string
    PageSize int32
    PageToken string
}
```

### 6.4 表结构示例

当 project `my-project`（internal_id=1）下创建 database `db1` 和 collection `posts` 后：

```sql
CREATE SCHEMA IF NOT EXISTS "TORCHWOOD_1_db1";

CREATE TABLE "TORCHWOOD_1_db1"."posts" (
    _id          TEXT PRIMARY KEY,
    _tenant      BIGINT NOT NULL DEFAULT 1,
    _created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    _updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    _created_by  TEXT,
    _updated_by  TEXT,
    "title"      VARCHAR(256),
    "views"      BIGINT,
    "published"  BOOLEAN,
    "tags"       JSONB,
    UNIQUE (_id, _tenant)
);
```

每个 schema 下有一张统一权限表：

```sql
CREATE TABLE "TORCHWOOD_1_db1"."_perms" (
    _id         BIGSERIAL PRIMARY KEY,
    _tenant     BIGINT NOT NULL,
    _collection TEXT NOT NULL,
    _document   TEXT NOT NULL,
    _type       TEXT NOT NULL,   -- read / create / update / delete
    _permission TEXT NOT NULL,   -- role string
    _created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (_tenant, _collection, _document, _type, _permission)
);
CREATE INDEX idx_perms_lookup ON _perms(_tenant, _collection, _document, _type);
CREATE INDEX idx_perms_role ON _perms(_tenant, _collection, _type, _permission);
```

### 6.5 权限查询

读取文档时，先计算当前请求的角色集合 `roles`，再生成 `_perms` 过滤：

```sql
SELECT d.* FROM "TORCHWOOD_1_db1"."posts" d
WHERE d._tenant = 1
  AND EXISTS (
    SELECT 1 FROM "TORCHWOOD_1_db1"."_perms" p
    WHERE p._collection = 'posts'
      AND p._document = d._id
      AND p._type = 'read'
      AND p._permission = ANY(?)
  )
  AND <additional query filters>
```

写操作前同样做 `EXISTS` 校验；若调用者是 admin / platform admin / API key 拥有 `keys` 角色且集合权限允许，则放行。

### 6.6 系统集合默认权限

每个 project 创建时，自动初始化系统集合并写入默认权限：

| 集合 | read | create | update | delete |
|------|------|--------|--------|--------|
| users | any（注册开放时）/ keys / admin | any / keys / admin | user:{id} / keys / admin | user:{id} / keys / admin |
| sessions | user:{id} / keys / admin | user:{id} / keys / admin | user:{id} / keys / admin | user:{id} / keys / admin |
| files | user:{id} / keys / admin | user:{id} / keys / admin | user:{id} / keys / admin | user:{id} / keys / admin |
| buckets | keys / admin | keys / admin | keys / admin | keys / admin |

> `user:{id}` 中的 `{id}` 在文档创建后替换为文档 `_id`。

### 6.7 查询 DSL（P0 子集）

参考 Appwrite query 数组：

```json
["equal(\"status\",[\"active\"])", "greaterThan(\"views\",100)", "orderDesc(\"createdAt\")", "limit(25)"]
```

P0 先实现：

- 比较：`equal`、`notEqual`、`lessThan`、`lessThanEqual`、`greaterThan`、`greaterThanEqual`
- 字符串：`contains`、`startsWith`、`endsWith`
- 存在性：`isNull`、`isNotNull`
- 列表：`orderAsc`、`orderDesc`、`limit`、`offset`、`select`

`internal/domain/databases/query` 负责 parse；`internal/infra/documentdb` 负责 SQL 生成。

---

## 7. 认证与授权设计

### 7.1 认证方式

| 方式 | 场景 | 实现 |
|------|------|------|
| Session cookie | 浏览器 / Client SDK | `TORCHWOOD_session_<project_id>` 存 signed session id，服务端查 `sessions` 动态集合 |
| JWT access token | Client SDK / 浏览器 | `authorization: Bearer <jwt>` |
| JWT refresh token | 刷新 access token | `TORCHWOOD_refresh_<project_id>` httpOnly cookie 或 body |
| API key | 服务端 SDK / 自动化 | `x-api-key: <key>` |
| Console cookie | Console 管理员 | `TORCHWOOD_session_console` 存 signed console admin id |

### 7.2 Token 分类

- `access`：JWT，短期（默认 15m），claims 含 `uid`、`pid`、`sid`、`roles`。
- `refresh`：JWT 或 opaque token，长期（默认 7d），用于换发 access。
- `api_key`：opaque key，用于 Server API，无过期或按 `api_keys.expire_at`。
- `session`：浏览器 cookie，signed session id，服务端状态。

### 7.3 JWT Claims

```go
type AccessClaims struct {
    TID string `json:"tid"` // project id
    UID string `json:"uid"` // user id
    SID string `json:"sid"` // session id
    USN string `json:"usn"` // email/name
    AKD string `json:"akd"` // actor kind: user / admin / service
    RLS string `json:"rls"` // roles CSV
    EXP int64  `json:"exp"`
    IAT int64  `json:"iat"`
}
```

### 7.4 AuthContext / Principal

沿用 fleetwork `internal/domain/shared.Principal`：

```go
type Principal struct {
    ActorID         idgen.ID
    ActorKind       ActorKind          // end_user / admin / service
    CredentialType  CredentialType     // token / session / api_key
    IsPlatformAdmin bool
    TenantID        string             // 此处映射为 project_id
    ProjectID       string
    Roles           []string
    Permissions     []string           // scopes（API key 时）
    TraceIdentity   TraceIdentity
}
```

### 7.5 角色生成

当前请求的角色集合（按优先级）：

```text
any
guests              (未登录)
users               (已登录)
user:{id}
user:{id}/verified
keys                (API key)
api_key:{id}
admin / owner / developer
team:{teamId}
team:{teamId}/{role}
member:{membershipId}
label:{label}
```

### 7.6 授权策略

- **API key**：
  - gRPC 方法注解 `ACCESS_API_KEY`。
  - 解析 API key 后角色包含 `keys`；动态文档权限表需显式包含 `keys` 才能访问系统资源。
  - `Permissions` 字段存放该 key 的 scopes，用于更细粒度 Server API 鉴权（P1 完整实现，P0 仅要求 `keys` 能访问 Server API）。
- **用户请求**：
  - 从 JWT/session 加载 user id，生成 `user:{id}`、`users` 等角色。
  - 访问 users/sessions 等系统集合时，通过 `_perms` 过滤；app 层再补一层业务校验（如只能改自己的 user）。
- **admin / console**：
  - console admin 角色为 `owner`/`admin`/`developer`。
  - 对 Server API 的管理员访问，auth interceptor 识别 `TORCHWOOD_session_console` 后构造 admin principal；动态文档层对 admin 跳过 `_perms`。

### 7.7 Console 管理员登录与权限

#### 简化模型（P0）

1. 静态 `console_admins` 表预置初始管理员（安装时创建）。
2. Console 登录：`POST /v1/console/auth/sign-in`，email + password 校验 `console_admins`。
3. 成功后写入 cookie `TORCHWOOD_session_console` = signed admin id。
4. Console admin 默认是全局 owner，可调所有 project。
5. P1 将 console admin 迁移到 console project 的动态 users，并引入 organization/team 邀请机制。

### 7.8 Session Cookie

- cookie 名：`TORCHWOOD_session_<project_id>`。
- cookie 值：signed session id（HMAC-SHA256，密钥 = `security.jwt.secret`）。
- 服务端按 session id 查 `sessions` 动态集合，验证 `expire_at`。
- 刷新机制：通过 `TORCHWOOD_refresh_<project_id>` cookie 换发新的 session cookie。

---

## 8. API 传输层设计

### 8.1 Proto 组织

```text
proto/
├── shared/v1/
│   ├── authz.proto            # AccessLevel / MethodAuth / ServiceAuth
│   ├── error.proto            # 通用错误模型
│   └── common.proto           # Pagination, ListRequest
├── client/v1/                 # Client SDK API（终端用户，JWT/session）
│   ├── account.proto          # sign-up / sign-in / me / sign-out
│   ├── teams.proto            # 当前用户团队操作
│   ├── databases.proto        # 项目内数据库/文档/查询
│   ├── storage.proto          # bucket/file（调用侧）
│   └── functions.proto        # function/execution（调用侧）
├── server/v1/                 # Server SDK API（服务端，API key / admin）
│   ├── users.proto            # 用户 CRUD
│   ├── projects.proto         # 项目/API key 管理
│   ├── teams.proto            # 团队/成员管理
│   ├── databases.proto        # collection/attribute/index 管理
│   ├── storage.proto          # bucket 管理
│   ├── functions.proto        # function/deployment 管理
│   └── health.proto           # 服务端健康检查
└── console/v1/                # Console 前端专属
    ├── auth.proto
    ├── init.proto
    ├── variables.proto
    └── scopes.proto
```

### 8.2 Client / Server / Console 区分

| 类型 | 认证 | URL 前缀 | proto 包 | app 层 |
|------|------|---------|----------|--------|
| Client API | JWT/session | `/v1/account/*`, `/v1/teams/*`, `/v1/databases/*` ... | `proto/client/v1/*` | `app/client/*` |
| Server API | API key / console admin session | `/v1/server/*` | `proto/server/v1/*` | `app/server/*` |
| Console API | console admin session | `/v1/console/*` | `proto/console/v1/*` | `app/console/*` |

proto authz 注解：

- `ACCESS_PUBLIC`：sign-up、sign-in、health 等。
- `ACCESS_AUTHENTICATED`：Client API。
- `ACCESS_API_KEY`：Server API（API key 或 console admin session 均可）。
- Console API 方法用 `ACCESS_AUTHENTICATED` 并在 interceptor/app 层额外检查 admin 角色。

### 8.3 grpc-gateway 路径示例

```text
# Client API
POST   /v1/account/sign-up
POST   /v1/account/sign-in
GET    /v1/account/me
POST   /v1/account/sign-out

GET    /v1/databases/{database_id}/collections/{collection_id}/documents
POST   /v1/databases/{database_id}/collections/{collection_id}/documents

# Server API
GET    /v1/server/users
POST   /v1/server/users
GET    /v1/server/users/{user_id}
PATCH  /v1/server/users/{user_id}
DELETE /v1/server/users/{user_id}

GET    /v1/server/projects
POST   /v1/server/projects
GET    /v1/server/projects/{project_id}

# Console API
POST   /v1/console/auth/sign-in
GET    /v1/console/init

# Health
GET    /v1/health
```

### 8.4 独立 HTTP handler

```go
mux := http.NewServeMux()
mux.Handle("/v1/", grpcGatewayMux)
mux.Handle("/v1/storage/buckets/", fileHandler)
mux.Handle("/v1/account/sessions/oauth2/", oauthHandler)
mux.Handle("/v1/realtime", wsHandler)        // P1
mux.Handle("/healthz", healthHandler)
```

### 8.5 错误映射

沿用 fleetwork `internal/infra/server/errors.go` 风格，统一返回：

```json
{
  "error": {
    "type": "authentication_error",
    "code": "UNAUTHENTICATED",
    "message": "...",
    "error_id": "...",
    "error_code": "ERROR_CODE_INVALID_CREDENTIALS"
  }
}
```

---

## 9. 领域端口与基础设施适配器

### 9.1 共享端口

```go
package shared

type EventPublisher interface {
    Publish(ctx context.Context, topic string, payload any) error
}

type Queue interface {
    Enqueue(ctx context.Context, queue string, job any) error
}

type Cache interface {
    Get(ctx context.Context, key string) (string, error)
    Set(ctx context.Context, key string, value string, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
}
```

### 9.2 BlobStore

```go
package storage

type BlobStore interface {
    Put(ctx context.Context, bucket, key string, data io.Reader, size int64, contentType string) error
    Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, bucket, key string) error
    PresignedUploadURL(ctx context.Context, bucket, key string, expiry time.Duration) (url string, fields map[string]string, err error)
    PresignedDownloadURL(ctx context.Context, bucket, key string, expiry time.Duration) (url string, err error)
}
```

### 9.3 FunctionExecutor

```go
package functions

type FunctionExecutor interface {
    Build(ctx context.Context, deployment Deployment) (BuildResult, error)
    Execute(ctx context.Context, deployment Deployment, payload []byte) (ExecutionResult, error)
}
```

P0 仅实现端口和 Docker 适配器桩。

---

## 10. 多租户与项目隔离

- 所有请求必须落在某个 `project_id` 下。
- 平台级请求（创建项目、管理 console）使用特殊 `project_id = "console"`。
- 元数据表用 `project_id` 列隔离。
- 动态文档用 `_tenant` 列隔离；`tenant` 值 = `projects.internal_id`。
- 每个 project 创建时：
  1. 写入 `projects` 表。
  2. 创建默认 database schema（`TORCHWOOD_<internal_id>_default`）。
  3. 创建系统 collections：users、sessions、files、buckets（P0 至少 users/sessions）。

---

## 11. 关键流程

### 11.1 Sign Up

1. `POST /v1/account/sign-up` → grpc-gateway → `AccountService.SignUp`。
2. 读取 `X-Torchwood-Project` 或 cookie 中的 project_id。
3. 若 project 未初始化系统集合，调用 `CreateCollection` 创建 users/sessions。
4. app 层校验 email 唯一、密码强度。
5. domain/users：密码哈希（Argon2）。
6. documentdb：创建 user 文档，写入 `_perms`。
7. 创建 session 文档。
8. 生成 signed session cookie + JWT tokens。
9. 返回 token + user。

### 11.2 Sign In

1. `POST /v1/account/sign-in`。
2. app 层：按 email 在 users 集合查 user。
3. domain/users：校验密码。
4. 创建 session 文档。
5. 返回 signed session cookie + JWT tokens。

### 11.3 API Key Auth

1. `x-api-key` header → auth interceptor。
2. 查 `api_keys` 表，验证 hash 和 scopes。
3. 构造 Principal：
   - `ActorKind = service`
   - `CredentialType = api_key`
   - `TenantID = project_id`
   - `Roles = ["keys"]`
   - `Permissions = scopes`
4. gRPC method 注解 `ACCESS_API_KEY`。

---

## 12. Wire 注入结构

```go
var ProviderSet = wire.NewSet(
    boot.New,

    clientgrpc.ProviderSet,
    servergrpc.ProviderSet,
    consolegrpc.ProviderSet,
    gateway.ProviderSet,
    httpsvc.ProviderSet,

    clientapp.ProviderSet,
    serverapp.ProviderSet,
    consoleapp.ProviderSet,

    domain.ProviderSet,

    infra.ProviderSet,
    bunrepo.ProviderSet,
    documentdb.ProviderSet,
    storage.ProviderSet,
    cache.ProviderSet,
    queue.ProviderSet,
    functionexec.ProviderSet,

    server.NewGRPCServer,
    server.NewGRPCGatewayServer,
    server.NewHTTPServer,
    server.NewMetricsServer,
    server.NewHealthChecks,

    NewComponents,
)
```

---

## 13. 测试策略

| 层级 | 方式 |
|------|------|
| domain | 纯单元测试，mock ports |
| app | 用 mock repo 测试 use-case |
| infra/bunrepo | 集成测试：本地 docker compose postgres |
| infra/documentdb | 集成测试：真实 PostgreSQL，验证 DDL/DML/permissions |
| api/grpc | bufconn 或本地启动 gRPC server |
| api/http | 标准 `httptest` |

---

## 14. 里程碑（P0 拆分）

| 阶段 | 交付物 |
|------|--------|
| P0.1 | 项目骨架、Lynx 启动、config、logging、docker-compose |
| P0.2 | bun 元数据表 migrations + models + 基础 repo（projects/api_keys/document_*） |
| P0.3 | 动态文档数据库 adapter（schema/DDL/CRUD/permissions）+ 系统集合初始化 |
| P0.4 | 认证中间件 + Principal + JWT/API key/session cookie |
| P0.5 | grpc-gateway + 独立 HTTP 框架 + 错误映射 + CORS |
| P0.6 | Account sign-up / sign-in / me / sign-out 端到端 |
| P0.7 | Project CRUD + API key CRUD |
| P0.8 | Health、metrics、基础测试、文档 |

---

## 15. 已确认的关键决策

1. **系统资源使用动态文档 + `_perms`**：users/sessions/files/buckets 等作为预定义动态集合。
2. **schema-per-database**：每个 database 一个 PostgreSQL schema。
3. **P0 动态字段子集**：string / integer / float / boolean / datetime / email / url / json；索引 key / unique / fulltext。
4. **Session cookie 存 signed session id**，服务端查表验证。
5. **API key 不 bypass `_perms`**：通过 `keys` 角色和集合权限控制。
6. **P0 队列**：audit / deletes / outbox，保留扩展性。
7. **文件元数据静态表 deferred**：文件元数据走动态 documents，二进制走 S3/MinIO。
8. **Console admin 简化**：静态 `console_admins` 表预置全局 owner。
9. **可改进 `pkg/crud`**：fleetwork 实现作为参考，允许使用更优实现。
