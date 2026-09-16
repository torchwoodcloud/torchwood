# RuntimeVars：项目级运行时配置下发（多集合 · 可见性 · 版本回滚）

> 状态：**已实施（2026-09-17，五阶段子代理依次实现 + 终验全过：全量 task test exit 0、task build 四二进制、E2E 关键路径实走——server 面 CRUD/版本链/回滚快照语义/匿名拉取/etag 短路/private 与不存在错误体一致/无效凭证 401/CLI 反射实调）**。范围三问已拍板：纯类型化 KV 下发（无灰度引擎）/ client 匿名可读 / ETag 轮询传播（不做 realtime 推送）；多集合、公开·私有可见性、版本回滚经深度复查与对抗式审查定稿（ETag 掺 epoch 防同名重建碰撞 §3-D2、回滚快照全列防元数据丢失 §3-D11、可见性判定下推数据访问层 §2.4/§2.6）。
> 相关代码：`db/migrations/000011_runtime_vars.*`、`internal/domain/projects/runtime_var.go`（双 port）、`internal/infra/bun/{model,bunrepo}/runtime_var*.go`、`internal/app/{server,client}/runtimevars.go`、`internal/api/{servergrpc,clientgrpc}/runtime_vars.go`、`proto/{server,client}/v1/runtime_vars.proto`、`sdk/go/{server,client}/runtimevars*.go`、`sdk/typescript/src/{server,client}/runtimeVars.ts`、`console/src/routes/runtime-vars/pages.tsx`。
> 实施偏差记录（均可回溯）：① 写事务规范调用序实测修正为 LockHead → 变更 → 快照回读 → InsertVersion → BumpRevisionAndPrune（淘汰窗口必须计入新版本行，port 契约注释为准）；② server 面读动词 admin_roles 按仓库 ClassifyTier 三档惯例省略声明（= 全角色，语义等价）；③ gateway 对 int64 输出 JSON 字符串为全仓既有行为，TS SDK/Console 在各自数据层归一为 number；④ 未知 project_id 报 InvalidArgument 而非 NotFound（与"不向匿名确认存在性"自洽）。
> 相关：`proto/shared/v1/authz.proto`（scope 词表三处同步）、`internal/domain/auth/policy.go`（client 面 PUBLIC 白名单）、`db/migrations/000003_catalog_global.up.sql`（public 控制面表同构先例）、`proto/client/v1/databases.proto`（client 面匿名读先例：ListDocuments/GetDocument/CountDocuments）、`internal/infra/bun/bunrepo/apikey_repo.go`（update_guard 合规 Update 模板）。

---

## 1. 现状与问题

客户端应用需要"不改版本就能调整行为参数"（功能开关、文案、阈值、URL）。仓库现状：

- **零存量**：无 proto、无表、无 Console 页、无 roadmap 条目；最接近的既有物是 Functions 环境变量 CRUD（函数级 env，永不下发客户端），语义无关。
- **DIY 成本高**：用户拿 DocumentDB 集合 + realtime 自拼，要自己组装公开读权限模型、全量拉取与缓存语义、类型校验、尺寸限制、Console 管理面——每一项都是本方案的存在理由。
- **落地模式齐备**：project-scoped 双 proto（server+client）惯例、`internal/app/server|client` 用例层、`internal/infra/bun/bunrepo` 仓储、console 路由页、Go/TS 双 SDK、authz 注解 fail-closed 护栏（`AssertSemantic`）、per-IP 限流与审计拦截器全部现成，每条设计决策都有直接先例可抄。

三个易混邻居的边界：Functions env vars 是服务端函数环境变量；DocumentDB 集合是走 RLS 的用户业务数据；`internal/pkg/config` 是部署期服务端自身配置。RuntimeVars 是**项目级、类型化、读多写少的客户端行为参数**，三面（Console / Server API / Client SDK）访问。

## 2. 目标设计

### 2.1 总览与数据流

```
写路径：Console 页面 / CLI / Agent ──HTTP──> server 面 RuntimeVarsService（ACCESS_SERVER）
        └─ app/server 用例（校验+限额）─Tx─> runtime_var_sets / runtime_vars
                                              + runtime_var_versions（全量快照）
                                              + runtime_var_heads.revision+1
读路径：客户端 SDK ──匿名/登录 GET /v1/runtime-vars/{var_set_id}?project_id=&etag=──>
        client 面 GetRuntimeVars（方法级 ACCESS_PUBLIC，集合级可见性在用例层判定）
        └─ app/client 用例：visibility 判定 → 读 heads.revision → etag 命中 → unchanged；否则全量 vars
```

