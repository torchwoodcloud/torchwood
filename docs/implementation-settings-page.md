# Torchwood Console Settings 页面实现方案

> **2026-09-09 追记：本文档描述的全局 `/console/settings` 页已退役。** 项目配置统一
> 收敛至项目设置区 `/console/projects/:id/settings`（显式项目作用域，四 tab：
> 基本信息 / 注册与登录 / OAuth / 危险区，见 `console/src/routes/projects/settings.tsx`；
> OAuth Providers 与项目详情页的注册策略/邀请码/重定向白名单同址合并，
> 侧边栏 Settings 变更为当前项目的设置捷径，旧路径重定向）。本文保留作为
> 当年「项目」Tab 能力的历史实现方案存档，其余内容不再反映现状。
>
> 状态：**已实现**（2026-08-09 验收通过：UpdateProject 全链路 + Console「项目」Tab +
> description 口径策略 (a) 双侧 512 上限；10 个 use-case 用例 + 3 个 handler 用例 +
> scope 用例全绿；范围外资产 docs/developer/ 与品牌 logo 一并交付）
> 目标读者：维护者与后续扩展
> 关联：`docs/roadmap.md` §2.8（Admin Console UI）、`AGENTS.md`（开发约定，必读）
> 参考：`docs/implementation-health-observability.md`（上一轮同类方案：先审查、后实现、再汇报）
> 修订记录：2026-08-09 v2（独立评审修订：description 校验口径、updated_at 处理、name 撞名查重、
> principal 改从 ctx 取、scope 测试必做、viewer 可写决策声明等）

---

## 1. 目标与验收标准

补齐 Console Settings 页面的「项目基本信息」编辑能力，并同步 roadmap 状态
（OAuth Providers 配置与 Messaging 只读说明**已实现**，仅文档滞后）。

**验收标准**：

1. `PATCH /v1/server/projects/{id}`（`UpdateProject`）可修改项目 `name`/`description`；
   平台 admin（owner/admin）可更新任意项目；非平台 admin 只能更新
   `principal.ProjectID` 对应的项目，越权返回 NotFound；**仅当 name 与 description
   均未提供**时返回 `InvalidArgument`。
2. API Key 调用 UpdateProject 需 `projects.write` scope（无 scope → `PermissionDenied`）。
3. Console Settings 新增「项目」Tab：展示 name/description/status/created_at/updated_at，
   可编辑 name/description（保存后 invalidate 项目查询）。
4. Settings 页面风格统一：改用 `PageHeader`（补 `settings` 路由标题映射）。
5. `go test ./...`、`task lint`、`task build` 全绿。

**权限决策（评审补充，显式声明）**：非平台 admin（含 member/viewer，只要有绑定
项目访问权）可更新其绑定项目——与 `GetProject` 语义一致；若需按角色收紧，另加
`principal.Roles` 校验，本期不做。

**不做（明确范围外）**：SMTP 真实只读展示（server 无配置暴露端点，维持静态说明）；
`DeleteProject` RPC（repo 层虽有，但需级联清理动态 schema 与 admin_projects，成本高、
roadmap 无要求——**后续版本单独排期**，在 roadmap 记 TODO）；member/viewer 项目列表
（既有设计，非本次范围）。

---

## 2. 现状盘点（调研结论）

| 项 | 现状 |
|---|---|
| Settings 页面 | `console/src/routes/settings/pages.tsx`（318 行）已实现 OAuth Providers 完整 CRUD + Messaging 只读说明；路由（App.tsx:92）与侧边栏（Layout.tsx System 分组）已注册；**未使用 PageHeader**（自带 h1，pages.tsx:34-39） |
| Projects API | proto 仅 3 个 RPC（Create/List/Get，projects.proto:15-25）；`Project{id,name,description,status,created_at,updated_at}`（:42-49）；use-case `GetProject` 越权保护（projects.go:121-133） |
| UpdateProject | **repository 层已有**：domain `UpdateProject`（repository.go:10）+ bunrepo 实现（project_repo.go:67-71，全列覆盖写）；**`GetProjectByName` 也已存在但未被任何 use-case 使用**（repository.go:8）；缺 proto/use-case/handler/scope/前端/测试 |
| CreateProject 校验 | **不校验 description**（projects.go:40-84 只查 name 非空 + id 白名单）；DB 无 description 长度约束（000001_init_tables.up.sql:5）；name 有**唯一索引**（:11，CreateProject 因 id 由 name 派生天然避免撞名） |
| 角色模型 | owner/admin = 平台 admin（validator.go:133-142）；member/viewer 的 `ListProjects` 返回空（projects.go:99-102）但 `HasProjectAccess` 不看角色（admin_project_repo.go:22） |

---

## 3. 分层实现规格

### 3.1 proto（`proto/server/v1/projects.proto` 扩展）

