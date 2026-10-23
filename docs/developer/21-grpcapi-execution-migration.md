# 21 · grpcapi 执行侧迁移：授权执行切换到库 Decider

> 2026-10-23，feat/grpcapi-v3。grpcapi v1.0 硬验收项（GENERALIZATION §11.4、
> MIGRATION §6）：**torchwood 的 legacy 双投影退役，授权执行走库 Decider**。
> 裁定依据「能接入 ≠ 被替代」——此前 torchwood 策略声明进了库（policy.Build），
> 执行仍留在项目侧（interceptor 手工档位判定 + params-JSON 投影绕行）。

## 1. 迁移前病灶

- `legacyProvider` 把旧五档注解整档编码为单个 opaque kind 的 params-JSON；
  `FromPolicySet` 再解码回 domain 形状——类型安全与 manifest 可读性双损；
- `interceptor/jwt.go` 的档位组合判定（SERVER 凭证族 ∪ scope 门 ∪ 角色门 ∪
  permission 门）手工执行，完全绕过库的 Decider——策略声明与策略执行分裂
  在两个系统。

## 2. 迁移后形态

```
proto 注解（vendored authz.proto，路径 A）
  └─ legacyProvider ──展开──▶ 五档规范树（domainauth.TreeSpec.Expand）
                                │
        ┌───────────────────────┴───────────────────────┐
        ▼（执行面）                                    ▼（派生面）
authz.NewInterceptor(set, nil)                   View（ViewOf/Views）
RequirementDecider 按树求值                        ├─ AssertSemantic（语义断言）
gRPC 链：clientInfo → authn → authz → …           ├─ 矩阵文档 / well-known
                                                  ├─ ScopeVocabulary（词表）
HTTP 面：AllowsAPIKey（委托 scope.Set             └─ golden 基线
        .Satisfies ∨ GrantsInstance，与树同构）
```

五档规范树（唯一策略载体，`domain/auth/policy_tree.go`）：

| 档位 | 树形 |
| --- | --- |
| PUBLIC | `{anonymous}` |
| END_USER | `require[principal_kind(user)]` |
| PERMISSION | `require[permission(perms, match=any)]` |
| SERVER | `any_of[ admin门, scope(res:op), resource(res)? ]`（组间 OR） |
| SYSTEM | 无对外契约，展开即错（Build 期失败） |

SERVER 档细节：

- **admin 门**：声明 admin_roles 时 `admin_role(roles...)`；未声明（不限角色）
  为 `principal_kind(admin)`；
- **scope 门**（操作级）：`*`/`all`/裸资源/`res:op` 命中即过；
- **resource 门**（实例级，可选）：仅当资源可实例寻址（databases/storage）
  且请求携带寻址字段（database_id/bucket_id/id）时展开；无目标请求返回
  哨兵（永不匹配任何实例 token），空 id 不放大成 Internal；
- **principal 投影**（`domainauth.ProjectPrincipal`）：end_user→user、
  admin→admin、service→api_key、execution→execution、system→system；
  `roles`/`permissions` claim = Roles（PERMISSION 门读，等价旧 HasAnyRole）、
  `admin_roles` claim 仅 admin 主体、`scopes` claim = Permissions。

提交序列（可单 PR revert）：

| 提交 | 内容 |
| --- | --- |
| `af855f74` | C1 policy 树重写（provider 产树 + View 识别投影，golden 零漂移） |
| `579bca08` | C2 执行点切换（jwt.go 手工判定退役、库 authz 拦截器接线、serverhttp 切库语义、stream 三流型测试） |
| `cfa9bb4c` | C3 投影退役（PolicySet 包装删除、消费面直吃 *policy.Set、wire 重生成） |
| `a4e14628` | C4 manifest golden 固化 + 本报告 |
| `f8c12e42` | gateway 兼容垫片迁 v0.3.0 形态（HeaderPolicy/ErrorEncoder，独立提交不与执行侧混提） |

## 3. golden 等价性验收（MIGRATION §2.5 / §6）

双基线锁定，均字节级：

- `testdata/policies.golden.json`——**迁移前**（阶段 0）固化的五档投影基线。
  全程未改动：C1 后重生成零漂移 = 「新树经 View 识别 ≡ 旧 params-JSON 投影」
  的逐方法等价证明（265 方法 × access/permissions/admin_roles/scope/
  request_fields/streaming 全字段）。
- `testdata/policies.manifest.golden.json`——**迁移后** v3 树自身的
  `set.Manifest().JSON()` 快照（新增），供此后策略漂移的 CI diff。当前树形
  分布：anonymous 30 · END_USER 47 · PERMISSION 30 · SERVER 158（其中带
  resource 实例门 37）。

## 4. 显式 review 的行为差异

以下差异均经逐条 review，**拒绝方向全部不变**（没有任何旧拒绝变放行）：