单一存储：public 控制面四张表，三面共用同一个 repo。

### 2.2 存储模型（迁移 `000011_runtime_vars`）

```sql
CREATE TABLE runtime_var_sets (
    project_id  TEXT NOT NULL REFERENCES public.projects(id) ON DELETE CASCADE,
    var_set_id  TEXT NOT NULL,             -- ^[a-z_][a-z0-9_]*$ ≤40，创建后不可改
    visibility  TEXT NOT NULL DEFAULT 'public',   -- 'public' | 'private'
    epoch       TEXT NOT NULL,             -- 创建时随机（8 字节 hex）：对外 etag 的防碰撞因子
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id)
);

CREATE TABLE runtime_vars (
    project_id  TEXT NOT NULL,
    var_set_id  TEXT NOT NULL,
    key         TEXT NOT NULL,             -- ^[a-z_][a-z0-9_]*$ ≤64
    value_type  TEXT NOT NULL,             -- 'string'|'integer'|'float'|'boolean'|'json'
    value       JSONB NOT NULL,            -- 标准化 JSON 值（标量即 JSON 标量）
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id, key),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);

CREATE TABLE runtime_var_heads (
    project_id TEXT NOT NULL,
    var_set_id TEXT NOT NULL,
    revision   BIGINT NOT NULL DEFAULT 0,  -- 集合级严格单调；= 版本链寻址号
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);

CREATE TABLE runtime_var_versions (
    project_id TEXT NOT NULL,
    var_set_id TEXT NOT NULL,
    revision   BIGINT NOT NULL,            -- = 快照时点的 heads.revision，一鱼两吃
    vars       JSONB NOT NULL,             -- 全量快照 {key: {t, v, d, c}}（复查修正：全列，见 D11）
    action     TEXT NOT NULL,              -- 'create'|'update'|'delete'|'rollback'
    summary    TEXT NOT NULL DEFAULT '',   -- 如 "update: a, b" / "rollback to 42"
    actor      TEXT NOT NULL,              -- admin:<id> / apikey:<id> / function:<id>
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, var_set_id, revision),
    FOREIGN KEY (project_id, var_set_id)
        REFERENCES runtime_var_sets (project_id, var_set_id) ON DELETE CASCADE
);
```

- FK 链两级 CASCADE 模仿 `catalog_collections → catalog_databases`；项目删除自动传导，删除集合 = 主动销毁其 vars + heads + 全部历史（Console 二次确认警示）。
- 对外 ETag = **不透明 token `"{epoch}:{revision}"`**（如 `a1b2c3d4:42`）；`revision` 单独作为版本链与回滚寻址的内部序号。
- heads 行不存在视同 revision 0；集合创建即建 heads 行（CreateVarSet 同事务）。
- down 迁移 = DROP 四张表。

### 2.3 proto 契约

**server 面** `proto/server/v1/runtime_vars.proto`（抄 `oauth_providers.proto` 骨架 + `assets.proto` 的 buf.validate 用法），服务级 `default_access: ACCESS_SERVER`，三组 13 个方法：

```
集合：POST   /v1/server/runtime-var-sets                          CreateVarSet
      GET    /v1/server/runtime-var-sets                          ListVarSets
      GET    /v1/server/runtime-var-sets/{var_set_id}             GetVarSet
      PATCH  /v1/server/runtime-var-sets/{var_set_id}             UpdateVarSet（optional visibility/description）
      DELETE /v1/server/runtime-var-sets/{var_set_id}             DeleteVarSet
变量：POST   /v1/server/runtime-var-sets/{var_set_id}/vars        CreateRuntimeVar
      GET    /v1/server/runtime-var-sets/{var_set_id}/vars        ListRuntimeVars
      GET    /v1/server/runtime-var-sets/{var_set_id}/vars/{key}  GetRuntimeVar
      PATCH  /v1/server/runtime-var-sets/{var_set_id}/vars/{key}  UpdateRuntimeVar
      DELETE /v1/server/runtime-var-sets/{var_set_id}/vars/{key}  DeleteRuntimeVar
版本：GET    /v1/server/runtime-var-sets/{var_set_id}/versions        ListRuntimeVarVersions（仅元数据）
      GET    /v1/server/runtime-var-sets/{var_set_id}/versions/{revision}  GetRuntimeVarVersion（全文）
      POST   /v1/server/runtime-var-sets/{var_set_id}/versions:rollback  RollbackRuntimeVar
```

