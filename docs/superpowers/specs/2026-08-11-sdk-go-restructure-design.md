# sdk/go 重构设计：Server API Client / Client API Client 拆分与 CLI 切换

日期：2026-08-11
状态：已获用户批准；经独立子代理审查后修订

## 背景与目标

`sdk/go` 当前是单 package `torchwood`，虽有 `Client`（Bearer JWT）与 `ServerClient`（x-api-key）之分，但缺少登录后 token 生命周期管理（自动刷新、持久化），且 CLI（`cmd/client`）直接依赖 genproto 与 grpc 拨号。

目标：

1. sdk/go 拆分为 `client` 与 `server` 两个子包，边界清晰。
2. Client API Client 支持账号密码登录（SignIn）、AccessToken 鉴权、自动 Refresh Token（主动刷新 + 401 重试）。
3. Server API Client 通过 API Key 认证，补齐全部 Server API 服务封装。
4. Torchwood CLI（`cmd/client` 包源码）切换到依赖 go sdk，**源码不再 import genproto / grpc / protobuf**。
5. SDK 内置 Token 持久化能力（内存 + JSON 文件），供 CLI 后续复用。

## 包结构

```
sdk/go/
├── go.mod                  # module github.com/torchwoodcloud/torchwood/sdk/go
├── client/                 # Client API Client（end-user 认证）
│   ├── client.go           # Client、Option、拨号、Close
│   ├── token.go            # TokenStore 接口、MemoryTokenStore、FileTokenStore
│   ├── auth.go             # 认证/自动刷新 unary interceptor
│   ├── account.go          # AccountService（SignUp/SignIn/RefreshToken/Me/SignOut）
│   ├── teams.go            # TeamsService
│   └── databases.go        # DatabasesService
├── server/                 # Server API Client（API Key 认证）
│   ├── client.go           # Client、Option、拨号、Close
│   ├── services.go         # Health/Users/Teams/Databases/Projects/Storage/Functions/OAuthProviders
│   ├── invoke.go           # InvokeJSON + 方法注册表
│   └── invoke_test.go      # 注册表完整性测试（protoreflect 校验）
└── internal/conn/          # 共享拨号逻辑（insecure + 自定义 DialOption）
```

- 根 package `torchwood`（现有 `torchwood.go`、`account.go`、`databases.go`、`teams.go`、`server.go`）删除，代码迁入对应子包；现有使用方只有 sdk 测试与文档，无兼容负担。
- **SDK 不封装自定义请求/响应类型**：所有类型化服务方法的签名、TokenStore 的 token 表示均直接使用 genproto 生成的 protobuf 类型（与 Google Cloud Go SDK 惯例一致）；genproto 仅出现在 SDK 内部，不泄漏给 CLI。

## client 包（Client API Client）

### 构造与配置

```go
c, err := client.New(target,
    client.WithProjectID("default"),
    client.WithDatabaseID("main"),
    client.WithTokenStore(store),              // 可选，默认 MemoryTokenStore
    client.WithOnTokensChanged(fn),            // 可选，token 变化回调
    client.WithInitialTokens(bundle),          // 可选，恢复已有会话
    client.WithDialOptions(...),               // 可选，透传 grpc.DialOption
)
```

- 认证 metadata：`authorization: Bearer <access_token>`。
- 未持有 token 时仅 `SignUp` / `SignIn` 等公开方法可用（服务端侧由 proto authz 注解控制）。
- `WithDatabaseID` 是**可选便利默认**：Client API 无数据库发现接口，database ID 由 Server API / Console 带外供给；初始化时不知道 ID 的场景不设该选项，改用 `c.UseDatabase(databaseID)` 晚绑定服务句柄。

### Token 管理

- 复用 proto 的 `clientv1.TokenBundle`（access_token / refresh_token / expires_at）作为 token 表示。
- `TokenStore` 接口：

