# 03 · API 表面

> 日期：2026-08-22  
> 切片：Client / Server / Console proto、gRPC handler、自定义 HTTP、SDK、CLI、鉴权表、Realtime、Console 实际调用。  
> 方法：代码即事实；不引用既有拍板。对比对象是 Firebase / Supabase / Appwrite：**给 SaaS 端用户的 Client SDK**，和 **给 SaaS 后端的 Server/Admin SDK**。Agent 工具目录不在本切片。  
> 词汇：模块、接口、深度、缝、适配器。

每条发现先写**事实**（可对源码），再写**判断**（在事实成立时，从 SaaS BaaS 最优接口给出的结论）。

---

## 0. 总判

Client vs Server 作为**凭证与特权缝**是对的：端用户拿 JWT 做「我自己」；构建方拿 API Key / admin 会话做「这个项目」。Firebase Auth vs Admin、Supabase anon+JWT vs service role，切的也是这条缝。

当前实现把这条缝落成了 **两套 protobuf 资源目录、两棵 REST 树、一份 Go 方法级鉴权表、再加一截不进 proto 的裸 HTTP**。经济子系统已经演示正确形状——同一领域、不同动词、共享用例。文档和组则把 CRUD 克隆了一遍。gRPC handler 几乎全是 1:1 浅映射：作为传输适配器这是对的，但产品接口本身是动词清单，不是深模块。

RPC 数量不是约束。下面按形状评，不按条目数护盘。

---

## 1. 现状

三套 gRPC 包挂在同一进程、同一 gateway mux（`internal/infra/server/grpc.go:133-153`，`grpc_gateway.go:56-78`）。HTTP 路径按包名前缀切开：

| 表面 | proto 包 | 默认 `service_auth` | REST 前缀 | 凭证 |
|---|---|---|---|---|
| Client | `proto/client/v1/` 6 个服务 | `ACCESS_AUTHENTICATED` | `/v1/...`（无 `server`） | 端用户 JWT；部分读 `ACCESS_PUBLIC` |
| Server | `proto/server/v1/` 13 个服务 | `ACCESS_API_KEY` | `/v1/server/...` | API Key **或** Console admin 会话 + `X-Torchwood-Project` |
| Console | `proto/console/v1/` 2 个服务 | Auth=`PUBLIC`，Admins=`PERMISSION` | `/v1/console/...` | 浏览器 HttpOnly cookie |

unary RPC 计数（各 `.proto` 的 `rpc` 声明，不含自定义 HTTP）：

| 包 | 服务 | RPC |
|---|---|---|
| Client | Account 35、Databases 7、Groups 8、Payments 4、Assets 3、Subscriptions 4 | **61** |
| Server | Databases 22、Functions 16、Assets 13、Groups 12、Storage 12、Users 9、Subscriptions 9、Projects 5、APIKeys 4、Payments 4、OAuth 3、Billing 3、Health 2 | **114** |
| Console | Auth 5、Admins 5 | **10** |
| 合计 | 21 个服务 | **185** |

proto 之外的产品入口（`serverhttp` + `realtime`，挂在同一 HTTP 端口）：

| 入口 | 路径 | 不走 gRPC 拦截器链 |
|---|---|---|
| 文件 multipart / 分片 / 下载 / 预览 | `/v1/storage/buckets/...`（**没有** `/server`） | 是 |
| Functions 代码包 | `POST /v1/server/functions/{id}/deployments/code` | 是 |
| OAuth 浏览器回调 | `GET /v1/account/oauth2/{provider}/callback` | 是 |
| 支付渠道 webhook（入站） | `POST /v1/payments/callbacks/{provider}` | 是 |
| Realtime WebSocket | `GET /v1/realtime` | 是 |

调用方适配器也是三份：Go Server SDK 走 gRPC（`sdk/go/server`）；TypeScript 走 REST（`sdk/typescript/src/http.ts`）；CLI 只打 Server gRPC（`cmd/client/cmd/root.go:46-50`）。Console 不 import SDK，自己用 axios 打 `/v1/server/*` 与 `/v1/console/*`（`console/src/api/client.ts:11-16`）。

