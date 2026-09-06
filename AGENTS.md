# AGENTS 指南

## 总体说明
- 本仓库使用 Lynx + Clean Architecture：`internal/api`（传输层）、`internal/app`（用例层）、`internal/domain`（领域与端口）、`internal/infra`（适配器层）。
- Torchwood 产品定位包含 **AI/Agent-Native**：Protobuf + OpenAPI 定义可机器读取的 API；Server API 通过 scoped API Key 供 Agent/自动化调用；详见 `docs/roadmap.md` §0 与 `sdk/README.md`。
- 运行时组合通过 Wire 注入：`cmd/server/provides.go` -> `cmd/server/wire_gen.go`。
- 服务器组件由 `cmd/server/provides.go` 启动，包含 gRPC、grpc-gateway、独立 HTTP handler、metrics、Admin Console SPA。
- gRPC/API Proto 定义在 `proto/client`、`proto/server`、`proto/console`、`proto/shared`，生成代码位于 `genproto/`。
- 典型调用链：gRPC handler -> app use-case -> domain repo port -> infra adapter（bun 或 documentdb）。
- 认证中间件位于 `internal/grpc/interceptor` 中，使用 Principal 注入；API_KEY 方法同时允许 admin console session（需带 `X-Torchwood-Project` header）。

## 项目结构补充
- `console/`：React + Vite + TanStack Query + shadcn/ui 管理后台前端，通过 `console/embed.go` 嵌入 Go 二进制。
- `cmd/client/`：Torchwood CLI 二进制（`bin/torchwood`），cobra 实现，通过 sdk/go（server 包 InvokeJSON）以 API Key 调用 Server API；CLI 源码不直接 import genproto/grpc（有 import_guard_test 兜底），方法覆盖完整性由 `sdk/go/server` 的测试保证，新增 RPC 无需在 CLI 登记。
- `internal/api/serverhttp/`：自定义 HTTP handler，例如 Storage multipart 上传下载。
- `pkg/query/`：Appwrite 风格查询 DSL 解析器，供动态文档层使用。
- `internal/testutil/`：集成测试数据库辅助工具。

## 开发流程
- 以 Task 作为主要工作流执行器（`Taskfile.yml`）。常用任务：
  - 一览：`task list`（等价于 `task --list-all`）
  - 基础：`task tools:install`、`task docker:up`、`task docker:down`、`task docker:purge`、`task db:migrate`
  - 生成：`task generate:proto`、`task generate:config`、`task wire:all`、`task generate:all`
  - 前端：`task console:install`、`task console:build`、`task console:dev`
  - 开发：`task dev:server`、`task dev:worker`
  - 质量与构建：`task test`、`task build`
- Proto 生成由 Buf 驱动（`buf.yaml`、`buf.gen.yaml`），输出到 `genproto/`；不要手工编辑生成的 `*.pb.go` 文件。
- 依赖注入由 Wire 驱动；provider 变更后请执行 `task wire:all`。
- 配置 proto 生成由 `task generate:config` 负责；API protobuf 生成由 `task generate:proto` 负责。
- 本地基础设施来自 `docker/local/docker-compose.yml`（Postgres + Redis + MinIO）。
- 修改 Console 代码后需先 `task console:build` 再 `task build`，否则 Go embed 会打包旧版本。

## 配置与环境约定
- 配置 schema 由 `internal/pkg/config/config.proto` 定义，运行时绑定位于 `internal/pkg/config/bind.go`。
- 环境变量覆盖前缀为 `TORCHWOOD_`；键名会从点号路径映射而来，例如 `data.database.source` -> `TORCHWOOD_DATA_DATABASE_SOURCE`。
- `TORCHWOOD_ENV`（development/production）决定关停排水窗口：development 为 0，production 默认 30s；可被 `TORCHWOOD_SERVER_DRAIN_TIMEOUT` 覆盖。Lynx 在绑定 YAML 之前就需要该值，因此不进 `config.proto`。
- MinIO 凭据请使用 `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` 和 `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY`。
- `cmd/server/main.go` 会通过 `godotenv` 尝试加载 `.env`，然后默认从 `./configs` 绑定配置。
- 请使用 `configs/config.yaml.template` 作为基础模板，并将敏感信息保持在环境变量中。
- 反向代理后需恢复客户端真实 IP 时设置 `security.trusted_proxies`（如 `TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32`）；默认不信任 X-Forwarded-For/X-Real-Ip，一律使用 gRPC peer 地址。
- Console admin 会话走 `TORCHWOOD_session_console` HttpOnly cookie（SameSite=Lax，refresh cookie 限 `/v1/console/auth` 路径），前端不再用 localStorage 存 token。