| # | 差异 | 理由 |
| --- | --- | --- |
| D-1 | SERVER 档树形采用组间 **OR**（`any_of[admin门, scope, resource]`），非 composite_gate_test 的组内 AND（`[scope ∧ resource]`） | 库 `GrantsInstance` 不认操作级 token：AND 形会让 `databases.read` 这类**最常用**的操作级 scope 在全部实例寻址方法（Get/Update/GetDocument…）上被拒——违反 MIGRATION §6「scope 四形态（`*`/`all`/resource/resource.op）行为不变」与既有矩阵锁。OR 形逐组等价旧逐 token 合取语义（∃token: opOK ∧ instOK ≡ ∃token:opOK ∨ ∃token:instOK，因 opOK 类与 instOK 类的交集恰为两门皆过的 token 类）。 |
| D-2 | ~~实例+方向**单 token** 形态（`databases:blog.read`）从放行收窄为拒绝~~ **已修复**（2026-10-23，库 `5ea3974`）：resource 门声明值扩展 `<res>:<op>` 方向感知形态，`GrantsInstanceFor` 剥实例段方向后缀按方法方向比对——单 token「方向 + 实例」合取语义全量恢复（`TestAllowsAPIKey_Scoped` 断言已回正，manifest golden 37 个 resource 门改携方向）。修复前短暂收窄期间无真实用户（转出 POC 前），无兼容负担。 |
| D-3 | SERVER 面端用户凭证：401 → 403（原 authn 凭证族门禁「developer API requires…」语义改由 any_of 全组失配表达） | 库 Decider 的拒绝码映射：有 principal 一律 PermissionDenied。CLI 退出码契约（40x→2）不受影响。矩阵 expectedDecision 已同步并注释。 |
| D-4 | END_USER 门从角色串（`HasAnyRole(["users"])`）改为身份类型 `principal_kind(user)` | 生产等价：`UserRoles.LoadUserRoles` 无条件携带基础角色 `users`（docrole 词表主权），不存在无 users 角色的端用户主体；旧测试的合成主体（roles=["guests"]）在生产不可达。任务五档映射与 MIGRATION §2.2 均裁定为此形。 |
| D-5 | 未登记策略的方法直接调用：403 → 500（库 MissingPolicyCode 默认 Internal） | 启动期 `AssertAllRegisteredHavePolicy` 兜底不变，运行期不可达；Internal 更符合「配置错误」语义。 |
| D-6 | admin 的 `ValidateAdminProjectAccess` 先于角色/scope 拒绝执行（旧实现角色门在前） | authn（业务校验）在链上先于 authz（档位执行）。代价仅是被拒 admin 请求多一次项目访问读；拒绝结果不变。 |
| D-7 | PERMISSION 门 match=**any**（旧 HasAnyRole 语义），registry 默认保留库约定 MatchAll、requirement 显式 `match:"any"` | `[owner,admin]` 声明语义是「owner 或 admin」，MatchAll 会要求同时持有两角色。manifest golden 可见 `match: "any"`，供 review。 |
| D-8 | 流式 open-time 授权（库 Stream 拦截器）不携带请求消息——实例 token key 在流建立时拒绝 | 与旧「无目标方法对实例限定 scope 恒不匹配」语义一致（fail-closed）；裸资源/通配符/操作级 token 照常放行。torchwood 当前无流式 RPC（AssertSemantic 维持禁用），stream 面以合成集验证（三流型 + per-message，`grpc_authz_stream_test.go`）。 |

## 5. 删除 / 保留清单

**删除**（对照迁移前 3e9d7a6c）：

| 位置 | 内容 | 量 |
| --- | --- | --- |
| `interceptor/jwt.go` | SERVER 凭证族门禁、scope 门 + `apiKeyScopeTargets`/`scopeTargetGetter` 实例提取、admin 角色门、PERMISSION/END_USER 角色门、policy_missing 前置检查 | 净 -47 行（387→340；删执行逻辑约 90 行，新增 principal 双注入/收尾约 40 行） |
| `domain/auth/policy_v3.go` | `PolicySet` 存储型包装、`FromPolicySet`、params-JSON 编解码（`decodeStrings`/`splitCSV`/`accessFromString`/`legacyAccessRequirement`/`inferMethodPolicy`/`splitScopeValue`）、`NewPolicySet`、`Raw()` 存在性投影 | 净 -190 行（含重写为 View 助手） |
| `domain/auth/scope_target.go` | `AllowsAPIKeyTargets` 手写逐 token 匹配 + `ScopeTargets`/`match` | -35 行 |
| `runtime/legacy_provider.go` | `legacyPolicy` params-JSON 编码、`legacyAccessKindDef`、`legacyRegistry`（下沉 domainauth） | 净 -60 行 |

**保留**（及原因）：