同一资源在 Client / Server 上的重叠（消息各写一份，`shared.v1.Document` 是少数已合并的载荷）：

| 资源 | Client 动词 | Server 动词 | 用例层 |
|---|---|---|---|
| Document | 7 个 CRUD（无 schema DDL、无 bulk） | 同 7 个 + DDL 13 + bulk 2 | 共用 `documents.Documents`，包装层投影特权 |
| Group | 8 个（无 prefs / GetMembership / UpdateMembership） | 同 8 个 + 4 个管理动词 | Client 包一层 owner 策略，调 `server.Groups` |
| Payments | 建单 / 本人查询 / 验票 | 全量查询 / 退款 / 人工履约 | 动词不同 |
| Assets | 只读 3 个 | 目录 CRUD + 五动词 + 对账 | 动词不同 |
| Subscriptions | ListPlans / Subscribe / me / Cancel | 计划 CRUD + 强制取消/过期 | 动词不同 |
| Account vs Users | 登录与自我服务 35 | 用户接管 9（含 `CreateUserToken`） | 身份方向不同 |
| Storage / Functions / Projects / APIKeys / Billing / OAuth | **无 Client proto** | 仅 Server | — |

鉴权有三层声明，不全在 proto 里：

1. `shared.v1.MethodAuth` / `ServiceAuth`（`proto/shared/v1/authz.proto:9-32`），启动期 `collectMethodsByAccess` 扫一遍（`internal/infra/server/grpc.go:212-246`）。
2. `apiKeyScopeRules`：**112** 条方法 → `{resource, read|write}`（`pkg/grpc/interceptor/apikey_scope.go:25-150`；Health 2 个 `ACCESS_PUBLIC` 不入表）。
3. `adminRoleMethodRules`：**65** 条写方法 → `member|owner|admin`（`pkg/grpc/interceptor/admin_roles.go:16-100`）。

2 与 3 启动期 fail-closed 对齐 proto（`grpc.go:90-96`）。自定义 HTTP **不**走这两张表的拦截器，只在 `serverhttp.httpAuth` 里抽查 API Key scope（`internal/api/serverhttp/auth.go:30-53`），没有 admin 角色表。

---

## 2. 设计问题

### AS-1 · Client vs Server 缝切对了；Document / Group 是克隆 CRUD

**事实。** 拦截器把 Server 方法收进 `apiKeyMethods`：必须是 API Key 或 admin（`pkg/grpc/interceptor/jwt.go:110-114`）。Client 的 `ACCESS_AUTHENTICATED` 在 `collectMethodsByAccess` 里被改写成 `permissionMethods`，缺省权限串 `"users"`（`grpc.go:232-236`）；API Key 调用这类方法直接 `PermissionDenied`（`jwt.go:147-149`）。端用户 JWT 的角色由 `resolveEndUserRoles` 补上 `users` + `user:{id}`（`internal/infra/auth/validator.go:267-268`）。所以运行时缝是：**端用户走 Client，构建方走 Server**。

文档 RPC 却是两份几乎同构的服务。Client `CreateDocumentRequest`（`proto/client/v1/databases.proto:130-138`）与 Server 同名消息（`proto/server/v1/databases.proto:291-300`）字段同序；REST 只差 `/v1` vs `/v1/server`（Client `65-67` 行，Server `115-116` 行）。handler 两边都是：解 Struct → `parseOptionalPermissions` → 调 use-case → `mapDocument`（`internal/api/clientgrpc/databases.go:28-46`，`internal/api/servergrpc/databases.go:306-337`）。深度在 `internal/app/documents`：两边共用 OCC / grant / 查询核，差别只有 `WriteOptions.AllowPrivilegedGrant`（`internal/app/documents/documents.go:16-21,37-59`）。

组更明显：`internal/app/client/groups.go:16-23` 的 `Groups` 里嵌的就是 `*server.Groups`；Client 只加「只有 owner 能删组」这类策略（`58-70`）。proto 仍复制了 `Group` / `Membership` 消息（Client `proto/client/v1/groups.proto:106-126`，Server `proto/server/v1/groups.proto:133-154`，Server 多一个 `permissions`）。

