# Torchwood 开发者文档

面向开发者的完整技术文档，覆盖架构、环境搭建、配置、代码生成、认证授权、核心子系统、开发指南、测试与运维。所有文档以仓库当前代码为准；如与代码不一致，以代码为准（`AGENTS.md` 为开发约定总纲）。

> 2026-09-16 全量重写：逐章按代码复核并重组成人类可读的规范文档。`authz-matrix.md` 为生成物（`mise run gen:authz-matrix`），不在重写范围。

## 章节索引

| 章节 | 内容 | 适合读者 |
|------|------|----------|
| [01-overview.md](01-overview.md) | 架构总览：产品定位、技术栈、Clean 四层、目录树、四进程拓扑、数据面三层、典型调用链、关键设计机制 | 所有开发者 |
| [02-quickstart.md](02-quickstart.md) | 环境搭建与快速开始：前置条件、本地基础设施、双账号准备、六步启动（含 authenticator 引导）、端点、任务速查、CLI 与多项目 profile | 新加入的开发者 |
| [03-configuration.md](03-configuration.md) | 配置体系：config.proto schema、`TORCHWOOD_` 环境变量映射、加载顺序、Functions 配置、`TORCHWOOD_ENV` 排水、特殊配置项 | 所有开发者 |
| [04-codegen.md](04-codegen.md) | 代码生成与工具链：mise、Buf proto 生成、config 生成、Wire、生成顺序与漂移门禁、新增 RPC 生成侧清单 | 后端开发者 |
| [05-authentication.md](05-authentication.md) | 认证与授权：四凭证、策略注册表（proto 注解 → PolicySet → 拦截器）、API Key 与 scope 词表、启动期 fail-closed 断言、频控、注册策略、威胁模型 | 后端开发者 |
| [06-databases.md](06-databases.md) | 动态文档数据库：三类库、全局 catalog、物理表名、查询（typed AST / KNN）、`_acl` + RLS 权限内核、OCC、事务、写幂等、事件链 | 后端开发者 |
| [07-storage.md](07-storage.md) | 存储子系统：S3/MinIO 适配、multipart 上传下载、分片上传、File Token、安全输出与缩略图 | 后端开发者 |
| [08-functions.md](08-functions.md) | 函数执行：dispatcher 唯一执行路径、常驻 runner 池、构建与平台代装依赖、执行身份（`twx_` token）、触发器（HTTP / cron / 事件）、客户端调用面 | 后端开发者 |
| [09-api-guide.md](09-api-guide.md) | 后端 API 开发指南：新增 gRPC 方法的完整流程（proto → domain → app → infra → api → Wire）、authz 注解、protovalidate、分页、错误映射、OpenAPI 一致性 | 后端开发者 |
| [10-console.md](10-console.md) | Console 前端开发：目录结构、API client 封装、会话 cookie、时区偏好、新增页面流程、治理面板 | 前端开发者 |
| [11-testing.md](11-testing.md) | 测试与质量：测试分层、testutil 集成库、`mise run test`、lint 全量门禁、CI 流水线、健康检查 | 所有开发者 |
| [12-sdk.md](12-sdk.md) | 官方 SDK：TypeScript（门面、17 个 Server 服务、执行身份入口、AnalyticsEventBuffer）与 Go（client/server、InvokeJSON、FileTokenStore） | SDK 用户 / Agent 集成方 |
| [13-operations.md](13-operations.md) | 部署与运维：四进程形态、构建发布、生产配置要点（双账号契约、roles-sig 时序）、健康检查、规模预警线、备份恢复、vector / RBAC runbook | 运维 / 部署负责人 |
| [14-agent-tools.md](14-agent-tools.md) | Agent 默认工具箱：overlay 18 动词映射现有 Server RPC；完整 API 覆盖见 `authz-matrix.md` | Agent / SDK 集成方 |
| [15-exit-poc.md](15-exit-poc.md) | 转出 POC 检查单：发布前门禁——A 区清零前不得对外发布；转出后挂账的活跃清单（**门禁账本，只追加闭环证据，不做文风重写**） | 维护者 / 发布负责人 |
| [16-document-modeling.md](16-document-modeling.md) | 文档建模指南：跨集合引用模式（1:N 引用属性、M:N 数组 / junction、ExecuteTransactions 原子写、删除卫生协议、查询陷阱清单，附 Agent 提示词规约） | Agent 构建者 / 后端开发者 |
| [17-update-write-guard.md](17-update-write-guard.md) | bun 更新写规范与 internal_id 漂移修复：UPDATE 列白名单强约束、三道护栏、存量漂移 runbook | 后端开发者 |
| [18-analytics.md](18-analytics.md) | 内置事件分析：双面摄入、月分区存储、rollup 聚合、固定形状查询、口径声明、TS SDK 缓冲器与各端接入配方 | 后端开发者 / 端侧接入者 |
| [19-runbook.md](19-runbook.md) | 版本化资源迁移：`NNNNNN_name.yaml` 双段文件、18 动词幂等 reconcile、checksum 防篡改、up / down / status / forgive | 后端开发者 / 运维 / CI 维护者 |
| [20-leaderboards.md](20-leaderboards.md) | 排行榜：Board / Entry 模型、期派生与封榜语义、tie-break 全序（rank / position）、幂等与限频、声明式结榜发奖（Phase 2 已落地） | 游戏开发者 / 后端开发者 |
| [authz-matrix.md](authz-matrix.md) | 全方法授权矩阵（**生成物，勿手改**）：`mise run gen:authz-matrix` 从策略注册表渲染，含档位与威胁模型已知取舍 | 后端 / 安全评审 |

## 推荐阅读路径

- **新开发者**：01 → 02 → 04 → 09 → 11
- **后端功能开发**：03 → 04 → 05 → 06 → 09
- **前端页面开发**：10（配合 05 §4 与 09 §8）
- **Agent / SDK 集成**：12 → 14 → 16（配合 05 §6 与 roadmap §0）
- **部署上线**：13（配合 03 与 11 §5）

## 相关文档

- `README.md` / `README_ZH.md` — 项目总览与快速开始（中文 / 英文）
- `AGENTS.md` — 开发约定（分层、生成、配置、数据库约定，必读）
- `docs/roadmap.md` — 开发路线图（含 AI / Agent-Native 战略与验收标准）
- `docs/design/` — 设计文档（documentdb-redesign、analytics、runbook、functions-v3 等）
- `docs/implementation-*.md` — 各功能实现说明（bootstrap-and-cli、health-observability、functions-executor、storage-chunked-upload 等）
- `docs/archived/` — 归档设计文档