鉴权档位：读动词（Get/List×3）`admin_roles: [VIEWER, MEMBER, ADMIN, OWNER]` + `api_key_scope {RUNTIME_VARS, READ}`；写动词（Create/Update/Delete/Rollback）`[ADMIN, OWNER]` + `{RUNTIME_VARS, WRITE}`。scope 枚举新值 `SCOPE_RESOURCE_RUNTIME_VARS = 17`（当前最大 16）。

核心 message：

```proto
message VarSet {
  string var_set_id = 1;             // buf.validate: pattern ^[a-z_][a-z0-9_]*$, max_len 40
  Visibility visibility = 2;         // VISIBILITY_PUBLIC / VISIBILITY_PRIVATE
  string description = 3;            // max_len 256
  string etag = 4;                   // "{epoch}:{revision}"，客户端直接透传用
  int64 revision = 5;                // 当前版本号
  int32 var_count = 6;
  int64 total_bytes = 7;             // 集合当前总值尺寸（Console footprint 展示）
  google.protobuf.Timestamp created_at = 8;
  google.protobuf.Timestamp updated_at = 9;
}

message RuntimeVar {
  string var_set_id = 1;
  string key = 2;                    // pattern ^[a-z_][a-z0-9_]*$, max_len 64
  RuntimeVarValue value = 3;
  string description = 4;            // max_len 256
  google.protobuf.Timestamp created_at = 5;
  google.protobuf.Timestamp updated_at = 6;
}

message RuntimeVarValue {
  oneof kind {
    string string_value = 1;
    int64 integer_value = 2;         // int64: gte -9007199254740991, lte 9007199254740991（HTTP JSON 精度 / TS 安全整数）
    double float_value = 3;
    bool bool_value = 4;
    string json_value = 5;           // 合法 JSON 文本；app 层校验解析 + 嵌套深度 ≤100
  }
}

message UpdateRuntimeVarRequest {
  string var_set_id = 1;
  string key = 2;
  RuntimeVarValue value = 3;         // 必填：完整新值，类型可随本次变更
  optional string description = 4;   // proto3 optional：未设置 = 不修改
}

message RuntimeVarVersion {
  int64 revision = 1;
  VersionAction action = 2;          // CREATE / UPDATE / DELETE / ROLLBACK
  string summary = 3;
  string actor = 4;
  google.protobuf.Timestamp created_at = 5;
}

message RollbackRuntimeVarRequest {
  string var_set_id = 1;
  int64 target_revision = 2;         // 等于当前版本 → InvalidArgument（D12）
}
```

**client 面** `proto/client/v1/runtime_vars.proto`（服务级 `default_access: ACCESS_END_USER`），唯一方法：

```proto
service RuntimeVarsService {
  option (torchwood.shared.v1.service_auth) = { default_access: ACCESS_END_USER };

  rpc GetRuntimeVars(GetRuntimeVarsRequest) returns (GetRuntimeVarsResponse) {
    option (google.api.http) = { get: "/v1/runtime-vars/{var_set_id}" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PUBLIC };
    option (grpc.gateway.protoc_gen_openapiv2.options.openapiv2_operation) = {
      security: {}  extensions: { key: "x-torchwood-access"  value: { string_value: "public" } }
    };
  }
}

message GetRuntimeVarsRequest {
  string var_set_id = 1;   // 路径参数
  string project_id = 2;   // 匿名唯一可靠寻址（resolveProjectID 链第一级）
  string etag = 3;         // 可选：上次响应的 etag；命中则短路返回 unchanged
}
message GetRuntimeVarsResponse {
  map<string, RuntimeVarValue> vars = 1;  // 全量快照；unchanged 时为空
  string etag = 2;
  bool unchanged = 3;
}
```

`RuntimeVarValue` 在 client proto 内独立定义同形（client/server 资源类型互不 import 是仓库现状），由 swagger 测试与 SDK 类型测试覆盖一致性。**不用 HTTP 304**：grpc-gateway 无自动 ETag 支持，`unchanged` 字段在 gRPC/HTTP 两面语义等价。

**鉴权词表三处同步**（漏任何一处 `AssertSemantic` 启动失败，护栏自验证）：
1. `proto/shared/v1/authz.proto`：`SCOPE_RESOURCE_RUNTIME_VARS = 17`；
2. `internal/domain/auth/policy.go`：`ScopeRuntimeVars` 常量进 `AllScopeResources` + `clientPublicMethodWhitelist` 登记 `/torchwood.client.v1.RuntimeVarsService/GetRuntimeVars`；
3. `cmd/server/internal/runtime/authz_policy.go`：`protoScopeResource()` 映射。