TypeScript 把克隆抬到 SDK：`this.databases.createDocument` 打 `/v1/databases/...`（`sdk/typescript/src/client/databases.ts:22-24`），`this.server.databases.createDocument` 打 `/v1/server/databases/...`（`sdk/typescript/src/server/databases.ts:176-188`，`auth: "apiKey"`）。调用方要选对树，而不是选对身份。

**判断。** SaaS BaaS 需要的缝是身份，不是资源副本。Firestore / PostgREST 是**一套文档接口 + 按 principal 的规则**；Admin SDK 提高特权，不另开 `AdminCollectionService.CreateDocument`。Account vs Users、Payments 建单 vs 退款、Assets 只读 vs 五动词——那些是不同意图，该分。Document / Group 的意图相同，ACL 已经按 principal 执行，再克隆 RPC 是 Appwrite 式目录税：改字段、改 OCC、改查询 AST 都要改两份 proto、两份 handler、两份 SDK。正确落点是一个资源模块，缝在 principal 适配器，而不是两套产品接口。

---

### AS-2 · gRPC handler 是 1:1 浅映射；产品接口因此也浅

**事实。** 抽样三条热路径，结构相同：

- Account `SignUp`：拼 `SignUpCommand` → 调 use-case → `mapUser` / `mapTokens`（`internal/api/clientgrpc/account.go:29-45`）。35 个 Account RPC 几乎全是这个形状。
- Server `CreateProject`：三个 getter 进 command，再 `mapProject`（`internal/api/servergrpc/projects.go:26-35`）。
- Server `Grant`：proto 字段搬进 `GrantCommand`（含 `IdempotencyKey`），再 `mapOpResult`（`internal/api/servergrpc/assets.go:160-191`）。

handler 里偶尔有的「逻辑」是传输家务：`projectID` 从 principal 取出、`WithAuditResource`、`BindListQuery`、cookie `Set-Cookie`（`internal/api/consolegrpc/cookies.go:34-40`）。没有编排、没有跨资源事务、没有把多个 CRUD 收成一个领域动词。

**判断。** 浅 handler 作为适配器没问题——深度应该在领域接口后面。问题是 **proto 服务就是 CRUD 动词表**，适配器再 1:1 映过去，调用方看到的接口没有杠杆：创建集合、加字段、建索引、写文档各是独立 RPC，调用方必须知道顺序与不变式。对比：Firestore 的接口是「文档 + 查询 + 监听」，schema 不是 13 个 DDL RPC；Stripe 的接口是「PaymentIntent」，不是 List/Get/Patch 订单字段。经济五动词（Grant/Consume/Transfer/Mutate/Expire）才是深接口；Databases 22 个 RPC 不是。handler 浅是症状，产品表面浅是病。

---

### AS-3 · 自定义 HTTP 与 gRPC 是两个产品接口，不是一个适配器

**事实。** Gateway 先注册全部 gRPC-HTTP 反代，再挂四类裸 handler（`internal/infra/server/grpc_gateway.go:85-89`），Realtime 在 mux 外按路径分发（`101-104`）。

存储把裂缝暴露得最清楚：

- 元数据 RPC：`POST /v1/server/storage/buckets/{bucket_id}/files`（`proto/server/v1/storage.proto:80-82`），走 gRPC 拦截器（authz / 角色 / 审计 / 限流 / usage）。
- 真上传：`POST /v1/storage/buckets/{bucketId}/files`（`internal/api/serverhttp/file_handler.go:109-119`），**丢掉 `/server`**。TS SDK `uploadFile` 打这条裸路径，`createFileToken` 打 `/v1/server/...`（`sdk/typescript/src/server/storage.ts:88-120`）。同一 SDK 方法族跨两棵树。
- `CreateFile` use-case **没有** `RequireServerWriteActor`（`internal/app/storage/storage.go:212-257`）；桶 CRUD 才有（`64-69`）。裸 HTTP `authorize` 只对 API Key 查 scope、对 admin 绑项目（`serverhttp/auth.go:30-53`），**不查** `adminRoleMethodRules`。端用户 JWT 只要通过 `Authenticate`，就可以打这条「Server 才有的」上传面。
- 下载还接受 `?token=` 与 public bucket 匿名读（`file_handler.go:519-553`），这是第三条读路径。