| 位置 | 原因 |
| --- | --- |
| 认证全链（凭证提取/校验/Principal 缓存/频控） | 任务边界：只迁执行；认证是 torchwood 主权 |
| PUBLIC 面无效 X-API-Key 401 + T-02 失败限速 | 认证语义，留在 authn（库 anonymous 树放行一切，401 收口在认证侧） |
| X-Torchwood-Project / ValidateAdminProjectAccess / 拒绝审计（M5 C6） | 业务校验与审计原位；拒绝审计经 `AuthzOnDecision` 回调延续到库拦截器拒绝 |
| `scope_target.go` 的 `ParseScopeToken`/`ValidateScopeTargetID`/`ScopeVocabulary.Valid` | 创建期校验（key 创建面），不是执行；执行语义已委托库 |
| serverhttp `httpAuth.authorize` 组合结构 | HTTP 存储面的主体族语义（端用户可上传/下载）≠ gRPC 方法的树；其 scope 谓词已全部委托库（`AllowsAPIKey`），不存在第二份匹配实现 |
| `AssertSemantic`/档位分类/词表 | 项目主权断言，消费 View 派生面 |
| legacy 注解（路径 A） | codemod 属独立后续工作（路径 B），不与本次混提 |

## 6. grpcapi 缺口清单（已闭环）

| # | 位置 | 缺口 | 处置 |
| --- | --- | --- | --- |
| G-1 | `policy/scope/resource.go`（`GrantsInstance`） | 实例段不剥离 op 后缀且无方向感知模式——T-02「单 token = 方向 + 实例」合取语义不可表达 | **已修复**（库 `5ea3974`）：resource 声明值扩展 `<res>:<op>` + `GrantsInstanceFor`（`GrantsInstance` 行为不变为兼容不变量）；OR 组合 + 方向感知门的完备范式用例入 `authz/composite_gate_test.go` |
| G-2 | `authz/interceptor.go`（Stream open-time） | open-time 授权不携带请求消息，实例门在流建立时恒无目标（语义固有：open-time 时消息未读，读取即消费） | **文档化**（库 `5ea3974`）：此类方法应使用 `StreamAuthPerMessage` 或要求资源整体/通配授权；torchwood 无流式 RPC，暂无影响 |

其余能力（`FieldID` 提取器、`Match` requirement 级覆盖、`OnDecision`
回调、`TargetedRequest` 侧通道）均足够表达，未在 torchwood 侧重新发明执行器。

## 7. 验收清单（MIGRATION §6 对照）

- [x] 执行侧迁移：legacy 双投影退役，授权执行走库 Decider（本文件）
- [x] `policy.Build` + `legacyProvider` 产出与 v2 golden 逐方法等价
      （`testdata/policies.golden.json` 零漂移 + manifest golden 新增）
- [x] API key scope 四形态（`*`/`all`/resource/resource.op）行为不变
      （矩阵全量 + `TestAllowsAPIKey_BackwardCompat` + VerifyToken 门）
- [x] PERMISSION/permissions、SERVER/admin_roles 行为不变（矩阵 + 语义锚点；
      permission 门 match=any 见 D-7）
- [x] streaming 方法 open-time 授权通过；per-message 单独验证
      （`grpc_authz_stream_test.go` 三流型 + per-message 实例门；生产流式
      维持禁用——见 D-8）
- [x] public 面无效凭证仍 401 不降级（`jwt_public_invalid_key_test.go` 全绿）
- [x] gateway 错误体/header 行为不变（gateway 代码零改动；v0.3.0 垫片）
- [x] guard 面：torchwood 侧对应守卫全绿（swagger 一致性/矩阵文档字节锁/
      SDK scope 契约/路由序；guard.Run 本体属 grpcapi 仓验收）
- [x] 路由表与旧注册面一致（`AssertAllRegisteredHavePolicy` +
      `gateway_route_order_test`）
- [x] 全量 `go test -race ./...` 与 CI 通过

## 8. 后续（2026-10-23 收尾状态）

- ~~grpcapi v0.3.0 打 tag 后撤 replace 切 tag~~ **已完成**：v0.3.0 发布
  （genproto/v0.3.0 → 主仓 tag 时序，release workflow 校验通过），torchwood
  require v0.3.0、replace 已摘（A11 闭环）。
- ~~路径 B codemod~~ **已完成**：34 个业务 proto 迁 policy.proto 声明
 （protopolicy provider），legacyProvider/vendored authz.proto/legacypb/
  阶段 0 五档基线退役；manifest golden 保留为策略漂移 CI 基线（codemod
  等价性：树逐字节一致，36 个继承方法 label 归一）。
- ~~流式 authn 拦截器~~ **已完成**：StreamAuthMiddleware + 完整流式链
 （clientInfo → authn → authz），streaming 禁用解除。
- **lynx 深度集成（gRPC 面）已完成**：拦截器链经库 `AssembleWith` 装配、
  gRPC 面经 `server.New`（注册/断言统一），`GRPCAPIService` 按 lynx 语义
  自管 listener/排水/健康轮询。HTTP 面迁移（bufconn gateway + 自定义
  HTTP 面路由语义拆分）挂账 B18。
- 错误处理器 nolint（v0.2.x 垫片参数）随库 v1.0 移除垫片时清理。