```go
type TokenStore interface {
    Load() (*clientv1.TokenBundle, error)
    Save(*clientv1.TokenBundle) error
    Clear() error
}
```

- 内置实现：
  - `MemoryTokenStore`：进程内保存（内置 mutex）。
  - `FileTokenStore`：JSON 文件持久化，文件权限 0600，写入采用「临时文件 + rename」原子写；`Load` 在文件不存在时返回 `(nil, nil)`（视为无 token），`Clear` 在文件不存在时不报错；**内置 `sync.Mutex` 串行化 Load/Save/Clear**，供自动刷新 interceptor 在多 goroutine 下并发使用。
- `OnTokensChanged func(*clientv1.TokenBundle)` 回调：登录、刷新、清空时触发（清空时传 nil）。

### 自动刷新（unary interceptor）

对 `Client` 的所有 Client API 调用透明生效：

1. **主动刷新**：调用前若 `expires_at` 距现在不足 30 秒且持有 refresh token，先调用 `RefreshToken` 换新 TokenBundle 并保存。
2. **401 重试**：调用返回 `codes.Unauthenticated` 且持有 refresh token 时，刷新一次并重试原调用一次；仍失败则返回原错误。
3. **并发去重**：刷新用一把 mutex 串行化；等待锁后先检查 token 是否已被其他 goroutine 刷新过（比较 access token 值），是则直接复用。
4. `expires_at` 为 0（不可信）时跳过主动刷新，仅保留 401 被动重试。
5. `SignIn` / `SignUp` / `RefreshToken` 等公开方法不经过刷新逻辑，避免递归。
6. **刷新失败的清空策略**：仅当刷新 RPC 返回 `codes.Unauthenticated`（refresh token 失效/被撤销）时才 `TokenStore.Clear` + 触发回调（nil）；网络错误、服务端 5xx 等临时失败保留 token，原样返回错误。

### Account 服务行为

- `SignIn(email, password)` / `SignUp(...)`：仅当响应 `mfa_required == false` 且 `tokens.access_token` 非空时才 `TokenStore.Save` + 触发回调；`mfa_required=true` 时不保存任何 token，响应原样返回（MFA 挑战流程不在本期范围）。
- `SignOut`：**不经过自动刷新**（避免为登出而先刷出新会话）；调用成功或返回 `codes.Unauthenticated`（token 已失效，会话本就不可用）时都 `TokenStore.Clear` + 触发回调（nil）；其他错误（网络等）不清本地 token，返回原错误。
- `RefreshToken` 保留为公开方法，供手动刷新；成功后同样 Save + 回调。

## server 包（Server API Client）

### 构造与认证

```go
c, err := server.New(target,
    server.WithAPIKey("..."),
    server.WithProjectID("..."),   // 可选，x-torchwood-project 头
    server.WithDialOptions(...),
)
```

- unary interceptor 注入 `x-api-key`，配置 ProjectID 时附加 `x-torchwood-project`。
- 无 token 状态，无刷新逻辑。`HealthService` 注解为 `ACCESS_PUBLIC`，可不配置 API Key 调用。

### 服务覆盖

封装 `proto/server/v1` 全部 9 个 service 中的 8 个（**除 `APIKeysService`**）：`Health`、`Users`、`Teams`、`Databases`、`Projects`、`Storage`、`Functions`、`OAuthProviders`。

注：`APIKeysService` 的 proto 注解同样是 `ACCESS_API_KEY`，但服务端拦截器（`pkg/grpc/interceptor/jwt.go`）对 API Key 凭证调用 APIKeys 方法做了额外拒绝（仅 admin console session 可用），因此 Server API Client 封装它无意义；与现 CLI registry 的排除范围一致。

`proto/server/v1` 全部为 unary 方法（无 streaming），InvokeJSON 的 unary 假设成立。

### JSON 调用层（InvokeJSON）

为 CLI 等通用工具提供「方法名 + JSON → JSON」能力：

```go
respJSON, err := c.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/CreateUser", reqJSON)
```