Functions 代码包倒是留在 `/v1/server/...`（`internal/api/serverhttp/functions_handler.go:92-94`），与存储前缀策略不一致。支付回调必须裸 HTTP（原始 body 验签，`payments_handler.go:1-40`）——这是真适配器。OAuth 回调是浏览器 302（`oauth_handler.go:36-74`），也是真适配器。

这些路径都不进 `AuditInterceptor`（只挂 unary gRPC，`grpc.go:121-126`）。审计对构建方是「gRPC 方法名日志」，文件上传只有 slog（`file_handler.go:83-106`）。

**判断。** 二进制、重定向、渠道 webhook、WebSocket 不能塞进 protobuf unary，需要适配器。适配器应该实现**同一产品接口**（同一路径前缀、同一鉴权词汇、同一审计）。现在是第二套接口：路径命名空间分裂、鉴权表覆盖不全、审计丢失。调用方无法从 OpenAPI/proto 发现分片上传或下载；TS SDK 靠手写路径把两套缝起来。对 SaaS 构建方，这不是「存储有 HTTP 传输」，而是「文档里的 Storage API 不是实际上传的那个 API」。

---

### AS-4 · scope / 角色表是第二份 API 规格

**事实。** proto 能表达的只有 `AccessLevel` + `repeated string permissions`（`proto/shared/v1/authz.proto:17-20`）。`permissions` 实际混用三种字符串：Console 角色（`owner`/`admin`/`member`/`viewer`/`console`，`proto/console/v1/admins.proto:64-101`）、端用户标签 `"users"`（Account 若干方法，`proto/client/v1/account.proto:86-88`）、以及 `ACCESS_AUTHENTICATED` 被收集器填进去的默认 `"users"`（`grpc.go:232-236`）。

API Key 的资源范围与 admin 写角色不在 proto 里。新增一个 Server 写 RPC 的真实清单是：proto `method_auth`（或服务默认）→ `apiKeyScopeRules` 一行 → `adminRoleMethodRules` 一行 → 可能再加 `RequireServerWriteActor` / `RequirePlatformAdmin`（`internal/app/shared/authz.go:18-56`）→ SDK → 若 Console 用到再手写 axios。启动期断言保证「表与 proto 方法集」一致，**不**保证 scope 名、角色宽严、use-case 守卫三者一致。Functions DDL 用 `RequireServerWriteActor`（API Key 可做），部分平台操作用 `RequirePlatformAdmin`（API Key 不可做）——同一 Server 表面，第三套口令。

**判断。** 鉴权是接口的一部分：调用方必须知道 Key 的 `economy.write` 能不能 `Grant`、viewer 会话能不能 `CreateDocument`。把这些写在与 proto 平行的 Go map 里，等于产品规格有两份源。fail-closed 是工程上的好保险，不是深模块：深度会是「方法注解即唯一规格，拦截器只解释注解」。现在注解只区分 public / user-tag / api-key 三档，细规格在旁边。自定义 HTTP 连这份旁边的规格都没有（AS-3），第二份规格还是残的。

---

### AS-5 · Console 该是 Server API 的客户端；现在是第三协议 + 手写副本

**事实。** 资源操作 Console 打的就是 Server REST：`/server/users`（`console/src/api/users.ts:39-40`）、`/server/databases`（`console/src/api/databases.ts:80-81`）、`/server/payments/orders`（`console/src/api/payments.ts:22-24`），靠 cookie + `X-Torchwood-Project`（`console/src/api/client.ts:62-67`）。这与拦截器「admin 可调 ACCESS_API_KEY 方法」（`jwt.go:110-114,128-141`）一致。

但 Console **不**用 `sdk/typescript`。`console/src/api/*.ts` 重新声明 `User` / `Document` / `PaymentOrder` 等类型，路径手写。Realtime 再写一个 WS 客户端（`console/src/api/realtime.ts:1-57`），与 SDK `sdk/typescript/src/client/realtime.ts` 协议相同、实现各一份。

真正独有的 Console 协议只有构建方身份：`ConsoleAuthService` 的 cookie 会话与 bootstrap（`proto/console/v1/auth.proto:52-77`），`AdminsService` 的 owner 管员（`admins.proto:61-102`）。cookie 下发写在 gRPC handler 里（`consolegrpc/cookies.go:34-40`），不是领域接口。