### 2.4 可见性判定（client 拉取）

方法级保持 `ACCESS_PUBLIC`（公开集匿名可读语义成立），集合级可见性判定**下推到数据访问层**（对抗审查修正：初稿放在 client app 用例层的一行 if，与 `ListDocuments` 的"数据层过滤同构"声明名不副实——RLS 是任何查询路径都绕不过的存储层强制，用例层判定只覆盖那一个用例，未来新增读路径或复用 repo 即旁路 private。修复：repo port 拆分，见 §2.6——client 用例注入的 `RuntimeVarPublicRead` 接口上**不存在未过滤的读**，private 拒绝做进 repo 实现，旁路在类型层消灭）。可见性是数据级属性、不是凭证族差异，这一点与 `ListDocuments`（PUBLIC）+ 集合级 `read:any` 的分层一致；app 用例层只负责凭证布尔（是否有有效 Principal、项目是否匹配）：

| 请求态 | public 集 | private 集 |
|---|---|---|
| 无 Principal（匿名） | 200 | **NotFound**（与"集合不存在"错误体一致，不向匿名探测者确认私有集合存在） |
| 有效 Principal（end_user / admin / apikey / 函数执行） | 200 | 200 |
| Principal.ProjectID ≠ "" 且 ≠ 请求项目 | PermissionDenied | PermissionDenied |

**边界声明（复查补充）**：匿名会话用户（`CreateAnonymousSession`）持有合法 end_user Principal，**可读 private 集**——"私有 = 项目用户可见、路人不可见"语义下成立；"仅实名/邮箱验证用户"是灰度引擎的条件维度，本期不做。函数运行时读配置走同一路径：执行凭证在 PUBLIC 方法上被认证注入 Principal，函数天然可读自己项目的 private 集，零新通道。

**读一致性（对抗审查补充）**：client 读路径的 revision 读取与 vars 读取若为两条独立 SELECT，中间写入提交会产生"etag 滞后于内容"的瞬时错配——方向安全（内容新于 etag，下一轮轮询必刷新自愈；反向不可能：revision bump 与 vars 写同事务提交，heads 读到新值意味着写已提交，后续 vars 读必见新内容），但修复几乎免费：两读放同一 Read Committed 事务（或单条 JOIN heads/vars）消除窗口，实现取后者。

### 2.5 写路径与回滚实现

所有写操作（var 增删改、回滚）单事务，首步 `SELECT ... FOR UPDATE` 锁 `runtime_var_heads` 行，串行化同集合并发写：

```
常规 var 写：
  锁 heads → 变更 vars 行（Insert / Update .Set 白名单 / Delete）
  → revision+1（UPDATE heads）
  → 快照 = 事务内 SELECT 回读该集合全部 var 行（以 DB 为准），序列化为 {key:{t,v,d,c}}
  → INSERT 版本行（action/summary/actor/快照）
  → 淘汰窗口外最老版本（保留最近 50 版）

回滚 RollbackRuntimeVar：
  锁 heads → 读目标版本快照（不存在 → NotFound "target revision pruned"）
  → DELETE 该集合全部 vars → 按快照重插
    （type/value/description/created_at 取快照值；updated_at = NOW()——回滚是一次新的写）
  → revision+1 → INSERT 新版本行（action='rollback', summary="rollback to {target}",
    vars = 目标快照原样）→ 淘汰
  不做限额校验：快照来源时点必然合规（写路径已拦），回滚不可能超限
```

- `summary` 生成：var 写记变更 key 列表（"update: a, b" / "delete: c"）；回滚记 "rollback to {n}"。
- `actor` 从 Principal 取：`admin:<id>` / `apikey:<id>` / `function:<id>`（handler 填入 Command）。
- 集合元数据（visibility/description）变更**不 bump revision、不入版本链**（只进 audit_logs）——见 D10。
- 快照从写后 DB 状态回读构造，不以内存态为准（避免序列化不一致）。

### 2.6 各层落点