```proto
rpc UpdateProject(UpdateProjectRequest) returns (Project) {
  option (google.api.http) = { patch: "/v1/server/projects/{id}", body: "*" };
}

message UpdateProjectRequest {
  string id = 1;
  optional string name = 2;         // 空值不修改（proto3 optional 表达 presence）
  optional string description = 3;
}
```

- service 级 `default_access: ACCESS_API_KEY` 已存在（:13），新 RPC 自动继承
  （`collectMethodsByAccess` 的 `resolveMethodAccess` 会 fallback 到 service default），
  无需方法级注解。
- `optional` + `patch + body:"*"` 风格与 `functions.proto:35/143-147`、
  `databases.proto:146-148`、`storage.proto:76-77` 一致。
- `task generate-proto`（genproto 不手改）。

### 3.2 use-case（`internal/app/server/projects.go` 扩展）

```go
type UpdateProjectCommand struct {
    ProjectID   string // 目标项目 id
    Name        *string
    Description *string
    // 无 Principal 字段：use-case 内从 contexts.Principal(ctx) 取（与
    // CreateProject/GetProject/ListProjects 的仓库模式一致，见 projects.go:41/87/122）
}

func (d *Projects) UpdateProject(ctx context.Context, cmd UpdateProjectCommand) (*projects.Project, error)
```

流程（顺序重要）：

1. `cmd.ProjectID` 非空（`InvalidArgument`）。
2. **"nothing to update" 前置检查**：`cmd.Name == nil && cmd.Description == nil` →
   `InvalidArgument` "nothing to update"（**在取数之前**，对齐 storage.go:328 先例——
   避免"项目不存在 + 全空请求"返回 NotFound 的语义歧义）。
3. 主体：`p, ok := contexts.Principal(ctx)`；`!ok` → `Unauthenticated`
   （与 CreateProject :41-44 同款）。
4. **越权保护**：`!p.IsPlatformAdmin` 时 `cmd.ProjectID != p.ProjectID` → `NotFound`
   （防枚举，与 GetProject :129-131 一致）。
5. 取现有项目：不存在 → `NotFound`。
6. `cmd.Name != nil` 时：
   - trim 后为空 → `InvalidArgument` "name is required"（**有意收紧，严格于
     CreateProject**——CreateProject 对空白 name 回落 `project-<uuid>`，见 :53-57；
     本方案对编辑场景拒绝空白名，更合理，需在测试注明）。
   - **撞名查重（评审补充）**：`Name != 现项目名` 时调
     `d.repo.GetProjectByName(ctx, *cmd.Name)`（已有接口，repository.go:8），命中
     → `InvalidArgument` "project name already exists"（避免依赖 DB unique
     violation → 500）。
7. `cmd.Description != nil` 时：长度上限 512（**与 CreateProject 对齐策略见下**）。
8. **`project.UpdatedAt = time.Now()`（评审补充）**：repo 的 UpdateProject 是全列
   覆盖写（project_repo.go:67-71），不处理则 updated_at 永远停滞——更新前必须
   置当前时间（参照 functions/management.go:131）。
9. 调 `d.repo.UpdateProject(ctx, &project)`。

**description 校验口径（评审修正——二选一，实现 agent 必须选一个并保持一致）**：

- (a) **推荐**：两侧同时加同一上限——`CreateProject` 与 `UpdateProject` 都对
  description 校验 ≤512（并给 CreateProject 补一个超长 description 的测试）；
- (b) 若不动 CreateProject：则 UpdateProject **也不加** description 校验
  （删除本方案的"512"表述，保持两侧一致的无约束）。

返回值：更新后的 `*projects.Project`。

### 3.3 gRPC handler（`internal/api/servergrpc/projects.go` 扩展）

```go
func (s *ProjectsService) UpdateProject(ctx context.Context, req *serverv1.UpdateProjectRequest) (*serverv1.Project, error)
```

- `contexts.WithAuditResource(ctx, req.GetId())`（审计资源）。
- 透传 optional 字段（`req.Name != nil` → 指针）；use-case 内自取 principal，
  handler 只透传 ctx。
- 错误直接透传。

### 3.4 scope 映射（`pkg/grpc/interceptor/apikey_scope.go`）

ProjectsService 段（:78-80）追加：

```go
"/torchwood.server.v1.ProjectsService/UpdateProject": {"projects", "write"},
```

### 3.5 Console

**`console/src/api/projects.ts`**（追加）：

```ts
export async function updateProject(
  id: string,
  input: { name?: string; description?: string }
): Promise<Project> {
  const res = await api.patch<Project>(`/server/projects/${id}`, input);
  return res.data;
}
```

**`console/src/routes/settings/pages.tsx`**（扩展）：

1. 新增「项目」Tab（`"project"`），默认 Tab 保持 `"oauth"`；Tab 顺序：项目 / OAuth /
   Messaging。