- **动态分发实现**（替代手写注册表）：从 `protoregistry.GlobalFiles` 按 full method name 查找 MethodDescriptor，限定 `torchwood.server.v1` 包内且不属于 `APIKeysService`；用 `dynamicpb` 构造请求/响应消息，`conn.Invoke` 发起调用。proto 新增方法自动获得支持，完整性由构造保证，无需维护注册表。
- protojson 解码请求（`DiscardUnknown: false`，未知字段直接报错）、调用、protojson 编码响应（`Multiline: true, Indent: "  "`，不输出零值字段——与 CLI 现有 `protoJSONMarshaler` 输出格式逐字节一致）。
- genproto / protojson 仅存在于 SDK 内部，调用方零接触。
- 错误形态（供 CLI 与测试断言）：
  - 未知方法名：`torchwood: unknown method "<method>"`（非 status 错误）。
  - 非法请求 JSON：protojson 原始错误原样返回（包含字段名与原因）。
  - RPC 错误：gRPC status 错误原样返回，CLI 侧沿用 `formatRPCError` 格式化。

### 完整性测试

`invoke_test.go` 用 protoreflect 遍历 `serverv1` FileDescriptor 中全部 service/method（排除 `APIKeysService`），断言每个方法都能被 InvokeJSON 解析并接受空 JSON `{}` 构造出请求——动态分发下完整性由构造保证，该测试防回归（如包名白名单被改坏）。此测试接管现 `cmd/client` registry 完整性测试的职责。

## CLI 切换（cmd/client/）

**module 关系**：`cmd/client` 不是独立 module，属于根 module `github.com/torchwoodcloud/torchwood`。因此：

- 根 `go.mod` 新增 `require github.com/torchwoodcloud/torchwood/sdk/go` 与 `replace github.com/torchwoodcloud/torchwood/sdk/go => ./sdk/go`。
- 目标是 **`cmd/client` 包源码不再 import genproto / grpc / protobuf**（用测试或 CI grep 兜底）；根 module 的其他包（`internal/`、`cmd/server` 等）继续直接使用 genproto/grpc，根 `go.mod` 的这些依赖保留。

**代码改动**：

- `conn.go`、`registry.go` 删除：连接与认证改为 `server.New(...)`；rpcEntry 注册表由 SDK 的 InvokeJSON 动态分发接管。
- **所有命令改为构造请求 JSON 而非 genproto 结构体**：
  - `cmd_rpc.go`（通用逃生舱）：`--data` JSON 直接透传给 `InvokeJSON`。
  - `cmd_users.go`、`cmd_databases.go`、`cmd_teams.go`、`cmd_projects.go`、`cmd_storage.go`、`cmd_functions.go`、`cmd_oauth.go`、`cmd_health.go` 等 flag 命令：用 `map[string]any` 构造 protojson（camelCase 键），proto3 optional presence 用「键存在/不存在」表达（替代 `changedBoolPtr`/`changedInt32Ptr`/`changedStringPtr` 的指针语义）；`--data` 合并语义改为「先 flag 生成 map，再把 --data JSON 解码为 map 覆盖合并」（与 --data 优先的现语义一致）。
  - **int64 精度**：所有 JSON 解码一律用 `json.Decoder.UseNumber()`（数字存为 `json.Number`，marshal 时原样输出，不经过 float64）；`UpdateDocumentRequest.increment`（`map<string, int64>`）等 64 位整型字段继续由 helper 解析为 `map[string]json.Number` 或字符串，禁止 float64 中转。
  - **bytes 字段**：protojson 要求 base64 字符串。`functions create deployment` 的 `--code <zip-file>` 等文件类 flag 保留现有体验：CLI 读取文件后 base64 编码放入 map，不让用户手写 base64。
  - protojson 解码接受 64 位整型的数字或字符串形式、枚举的名字字符串，其余 flag 取值均可直接表达。
  - `cmd_helpers.go` 的 `jsonStringList`/`jsonStringMap`/`jsonInt64Map`/`structData` 与 `cmd_projects.go` 的 `buildListRequest` 全部改为 JSON map 版本。