| 层 | 位置 | 内容 |
|---|---|---|
| domain port | `internal/domain/projects/runtime_var.go` | `VarSet`/`RuntimeVar` 实体 + **两个窄 port**（对抗审查修正，SettingsWriter 同模式）：`RuntimeVarRepository`（server 面：集合 CRUD + `GetHead`/`LockHead`（FOR UPDATE；**0 行 = 集合不存在 → NotFound**，防 DeleteVarSet 竞态下误当成功）+ var CRUD + `Footprint(projectID, varSetID) (count, bytes)` + `ReplaceVars`（回滚整替）+ 版本写/读/淘汰）与 `RuntimeVarPublicRead`（client 面专用：`GetVisibleVars(ctx, projectID, varSetID, principalAllowed bool) (vars, revision, err)`——repo 实现 JOIN sets 过滤，private 且非 allowed → nil, nil 与"集合不存在"合并；**接口上不存在未过滤读**，旁路在类型层消灭，wire 误接线为编译错误）；Update 注释明确 `.Set` 白名单（value/value_type/description/updated_at） |
| infra | `internal/infra/bun/model/runtime_var.go` + `bunrepo/runtime_var_repo.go` | value JSONB 用 `string` 承载 JSON 文本（catalog 三列同模式）；Update 走 apikey 式 `cols map` + 白名单校验（过 update_guard AST 扫描）；`Footprint` = `SELECT COUNT(*), COALESCE(SUM(octet_length(value::text)),0)` 加 var_set 维度；`RuntimeVarPublicRead` 与全量 repo 同文件双实现，可见性过滤 = JOIN runtime_var_sets 单条 SQL |
| app/server | `internal/app/server/runtimevars.go` | key/var_set_id 格式、类型一致、JSON 合法 + 深度 ≤100、限额（D13）；写包 `db.RunInTx`；错误直接 `status.Error`（P3-18 约定） |
| app/client | `internal/app/client/runtimevars.go` | loadProject → 凭证布尔（有 Principal 且项目匹配）→ 注入 `RuntimeVarPublicRead` 取可见快照（过滤在 port 实现内）→ etag 命中短路 / 全量返回；**不注入全量 port** |
| handler | `internal/api/servergrpc/runtime_vars.go` + `clientgrpc/runtime_vars.go` | 透传 app 错误；server 面 `projectIDFromContext` + `contexts.WithAuditResource(ctx, "runtime_var_sets/<id>")`；client 面实现 resolveProjectID 三级回落（请求字段 → `X-Torchwood-Project` → Principal，与 `clientgrpc/databases.go` 同链） |
| 装配 | `grpc.go`（Register×2 + `authzFileDescriptors` 两个新 File）→ `grpc_gateway.go`（两个 register）→ `provides.go`×3 → `task wire:all` | |
| SDK | `sdk/go/server/runtimevars.go`（13 方法）+ `sdk/go/client/runtime_vars.go`（GetRuntimeVars）+ TS 对应两文件 + `sdk/go/server/client.go`/`scopes.go` + TS index/torchwood 登记 | CLI 零登记（InvokeJSON 反射自动覆盖） |
| Console | `console/src/routes/runtime-vars/pages.tsx`（单文件两页）+ `console/src/api/runtimeVars.ts` + `App.tsx`/`Layout.tsx` | 见 §2.7 |

### 2.7 Console

- `VarSetsListPage`（`/console/runtime-vars`）：集合表格——visibility badge、var_count、当前 revision、**footprint（总值/1MB 上限）**；空态引导"创建第一个变量集"。
- `VarSetDetailPage`（`/console/runtime-vars/:varSetId`）三区块：
  - 变量编辑表格：key/类型 badge/值预览（截断）/updated_at + Dialog 编辑（key 编辑时禁改、类型 select 切换值输入控件、json 用 textarea + 前端 JSON 校验）；
  - 版本历史：元数据列表（revision/action/summary/actor/时间）+ 查看全文 + 两版本对照 diff（前端拉两版全文自行对比，服务端不做 diff API）+ 回滚按钮带确认；
  - 集合设置：visibility/description 编辑（**双向切换均强确认**：→public 明示"整集对任何持 project_id 者公开"；→private 明示"匿名客户端将立即 404"）+ 删除集合（警示含全部历史销毁）。
- 页面顶部固定提示："变量对持有 project_id 的客户端公开（public 集）或登录用户（private 集），禁止存放机密"。
- axios 直连 `/server/runtime-var-sets`（Console 不用 SDK 包，现状约定）。

## 3. 关键决策及理由

**D1 存储放 public 控制面，不放项目数据面。** runtimeVars 是项目级配置元数据，三个访问者都不走文档 RLS；放数据面意味着每项目建表（projectschema 清单 + DDL 对账）还要为匿名读设计 RLS 豁免通道。public 方案一张表、FK CASCADE 跟随项目删除（与 `catalog_databases` 完全同构，同样不进 `DeleteProjectControlPlaneRows`）。**否掉**：buckets/files 式项目数据面系统静态表。