## 数据库约定
- **DocumentDB 子系统边界**：指以 Databases → Collections → Documents 为核心的端到端数据存储方案（元数据 catalog、物理表管理、CRUD 与查询编译、权限集成、事件集成）；模块地图与十条关键不变量见 `docs/developer/06-databases.md` §0。系统静态表（users/sessions/…）是其边界邻居而非组成部分。从零重设计方案见 `docs/design/documentdb-redesign.md`（**已全量落地**；其 POC 直切点的存量升级路径/转出迁移方案见 `docs/design/poc-to-release-migration.md`）。
- 三层物理模型（当前态以本文与 `docs/developer/06-databases.md` 为准；`docs/design/project-data-plane-schema.md` 已落地但部分被 E-5/D-7 supersede，文首有过期横幅）：`public` 控制面+事件脊柱（projects、admins、admin_projects、api_keys、audit_logs、provider_resource_index、outbox）+ **全局 catalog 两表**（`catalog_databases`/`catalog_collections`，attrs/indexes/permissions JSONB 合一 + 物理名 + ddl_seq，`db/migrations/000003`）+ **RBAC 三角色**（`tw_owner`/`tw_app`/`tw_system`，迁移 000004）与 **RLS 判定函数**（`tw_can`/`tw_coll_allows`/`tw_visible`/`tw_roles`，迁移 000004）用 bun + golang-migrate；项目数据面 `tw_<project.id>` 容纳 **系统静态表**（users / sessions / identities / groups / memberships / buckets / files，bun，无 `_id`/`_acl`/`_version`）+ 项目账本 / Functions / OAuth（`internal/infra/projectschema/`；每项目 catalog 四表已退役，000001 no-op + 000011 DROP 存量）；业务文档面 `tw_<project.id>_<database.id>` 只放用户 collection 物理表（`c_<base32(8)>` 服务端分配，逻辑 collectionID 与物理表名解耦；DDL/行查询/索引名走物理名，realtime 频道保持逻辑 ID；物理名不出现在任何 API 响应）。
- **权限内核（阶段③，redesign §3.2/§4.3；roles_sig 与 _acl 通道收口于阶段③-b，迁移 000004）**：文档 ACE 内嵌 `_acl TEXT[]`（`_perms` 表退役）；**权限判定在 RLS policy（`tw_visible`，业务文档表四条 policy + FORCE，`internal/infra/documentdb/rls_policy.go`），角色经 `SET LOCAL ROLE` + `app.roles` GUC 注入（每请求一事务，`clients.Database.RunInTx`；漏注入 fail-closed）**；tw_app 注入同时携带 `app.roles_sig` HMAC 签名，`tw_roles()` 为 SECURITY DEFINER 验签函数（无 sig/错 sig/过期 → 零角色 fail-closed；密钥 = `HMAC-SHA256(jwt.secret, "tw-roles-guc-v1")` 进程派生 + 部署期 owner 作业 `torchwood admin sync-roles-sig` 按双钥轮换落 `tw_secrets`（B15：运行钩子已退役，运行 DSN 对 `tw_secrets` 零权限——迁移 000004；时序 = 迁移 → sync 作业 → 启动，未跑作业前文档查询 fail-closed 属预期）：current/previous 槽位 + `tw_sig_match` 任一钥命中，换钥窗口内旧 sig 不降级，迁移 000004）；sentinel 系统集合（测试面）保留应用层判定 `AllowsDocumentAccess`；**`_acl` 变更通道唯一化为 `tw_set_document_acl`（SECURITY DEFINER owner=tw_system 绕自锁；create/upsert 插入支函数补设、update/upsert 更新支/bulk 函数替换；INSERT/UPDATE 列授权双向排除 `_acl`）**。
- **数组列（阶段③-b，redesign §3.1/§10.5 P0；Intersect/Diff/Insert/Filter 补齐于转出 POC B1）**：属性 `array=true` 落地 PG 原生 `T[]` 列（元素类型仅 string/integer/float/boolean/datetime）；查询算子 `containsAny`（`&&`）/`containsAll`（`@>`）仅 array 属性可用（白名单；**查询侧不设 arrayIntersect/Diff 对应算子**——Appwrite 无查询侧语义，B1 裁决）；写侧 `UpdateDocumentRequest.array_updates` 八算子（APPEND/PREPEND/REMOVE/UNIQUE/INTERSECT/DIFF/INSERT/FILTER；DIFF/FILTER 与 REMOVE 同构受限形态，INSERT 0 基 index 越界尾插；NULL 语义：append/prepend/insert 归一、其余保真）编译为单语句 SET（与 data/increment 组合、OCC 不变），execute-tx op 的 `array_updates` 同源（仅 update 消费）；数组列 key 索引自动 GIN array_ops，unique 对数组列拒绝。
- **事件链（阶段④，redesign §4.5）**：outbox 行带全局 `seq`（000002 identity，B1：单文档全序、集合内分配序、空洞=回滚事务不丢事件）+ AFTER INSERT 触发器 `pg_notify('tw_outbox','')` 唤醒 worker（LISTEN + 5s 兜底，零额外客户端语句）；投递走 Redis Stream `torchwood:events`（每实例一消费组 XREADGROUP/XACK，XTRIM ~100k，Stream 只管传输、重放窗口在 outbox 表）；补偿 = `:changes?since_seq=`（双面 RPC）+ WS subscribe 帧 `last_seq` 门控重放（窗口外 `EVENTS.RESUME_EXPIRED`）；慢消费者 buffer 1024 满水位带 `resync:<last_seq>` close 断开，客户端重连带 last_seq 即重同步；信封载荷上限 1MiB（对齐 H1），`transaction_id` 标识 execute-tx 批（事件序 = op 序）。
- sentinel `_`（`ident.ProjectDataPlaneID`）仅内部寻址 / 对外 `RejectExternalDatabaseID` 拒绝；系统资源不再是文档集合。`default` 是普通第一库（可删可重建）。DDL 只走两段式 `businessSchema`，永不解析一段式。`project.id` / `database.id` 规则见 `docs/developer/06-databases.md`。
- 动态文档查询使用 Appwrite 风格 DSL（`pkg/query`），支持 `equal`、`greaterThan`、`contains`、`containsAny`、`containsAll`、`orderDesc`、`limit` 等（后三个数组算子/构造器仅 array=true 属性可用，服务端白名单校验）。向量近邻查询走 `query.vectorSearch`（KNN 一等算子，非 filter 节点；typed builder only，DSL 字符串不支持；需 metric 匹配的 hnsw 索引，iterative scan 为契约；多页经 `kvc:` 距离游标续传——语义见 `docs/developer/06-databases.md` §6）。