- 输出：`InvokeJSON` 返回的 JSON 字节原样写 stdout（格式与现状一致）；`formatRPCError` 的 code + message + scope 提示逻辑保留，输入改为 `InvokeJSON` 返回的 status 错误。
- flags（`--endpoint`、`--api-key`、`--timeout`、`--output`、`--tls`）、`health` 免 API Key 逻辑、全部命令的 UX 不变。
- 现有各 `cmd_*_test.go` 按新实现适配（构造 JSON 断言替代结构体断言）；registry 完整性测试移入 SDK（见上）。

## internal/conn 包

- `Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error)`：target 为空报错；默认 `insecure.NewCredentials()` 在前，调用方 DialOption 在后（与现状一致）。
- 供 `client` 与 `server` 两包复用。

## 错误处理

- SDK 方法原样返回 gRPC status 错误，调用方用 `status.Code` 判别（保持现状）。
- `FileTokenStore` I/O 错误包装为 `fmt.Errorf("torchwood: ...: %w")`。
- 刷新失败的清空策略见「自动刷新」第 6 条。

## 测试

- 沿用 bufconn 内存 fake 服务风格（现有 `*_test.go` 模式），无需外部依赖。
- client 包：
  - interceptor 主动刷新：fake Account 服务返回过期 `expires_at`，断言调用前触发 RefreshToken。
  - 401 重试：首次 Unauthenticated → 刷新 → 重试成功；刷新返回 Unauthenticated 时清空 token 并返回该错误；刷新返回临时错误（如 Unavailable）时保留 token。
  - 并发去重：多 goroutine 同时触发，断言 RefreshToken 只被调用一次。
  - `FileTokenStore`：保存/加载往返、权限位、损坏文件、Clear 幂等、并发 Load/Save。
  - SignIn：成功自动落 token + 回调；`mfa_required=true` 时不落 token。SignOut：成功与 Unauthenticated 都清空；网络错误不清空。
- server 包：
  - InvokeJSON 正常往返、未知方法名报错（`torchwood: unknown method`）、非法 JSON 报错。
  - 注册表完整性（protoreflect 遍历校验）。
  - `x-api-key` / `x-torchwood-project` 头注入。
- CLI：
  - `go test ./cmd/client/...` 适配后通过。
  - int64 精度回归：`increment` 大数（>2^53）经 JSON 路径后值不变。
  - 兜底测试/脚本：断言 `cmd/client` 源码不出现 `genproto`、`google.golang.org/grpc`、`google.golang.org/protobuf` import。

## 影响面与文档

- 根 `go.mod`：新增 sdk/go 的 require + replace；`task build`（编译 `./cmd/client`）与 `task test`（含 `test-sdk-go`）保持可用，实现后需实际运行验证。
- `docs/developer/12-sdk.md`：更新 Go SDK 部分（包结构、token 管理、InvokeJSON、CLI 依赖关系）。
- `AGENTS.md`：`cmd/client/` 条目更新为「通过 sdk/go 调用 Server API」。
- `sdk/README.md`：Go SDK 快速开始更新。
- 不涉及：wire、`tests/acceptance`（未引用 sdk/go 与 cmd/client 内部）、`sdk/demo` 与 `sdk/typescript`（TypeScript 侧）、`Taskfile.yml` 任务定义本身。

## 非目标（YAGNI）

- client 包不提供 InvokeJSON（当前无使用者）。
- 不封装 `APIKeysService`（API Key 凭证无法调用）。
- 不处理 MFA 挑战流程（`mfa_required=true` 时原样透传响应，不落 token）。
- 不支持 TLS 拨号（与现状一致，DialOption 留给调用方）。
- CLI 不新增 Client API（end-user）命令。