**D2 ETag = `"{epoch}:{revision}"` 不透明 token（复查修正）。** v1 曾定 etag = revision 十进制字符串：集合删除后同名重建 revision 归零，持久化了旧 etag 的离线客户端在重建集合 revision 追平旧值时被误判 unchanged，永久持有陈旧缓存。epoch（创建时随机）使重建必然全量返回。revision 仍单独作版本链寻址，职责分离。**否掉**：裸 revision（同名重建碰撞）；hash(全行) etag（编码顺序问题）。

**D3 值模型 typed oneof，存储 JSONB 单列 + value_type 锚点列。** oneof 让 SDK 能给 typed API 且"类型创建时锁定、变更走显式 Update"；JSONB 单列让五类型一条 roundtrip 通路。integer 限 ±2^53−1 是硬约束：gateway 输出 JSON number，TS `number` 超精度静默损坏。json_value 限嵌套 ≤100 层（64KB 的 `[[[[…` 可构造 ~3 万层，Go encoding/json 万层栈保护能拦但错误不可读，显式限制给出友好错误）。**否掉**：全 string 值（客户端自行解析）；`google.protobuf.Struct`（int/float 不可区分，SDK 生成物笨重）。

**D4 client 拉取是按集合的全量快照，无单 key 读、无分页、无跨集合聚合。** 客户端启动要的就是整集；分页会在拉取中途写入时撕裂快照；跨集合聚合需要复合 etag（膨胀）且集合名按惯例硬编码在客户端。删除语义靠客户端"整 map 替换"自然生效。

**D5 server 管理 Create/Update 分立，不做 Upsert。** Upsert 的"静默覆盖"是 Console 误提交事故源；Agent 幂等初始化用 List 后判断。限额（D13）属跨字段业务规则，按仓库约定放 app 用例层而非 protovalidate。

**D6 鉴权档位：读四档 admin 角色 + scope READ，写 ADMIN/OWNER + scope WRITE，不设 ADMIN 第三档。** ScopeOp admin 档是"热路径/控制面凭证分离"的 opt-in（leaderboards 先例）；runtimeVars 无热路径凭证，read/write 两档与 assets/functions 对齐。viewer 可读不可写。

**D7 防滥用不新增基础设施。** per-IP 300/min（现有 RateLimitInterceptor）自动覆盖匿名端点；unchanged 响应极小；1MB 上限封顶响应体。匿名可读的暴露面与既有 PUBLIC `ListDocuments` 同级。残余风险靠 Console 提示 + "不存秘密"口径（与 Firebase Remote Config 同口径）。

**D8 审计零新代码。** `UnaryAuditMiddleware` 按 FullMethod 自动落库（写动词默认落、读方法默认豁免），handler 只需 `WithAuditResource` 标注。

**D9 集合是显式实体，不建缺省集合。** `CreateVarSet` 显式创建，Console 空态引导。**否掉**"缺省建 default 集"——隐式资源（对比：`app` 缺省库有明确产品动机，这里没有），20 集合上限下自建无摩擦。

**D10 版本号复用 heads.revision；版本链语义收窄为"值快照序列"。** revision 既是版本号又参与 etag，回滚后 ETag 必变，客户端缓存与版本链永远一致。**var 增删改/回滚 bump；集合元数据（visibility/description）变更不 bump、不入版本链**（只进 audit_logs）——可见性对客户端是拉取瞬间判定的数据属性，不属于值快照；混入会让"同 etag 但可见性变了"成为不可检测状态。

**D11 版本存全量快照（全列），不存 diff；保留 50 版/集合（含复查修正）。** 全量快照使回滚 = 整替，原子且无重放逻辑（diff 回放要处理中途删除/重建边界）。**快照结构 `{key:{t,v,d,c}}` 为全列四元组**：v2 初稿只存 type/value，回滚重插会把 description 抹空、created_at 重置——元数据静默丢失违背"回滚到版本 N"直觉；重插时 created_at/description 取快照值、updated_at 设回滚时刻。膨胀由窗口封顶：最坏 20 集 × 50 版 × 1MB ≈ 1GB/项目，实际配置集 KB 级 + JSONB TOAST 压缩。**否掉**：diff 版本链（回放复杂）；无限保留。

**D12 `target_revision` 等于当前版本 → InvalidArgument。** 回滚到自身是语义错误而非幂等操作（无谓 burn 版本号），真幂等诉求不存在。