## 编辑时应遵循的模式
- 保持端口在 domain、适配器在 infra。
- gRPC 方法必须带 proto authz 注解，否则 `collectMethodsByAccess` 会报错。
- **Proto 规范**：删除字段一律 `reserved`（字段号 + 字段名），禁止复用字段号；`deprecated = true` 保留为合法过渡态（兼容旧 SDK 一个版本周期后再迁 `reserved`，过渡期内服务端忽略该字段）；
  更新类请求的可选字段用 `proto3 optional` 表达 presence（未设置=不修改）；
  时间字段一律 `google.protobuf.Timestamp`（HTTP JSON 为 RFC3339）；
  OpenAPI 建模约定见 `docs/developer/09-api-guide.md` §1.4（swagger 扩展与
  `method_auth` 一致性由 `internal/infra/server/grpc_swagger_test.go` 断言）。
- 列表查询复用 `pkg/crud` 或等价的 AIP-132/158/160 抽象，不要手拼 SQL filter/order；动态文档优先使用 `pkg/query`。
- JWT claims 保持与 `pkg/jwtparser` 的映射兼容。
- Console 前端组件放在 `console/src/components/ui/`，样式基于 Tailwind + shadcn/ui。

## 已接受取舍（P3-18）
- **app 层允许 `grpc/status`**：`internal/app/*` 直接使用 `google.golang.org/grpc/status` 与 `codes` 表达领域错误（56 文件），视为项目约定而非分层违规，不引入中间 AppError 类型；`domain` 层仍保持纯净，禁止依赖 gRPC。
- **SDK 覆盖测试保证范围措辞收敛**：CLI 不直接 import `genproto`，方法覆盖完整性由 `sdk/go/server` 的反射测试保证（`invoke_test.go` 遍历 registry 断言每个 server 方法在 `findServerMethod` 可解析），措辞收敛为“覆盖测试保证范围”，新增 RPC 无需在 CLI 登记即自动可用。

## 特别约定
- 对话和文档优先使用简体中文。
- 管理后台通过 `/console/` 路径访问，由 Go server 嵌入并 serve。
- 对外发布（真实存量用户）前过 `docs/developer/15-exit-poc.md` 转出 POC 门禁（A 区清零；挂账的活跃清单以该文件为准）。