**判断。** 构建方控制台在 BaaS 里有两种正当身份：（1）**平台操作员登录**——与端用户 Account 不是同一模块，单独一条 ConsoleAuth 合理；（2）**项目资源管理**——应只是 Server API 的一种适配器（Firebase Console 也是 Admin 表面的 UI）。现在（2）绕开 SDK 另维护一套 REST 客户端，类型与路径漂移没有编译器帮忙。CLI 倒是认清了这一点：只包装 Server，漏网用 `InvokeJSON`（`cmd/client/cmd/rpc.go:9-12`，`sdk/go/server/invoke.go:17-21`）。Console 没有等价的「只有一个 Server 客户端」。

---

### AS-6 · `ACCESS_AUTHENTICATED` 名不副实，鉴权词汇碰撞

**事实。** 枚举字面是「已认证即可」（`authz.proto:9-15`）。收集器却把它送进 `permissionMethods`，默认 `["users"]`（`grpc.go:232-236`）。拦截器对 `permissionMethods` 拒绝 API Key，并要求 `HasAnyRole`（`jwt.go:144-154`）。Admin 角色是 `{owner|admin|member|viewer}` + `console`（`internal/infra/auth/validator.go:166`），没有 `"users"`，所以 admin 会话也调不了 Client 的「已认证」方法——必须走 Server 树。`"users"` 同时是：端用户角色标签、系统集合名、API Key scope 资源名、Account 方法的 `permissions: ["users"]`。

**判断。** 接口用词应该把调用方必须知道的事说清楚。构建方读 proto 会以为「带任意有效凭证就能调 Client Databases」；运行时要的是「端用户角色 `users`」。四种「users」碰撞，是把文档 ACL、拦截器门禁、Key scope 塞进同一个字符串口袋。缝的位置（Client=端用户，Server=构建方）被这套词汇弄糊，而不是被模块边界弄清。

---

### AS-7 · Realtime 拟合 BaaS 听接口，但落在协议外面

**事实。** `GET /v1/realtime` 是 JSON 帧 WebSocket，不在任何 `.proto` 里（`internal/api/realtime/handler.go:1-6,104-105`）。频道只有 `databases.{db}.collections.{coll}[.documents.{id}]` 与 `accounts.{userId}`（`channels.go:12-35,50-77`）。握手明确拒绝 API Key（`handler.go:333-370`）；Go 注释写明 Server SDK 不提供 Realtime（`sdk/go/client/realtime.go:8`）。事件来自文档写 outbox 信封（`internal/domain/events/envelope.go:12-21,33-35`），WS 出站剥掉 ACL。配额、ping、JWT 到期断开都在这个包内，是完整的听协议。Console 试听与 Client SDK 各实现一遍（AS-5）。

**判断。** 对 SaaS BaaS，Realtime 是 Client 表面该有的深度：端用户订阅「我能读的文档」，而不是轮询 ListDocuments。它与文档 ACL 同一套 principal，形状对。问题是它是**第三个传输产品**：无 protobuf/OpenAPI、无版本字段、构建方（API Key）不能作为服务身份订阅（Firebase Admin 可以挂监听）。文档事件已经有内部信封，却只接到 WS，接不到构建方后端（见 AS-8）。作为模块它够深；作为产品接口它是暗门。

---

## 3. 能力缺失

对照 Firebase / Supabase / Appwrite，构建方把 Torchwood 嵌进自己的 SaaS 时，产品接口还缺这些面。不是「RPC 不够多」。

### AS-8 · 没有出站 webhook 到构建方后端

**事实。** 入站只有支付渠道：`POST /v1/payments/callbacks/{provider}`（`payments_handler.go:37-40`）。proto / Server 服务 / Console 路由都没有 Webhook CRUD。`audit.Repository` 只有 `Insert` 与 `ListByActor`（`internal/domain/audit/audit.go:22-26`）；文档写会进 outbox 信封（`events/envelope.go:12-21`），消费者是 Realtime Hub，不是 HTTP 投递。Account `CreateJWT` 的 SDK 注释写「用于服务端安全回调/Webhook」（`sdk/typescript/src/client/account.ts:433-435`），那是端用户短 JWT，不是平台出站。