**D13 限额集合级**：集合 ≤20/项目、vars ≤500/集合、单值 ≤64KB（JSON 字节）、集合总值 ≤1MB、版本保留 ≤50/集合；key `^[a-z_][a-z0-9_]*$` ≤64、var_set_id 同正则 ≤40、description ≤256。

**D14 不提供 client 面"列出项目全部集合"端点。** 集合名硬编码在客户端；向匿名暴露集合目录是纯信息泄露面。目录查询走 server 面 ListVarSets。

**D15 visibility 双向切换都是破坏性 Console 强确认（复查补充）。** →public：一键把整个私有集暴露给任何持 project_id 者（泄密）；→private：全部匿名客户端瞬间 404，App 回退内置默认值（SDK 文档口径：收到 404 保留 last-known-good 缓存）。服务端不做二次保护（管理员意图即授权），审计自动记录。

## 4. 分步实施计划

每步是可交付中间态，验证通过才进下一步：

1. **存储层**：迁移 000011（四表）+ bun model + repo + repo 集成测试。验证：`task db:migrate && go test ./internal/infra/bun/...`（五类型 roundtrip、版本链、回滚、CASCADE、footprint）。
2. **proto 与词表**：两个 proto + authz 三处登记 + `task generate:proto`。验证：生成物出现 swagger json，`buf lint` 干净。
3. **服务面**：domain port + 两个 app 用例 + 两个 handler + 装配登记全集 + `task wire:all`。验证：`task dev:server` 启动过（AssertSemantic + authzFileDescriptors 护栏即验收）；`curl` 走通集合 CRUD + var CRUD + 版本列表/回滚 + 匿名 `GET /v1/runtime-vars/{set}`（无凭证 200；同 etag 二次请求 unchanged:true；写入后 etag 变化；private 集匿名 404、与不存在集合错误体一致）。
4. **SDK**：Go/TS 四个服务文件 + scopes + 登记点。验证：`go test ./sdk/...`（invoke_test 反射覆盖自动纳管）+ TS 构建。
5. **Console**：两页 + API 层 + 路由/nav。验证：`task console:build && task build`，页面走通建集/建改删 var/版本查看/回滚/visibility 切换确认。
6. **收口**：`task gen:authz-matrix`（CI 字节锁定）+ `task test` + `task build` 全绿。

## 5. 风险与对策

| 风险 | 对策 |
|---|---|
| 匿名端点读走 public 集全部值（机密泄露） | Console 显著提示 + "不存秘密"口径；限额封顶；接受残余风险（与 ListDocuments PUBLIC、Firebase RC 同级） |
| PRIVATE→PUBLIC 误操作整集泄密 | Console 强确认 + 审计（D15） |
| PUBLIC→PRIVATE 匿名客户端瞬间 404 | Console 强确认 + SDK 文档"保留 last-known-good"口径 |
| 同名删除重建 etag 碰撞 | epoch 因子（D2），重建必然全量返回 |
| 版本快照膨胀 | 50 版窗口 + 1MB 集合上限同事务淘汰；Console footprint 展示；JSONB TOAST 压缩 |
| 同集合并发写/回滚竞争 | 写事务全部先锁 heads 行 FOR UPDATE，天然串行化 |
| 回滚目标已被淘汰 | NotFound + 明确错误 "target revision pruned"（窗口外属预期） |
| 匿名探测 var_set 名 | private 与不存在同答 NotFound，错误体一致不可区分 |
| 轮询模式的常态出口流量（runtime-vars 是唯一鼓励客户端周期轮询的 PUBLIC 端点） | unchanged 短路为主防线（常态 99% 轮询为几十字节）；SDK 文档口径建议 TTL ≥ 60s；1MB 上限封顶单响应 |
| int64 精度在 HTTP JSON 静默损坏 | protovalidate ±2^53−1 写入拦截 |
| 深嵌套 JSON 打爆解析栈 | 深度 ≤100 显式校验，错误友好 |
| heads 与 vars/版本写入跨语句 | 用例层 `db.RunInTx` 包住全部写语句，repo 经 `Conn(ctx)` 感知事务；快照从写后 DB 回读 |
| 迁移 down 丢配置数据 | 回滚流程：先 server API 导出 JSON 再 down；纯新增表无存量风险 |
| update_guard 白名单漏登记 | 漏登记表现为"改不动"（非静默清零），AST 护栏强制 `.Set` 形态 |
| authz 白名单/scope 词表漏登记 | `AssertSemantic` 启动失败即验收，fail-closed |

## 6. 测试策略