2. `ProjectInfoPanel`（新组件）：
   - `useAuth().projectId` 为空 → 提示"请先在侧边栏选择一个项目"（与 OAuth panel
     pages.tsx:131-135 一致）。
   - `useQuery(["projects", projectId], () => getProject(projectId!), {
     enabled: !!projectId })`（**必须带 enabled**，对齐 OAuth panel :75）。
   - 展示 `DetailGrid`：ID/name/description/status/created_at/**updated_at**（评审
     补充——呼应更新闭环）。
   - 编辑表单：name + description 两个 `FormField`，mutation 调 `updateProject`，
     成功后 `toast.success` + invalidate `["projects", projectId]` 与 `["projects"]`
     （后者刷新侧边栏 ProjectSelector 项目列表）。
3. **风格统一**：SettingsPage 改用 `PageHeader`（title="Settings"，description 改为
   中性文案如"当前项目的配置与管理"，因加入项目 Tab 后不再是 OAuth 向文案）；
   `console/src/components/PageHeader.tsx` 的 `routeNames` 补 `settings: "Settings"`
   映射（:4-17）。PageHeader 已被 FormPage/Dashboard/CollectionLayout 广泛使用，
   替换无破坏。

**可选加分项（不阻塞验收）**：`console/src/routes/Dashboard.tsx` 加「设置」入口链接。

---

## 4. 实现顺序（建议）

| 步骤 | 内容 | 验证 |
|------|------|------|
| 1 | proto + `task generate-proto` | 编译通过 |
| 2 | use-case UpdateProject + 单测 | `go test ./internal/app/server/...` |
| 3 | gRPC handler + scope 映射 + `task wire-all`（无依赖变化，wire_gen 应无 diff） | `go build ./...` |
| 4 | Console：api/projects.ts + settings 页面「项目」Tab + PageHeader 映射 | `task console-build` |
| 5 | 全量验证 + roadmap 更新 | 见 §5 |

每步完成跑 `gofmt -l .`（必须空）+ `go vet ./...`。

---

## 5. 测试与验证

- **use-case 单测**（`internal/app/server/projects_test.go` 追加，复用
  `platformAdminCtx` :21-27 模式）：
  - 平台 admin 更新 name/description 成功（断言 repo 层落库 + **updated_at 单调递增**）；
  - 非平台 admin 更新**自己**项目成功；
  - 非平台 admin 更新**他人**项目 → NotFound；
  - 项目不存在 → NotFound；
  - **name 与 description 均未提供** → InvalidArgument（"nothing to update"）；
  - 空白 name → InvalidArgument（有意收紧，注明）；
  - **改名撞名**（存在同名项目）→ InvalidArgument；
  - 若选 §3.2 策略 (a)：CreateProject 超长 description → InvalidArgument。
- **handler 测试**：新建 `internal/api/servergrpc/projects_test.go`（**必做**；
  参照 `functions_test.go` 的 stubRepo + `contexts.WithPrincipal` 模式——servergrpc
  下无 apikeys 测试文件，不要参照不存在的东西）——冒烟：合法请求透传、无 principal
  → Unauthenticated。
- **scope 测试（评审修正——必做，文件已存在）**：
  `pkg/grpc/interceptor/apikey_scope_test.go`（162 行，含 `TestAPIKeyScopeAllowed`）
  追加断言：`/torchwood.server.v1.ProjectsService/UpdateProject` 在
  `projects.write`/裸 `projects`/`*` 下放行，`projects.read`/无关 scope 拒绝。
- **全量验证**：`go test ./...`（.env 提供 TORCHWOOD_TEST_DATABASE_SOURCE）、
  `task lint`、`task build`。
- CI 无需改动。

---

## 6. 关键坑（实现时必须注意）

1. **authz fail-closed**：UpdateProject 继承 ACCESS_API_KEY 服务默认即可；但
   **必须登记 `apikey_scope.go`**，否则 API key 调用（含 `*` scope）被拒绝。
2. **越权保护语义**：非平台 admin 越权返回 **NotFound**（非 Forbidden）——与
   GetProject（projects.go:129-131）一致，防项目存在性泄露。
3. **optional 字段**：proto 用 `optional string` 表达 presence；handler 必须区分
   "未传"（不修改）与"传空"（校验后拒绝）——name 传空串视为校验失败。
4. **改名不改 id**：UpdateProject 只改 name/description，不重派生 id。
5. **updated_at 必须手动置**（§3.2 步骤 8）——repo 全列覆盖写，不置则停滞。
6. **name 撞名**：必须 `GetProjectByName` 查重（§3.2 步骤 6）——name 唯一索引存在，
   不查重会 500。
7. **description 口径**：§3.2 二选一，两侧保持一致（含 CreateProject 补测试）。
8. **Console 默认 Tab**：新增「项目」Tab 不改变默认 `"oauth"`。
9. **PageHeader 映射**：`routeNames` 补 `settings`，否则 breadcrumb 显示原始 segment。
10. **wire 无变化**：UpdateProject 只新增方法，不新增依赖——`task wire-all` 后
    wire_gen 应无 diff（如出现 diff 需排查）。