**判断。** SaaS 构建方的后端才是权威业务系统。BaaS 必须能把「用户注册了、文档变了、支付完成了」推到构建方 URL（签名、重试、死信）。现在构建方只能：Realtime（仅浏览器/端用户 JWT）、轮询 Server List*、或自己改数据库。支付完成了履约资产，但构建方订单系统收不到第一方事件。这是 BaaS 嵌进 SaaS 的主缝，不是附加功能。

---

### AS-9 · 没有环境；Project 同时当租户和部署单元

**事实。** proto 无 Environment 服务、无 `environment_id` 字段。租户单元是 `ProjectsService` 五动词（`proto/server/v1/projects.proto:57-76`）。Client 请求用 `project_id` 或 `X-Torchwood-Project`。Realtime hello 也绑 `project_id`（`realtime/handler.go:144-148`）。

**判断。** 构建方 SaaS 需要 dev / staging / prod 隔离（数据、Key、OAuth 回调、支付渠道）。Firebase 用多项目假装环境，Supabase 用 branch，Appwrite 用 project+region。Torchwood 只有 Project：复制项目等于复制租户，没有「同一应用的一条环境缝」。接口上把「租户」和「部署」焊死了。

---

### AS-10 · 版本只是路径上的 `v1`，没有版本策略

**事实。** 包名 `client.v1` / `server.v1` / `console.v1` / `shared.v1`，HTTP 一律 `/v1/...`。没有 `/v2`、没有 sunset 头、没有协商。破坏性变更靠 `reserved` 字段号（例如 Console `SignUpResponse` reserved 4，`auth.proto:122-123`）。每个 proto 文件复制同一段 swagger `security_definitions`（见任意 client proto 文件头约 18-58 行），生成互不合并的 `*.swagger.json`。TS SDK 与 Console 手写路径字符串，没有从 proto 生成客户端。

**判断。** 嵌进别人生产环境的 BaaS，版本是接口的一部分：何时弃用 List 的 string queries、何时去掉 guest `project_id`、双栈 Query AST 何时收掉（`proto/client/v1/databases.proto:175-185` 仍 dual-stack）。现在 `v1` 只是目录名。构建方无法依赖「旧客户端还能活一个窗口」。这不是要冻结这 185 个 RPC；是要承认表面会变，并把变化做成可观察的接口。

---

### AS-11 · 写路径幂等与构建方审计都不是产品接口

**事实。** `idempotency_key` 只出现在经济相关消息：Client `CreateOrder` / `Subscribe`（`proto/client/v1/payments.proto:93`，`subscriptions.proto:138`），Server 资产五动词与退款（`proto/server/v1/assets.proto:190,203,215,225,232,242`，`payments.proto:96`）。Document / User / Group / Storage / Functions 写请求没有该字段，也没有 `Idempotency-Key` 头。

审计：所有 unary gRPC 成功或失败都 `Insert`（`pkg/grpc/interceptor/audit.go:33-79`），`action` 是全方法名。产品读接口只有端用户 `Account.ListLogs` → `ListByActor`（`internal/app/client/logs.go:16-32`，`audit.go:24-25`）。Server / Console 没有 `ListAuditLogs`。自定义 HTTP 不写 `audit_logs`（AS-3）。

**判断。** 构建方后端会重试。没有幂等键，CreateDocument / CreateUser / 上传在超时后是未定义行为；经济子系统已经证明这个接口该长什么样。审计日志若不能按项目查询，就只是运维表，不是给构建方的合规/调试 API。Firebase/Appwrite 都把项目活动日志放在 Admin 表面。现在深度停在拦截器，没露出产品接口。

---

### AS-12 · Client 没有 Storage / Functions 产品面；上传是漏出来的缝