- **repo 集成**（testutil）：五类型值 roundtrip；集合 CRUD + epoch 唯一性；var CRUD + Update 白名单；`RuntimeVarPublicRead` 可见性过滤（private + `principalAllowed=false` → nil,nil，与"集合不存在"不可区分；public 恒可见）；`LockHead` 0 行 → NotFound 契约（DeleteVarSet 竞态）；版本链（每次写恰好一版、revision 严格单调、summary/actor 正确、集合元数据变更**不**产生版本）；回滚 roundtrip（改 N 次 → 回滚到中间版 → 值逐 key 等于目标快照且 description/created_at 保留；回滚产生新版本可再回滚；淘汰窗口边界：第 51 版写入时最老版被删、回滚到它 NotFound）；并发（两个并发 Update 同集合，版本链无空洞无重复；DeleteVarSet 与并发写竞争最终一致）；footprint；项目删除 CASCADE 四表。
- **app/server 单测**（fake repo）：key/var_set_id 格式拒、value 类型与 oneof 一致、json_value 非法 JSON/超深拒、限额全项拒（含明细错误）、Create 冲突 409/Update 缺失 404/回滚到当前 InvalidArgument、description optional 不改语义。
- **app/client 单测**：可见性矩阵全格（匿名×public=200/匿名×private=404/登录×private=200/跨项目 Principal=403——凭证布尔在用例层、过滤语义在 port，两处分别断言）；etag 命中 unchanged（vars 为空）/未命中全量/epoch 变化必全量；未知 project InvalidArgument。
- **handler 单测**：错误透传、audit resource 标注、resolveProjectID 三级回落。
- **护栏自动生效**：AssertSemantic（PUBLIC 白名单 + scope 词表三处同步）、`grpc_swagger_test`（swagger 与 method_auth 一致性）、authz-matrix 字节锁定、`invoke_test` 反射覆盖（SDK 方法完整性）。

## 7. 明确不做的事

条件下发/灰度引擎（用户属性、百分比 rollout——"仅实名用户可见"属其领地）；realtime 推送与 outbox 事件接入；client 面单 key 读、分页、跨集合聚合、集合目录端点（D14）；批量 import/export；版本 diff 服务端 API（Console 前端拉两版全文自行对比）；无限版本保留；SDK 内置 TTL 缓存（文档给轮询 recipe）；缺省集合（D9）；集合重命名（var_set_id 创建后不可改，重建 + 回滚替代）；按集合单独授权（某集合仅某用户组可见——灰度引擎领地）；秘密/加密存储。

## 改动面清单

**新增 16 文件 + 测试**：
`db/migrations/000011_runtime_vars.{up,down}.sql`、`proto/server/v1/runtime_vars.proto`、`proto/client/v1/runtime_vars.proto`、`internal/domain/projects/runtime_var.go`、`internal/infra/bun/model/runtime_var.go`、`internal/infra/bun/bunrepo/runtime_var_repo.go`、`internal/app/server/runtimevars.go`、`internal/app/client/runtimevars.go`、`internal/api/servergrpc/runtime_vars.go`、`internal/api/clientgrpc/runtime_vars.go`、`sdk/go/server/runtimevars.go`、`sdk/go/client/runtime_vars.go`、`sdk/typescript/src/server/runtimeVars.ts`、`sdk/typescript/src/client/runtimeVars.ts`、`console/src/routes/runtime-vars/pages.tsx`、`console/src/api/runtimeVars.ts`。

**修改 12 文件**：
`proto/shared/v1/authz.proto`（scope 枚举 =17）、`internal/domain/auth/policy.go`（常量 + AllScopeResources + PUBLIC 白名单）、`cmd/server/internal/runtime/authz_policy.go`（映射）、`cmd/server/internal/runtime/grpc.go`（Register×2 + authzFileDescriptors×2）、`cmd/server/internal/runtime/grpc_gateway.go`（register×2）、`internal/infra/bun/provides.go` + `internal/app/provides.go` + `internal/api/provides.go`、`cmd/server/wire_gen.go`（生成物，`task wire:all`）、`sdk/go/server/client.go` + `sdk/go/server/scopes.go`、TS 侧 `server/index.ts`/`torchwood.ts`/`client/index.ts`、`console/src/App.tsx` + `console/src/components/Layout.tsx`、`docs/developer/authz-matrix.md`（`task gen:authz-matrix` 再生成）。

## 回滚

代码 revert（纯新增功能，无存量调用方依赖）→ `000011.down.sql` DROP 四表。down 前若已有真实使用，先经 server API 导出 JSON 存档。