**事实。** `proto/client/v1/` 无 storage、无 functions。TS Client 对象有 account/databases/groups/realtime/payments/assets/subscriptions，没有 `storage` / `functions`（`sdk/typescript/src/torchwood.ts:42-49`）。Functions `CreateExecution` 要求 `RequireServerWriteActor`（`internal/app/functions/executions.go:67-70`），端用户不能调。存储上传却经裸 HTTP 接受任意通过 `Authenticate` 的主体（AS-3），SDK 只包装了 `auth: "apiKey"` 的 `uploadFile`（`sdk/typescript/src/server/storage.ts:108-120`）。

Server `CreateUserToken`（`proto/server/v1/users.proto:93-97`）能签发端用户令牌——这是 impersonation，不是「以用户身份调函数」的产品动词。

**判断。** SaaS 端用户要传头像、下文件、调「以我的身份跑的函数」。Firebase Storage / Callable Functions、Supabase Storage / Edge Functions 都在 Client 表面，规则按用户。Torchwood 把这两块标成 Server-only，同时又在 `/v1/storage` 留下未声明的端用户写路径。结果是：正式接口缺能力，非正式接口越权形状。该有的是带文档 ACL 语义的 Client Storage，以及显式的「用户可调 / 仅构建方可调」函数入口，而不是靠路径漏缝。

---

## 4. 已够深

这些形状已经对，还债时不要推翻。

### AS-13 · 经济、自我服务身份、组策略包装、proto 门禁

**经济是真的 Client/Server 缝。** Client Assets 三个只读 RPC，注释写明端用户无写入口（`proto/client/v1/assets.proto:58-71`）。Server 才有 Grant/Consume/Transfer/Mutate/Expire，并且带 `idempotency_key`（`internal/api/servergrpc/assets.go:160-191`）。Payments / Subscriptions 同样是「端用户下单/订阅」对「构建方退款/强制作废」，不是 List 的克隆。handler 浅，领域动词深——这才是 AS-1 该抄的样板。

**Account vs Users 方向对。** 35 个 Account RPC 是登录方式目录（密码 / OTP / OAuth / 微信 / 匿名 / Magic URL / MFA），对 BaaS 端用户这是正当宽度，不是该砍的 CRUD。Users 是构建方接管（禁用户、删会话、签发 token）。两边不该合并成一个 `PersonService`。

**Groups 用例层已经是「一个核 + Client 策略适配器」**（`internal/app/client/groups.go:16-23,58-70`）。缺的是把这个缝抬到产品接口，而不是再复制一份 proto 消息。

**文档核共用**（`internal/app/documents/documents.go:23-32`）同理：特权差是 `WriteOptions`，不是两套存储。

**proto `method_auth` + 启动期 fail-closed**（`collectMethodsByAccess`、`AssertAPIKeyScopeCoverage`、`AssertAdminRoleWriteCoverage`、`assertRegisteredMethodsHaveAuthz`）是正确的拦截器模块。该做的是把 AS-4 的第二份规格收进注解，而不是拆掉断言。

**支付入站回调、OAuth 302、文件字节、WS 听** 这些传输适配器的存在理由成立。该收的是它们与 gRPC 表面的路径/鉴权/审计对齐（AS-3、AS-7），不是改回全部 unary。

---

## 5. 对照表（给后续切片）

| 问题 | 当前落点 | SaaS BaaS 该落的缝 |
|---|---|---|
| 端用户 vs 构建方 | 两套 proto 包 + 两棵 URL 树 | 同一资源接口，principal 适配器；Account/Users 与经济动词保持分面 |
| 传输 | gRPC unary + 平行裸 HTTP + JSON WS | 一个产品接口，多种适配器 |
| 鉴权规格 | proto 三档 + 112 scope + 65 角色 + use-case 守卫 | 方法注解即唯一规格 |
| Console | 独立 Auth/Admins + 手写 Server REST | 操作员身份独立；资源 UI 只消费 Server SDK |
| 构建方集成 | 无出站 webhook；审计不可查；无环境 | 事件出站、项目审计、环境作为一等接口 |
| Realtime | 深，但协议外、仅 JWT | Client 听接口（可进规格）；事件同时可投递到构建方 |

不把「维持这 185 个 unary」当成目标。该瘦的是 Document/Group 的克隆表面；该深的是事件、环境、幂等、Client 文件与函数；该保住的是经济动词、Account 自我服务、以及「拦截器 fail-closed」这个模块。
