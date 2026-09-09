# sdk/go 重构实现计划：Server API Client / Client API Client 拆分与 CLI 切换

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 sdk/go 拆分为 `client`（end-user，自动刷新 token）与 `server`（API Key，InvokeJSON 动态分发）两个子包，并把 CLI（cmd/client）切换为只依赖 sdk/go。

**Architecture:** SDK 内部直连 genproto；server 包的 InvokeJSON 用 protoregistry + dynamicpb 动态分发，CLI 只传 JSON 字符串；client 包用 unary interceptor 实现主动刷新 + 401 重试 + mutex 去重，TokenStore 接口配内存/文件实现。

**Tech Stack:** Go 1.26.5、google.golang.org/grpc v1.83.0、google.golang.org/protobuf v1.36.11（protojson / protoregistry / dynamicpb）、stretchr/testify、bufconn 内存测试。

**Spec:** `docs/superpowers/specs/2026-08-11-sdk-go-restructure-design.md`

## Global Constraints

- sdk/go module 为 `github.com/torchwoodcloud/torchwood/sdk/go`，其 go.mod 已有 `replace github.com/torchwoodcloud/torchwood/genproto => ../../genproto`，不要改动。
- 根 module `github.com/torchwoodcloud/torchwood` 已有 genproto 的 require + `replace => ./genproto`；本计划只新增 sdk/go 的 require + replace。
- SDK 所有类型化方法签名直接使用 genproto 类型，不定义自定义 DTO。
- InvokeJSON 响应编码必须是 `protojson.MarshalOptions{Multiline: true, Indent: "  "}`（不开 EmitUnpopulated），与 CLI 现有输出逐字节一致。
- InvokeJSON 仅允许 `torchwood.server.v1.*` 且排除 `APIKeysService`；错误形态：`torchwood: unknown method "<method>"`。
- CLI 所有 JSON 解码必须用 `json.Decoder.UseNumber()`，禁止 float64 中转 64 位整型。
- **工作区有大量无关的在途改动**（git status 里几十处 M/D）：每个 commit 只 `git add` 本任务列出的具体文件，禁止 `git add -A` / `git add .`。
- sdk/go 内运行测试：`cd sdk/go && go test ./...`；CLI 测试在仓库根：`go test ./cmd/client/...`。
- 测试基建沿用 bufconn 模式（见 Task 2 Step 1 的基建代码），fake 服务嵌入 `Unimplemented*ServiceServer` 只实现待测方法。
- proto 方法名常量（如 `serverv1.UsersService_CreateUser_FullMethodName`）只可在 SDK 内部使用；CLI 一律用字符串字面量 `"/torchwood.server.v1.UsersService/CreateUser"`。

---

### Task 1: `sdk/go/internal/conn` 共享拨号包

**Files:**
- Create: `sdk/go/internal/conn/conn.go`
- Test: `sdk/go/internal/conn/conn_test.go`

**Interfaces:**
- Produces: `conn.Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error)` — target 为空返回错误；默认 `insecure.NewCredentials()` 在最前，调用方 opts 在后。Task 2/6 的 `New` 都依赖它。

- [ ] **Step 1: 写失败测试**

```go
// sdk/go/internal/conn/conn_test.go
package conn

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func TestDialEmptyTarget(t *testing.T) {
	_, err := Dial("")
	require.ErrorContains(t, err, "target is required")
}

func TestDialOK(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	defer lis.Close()
	c, err := Dial("passthrough:///bufconn", grpc.WithContextDialer(
		func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	require.NoError(t, err)
	require.NotNil(t, c)
	require.NoError(t, c.Close())
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./internal/conn/`
Expected: FAIL，`conn.go` 不存在（编译错误）。

- [ ] **Step 3: 实现**

```go
// sdk/go/internal/conn/conn.go
// Package conn 提供 client 与 server 两包共享的 gRPC 拨号逻辑。
package conn

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Dial 建立到 target 的 gRPC 连接；默认明文（insecure），调用方可通过
// opts 传入 TLS 凭据或自定义 dialer。target 不能为空。
func Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if target == "" {
		return nil, fmt.Errorf("torchwood: target is required")
	}
	all := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
	return grpc.NewClient(target, all...)
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./internal/conn/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/internal/conn/conn.go sdk/go/internal/conn/conn_test.go
git commit -m "feat(sdk-go): add shared internal/conn dial helper"
```

---

### Task 2: `sdk/go/server` 骨架（New / Option / 认证 interceptor）

**Files:**
- Create: `sdk/go/server/client.go`
- Test: `sdk/go/server/server_test.go`（本任务建基建，后续任务复用）

**Interfaces:**
- Consumes: `conn.Dial`（Task 1）。
- Produces:
  - `server.New(target string, opts ...Option) (*Client, error)`
  - `server.WithAPIKey(key string) Option`、`server.WithProjectID(id string) Option`、`server.WithDialOptions(...grpc.DialOption) Option`
  - `(*Client).Close() error`
  - 内部字段 `c.conn *grpc.ClientConn`、方法 `c.authContext(ctx) context.Context`（Task 3/4/5 依赖）。
- 行为：unary interceptor 在每次调用注入 `x-api-key`（APIKey 非空时）与 `x-torchwood-project`（ProjectID 非空时）。

- [ ] **Step 1: 写失败测试（含 bufconn 基建）**

```go
// sdk/go/server/server_test.go
package server

import (
	"context"
	"net"
	"testing"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// recorder 记录 fake 服务收到的 metadata。
type recorder struct{ md metadata.MD }

type fakeServer struct {
	serverv1.UnimplementedHealthServiceServer
	rec *recorder
}

func (f *fakeServer) Check(ctx context.Context, _ *serverv1.HealthCheckRequest) (*serverv1.HealthCheckResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.rec.md = md
	return &serverv1.HealthCheckResponse{Status: "ok"}, nil
}

func newBufconn(t *testing.T) (*bufconn.Listener, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	serverv1.RegisterHealthServiceServer(srv, &fakeServer{rec: rec})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis, rec
}

func dialer(lis *bufconn.Listener) grpc.DialOption {
	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
}

func newTestClient(t *testing.T, lis *bufconn.Listener, opts ...Option) *Client {
	t.Helper()
	opts = append(opts, WithDialOptions(dialer(lis)))
	c, err := New("passthrough:///bufconn", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAuthHeadersInjected(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("secret"), WithProjectID("proj-1"))
	_, err := c.Health.Check(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"secret"}, rec.md.Get("x-api-key"))
	require.Equal(t, []string{"proj-1"}, rec.md.Get("x-torchwood-project"))
}

func TestNoHeadersWithoutConfig(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.Health.Check(context.Background())
	require.NoError(t, err)
	require.Empty(t, rec.md.Get("x-api-key"))
	require.Empty(t, rec.md.Get("x-torchwood-project"))
}
```

注：`HealthService` 与 `Check` 在 Task 4 才完整迁移；本任务在 `client.go` 中先放最小 `HealthService`（仅 `Check`），Task 4 补齐 `Version`。

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./server/`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现**

```go
// sdk/go/server/client.go
// Package server 是 Torchwood Server API 的 Go 客户端（x-api-key 认证）。
package server

import (
	"context"

	"github.com/torchwoodcloud/torchwood/sdk/go/internal/conn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Config 汇总 Server API 客户端配置，由 Option 修改。
type Config struct {
	// APIKey 用于 x-api-key 头认证。
	APIKey string
	// ProjectID 非空时以 x-torchwood-project 头发送（admin session 项目覆盖）。
	ProjectID string
	// dialOptions 透传给底层拨号。
	dialOptions []grpc.DialOption
}

// Option 以函数选项模式修改 Config。
type Option func(*Config)

// WithAPIKey 设置 Server API 认证用的 API Key。
func WithAPIKey(key string) Option { return func(c *Config) { c.APIKey = key } }

// WithProjectID 设置 x-torchwood-project 头。
func WithProjectID(projectID string) Option { return func(c *Config) { c.ProjectID = projectID } }

// WithDialOptions 附加底层 gRPC 拨号选项。
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *Config) { c.dialOptions = append(c.dialOptions, opts...) }
}

// Client 封装 Torchwood Server API（API Key 认证）。
type Client struct {
	cfg  Config
	conn *grpc.ClientConn

	// Health 提供服务健康检查（ACCESS_PUBLIC，可不配置 API Key）。
	Health *HealthService
}

// New 建立 Server API 连接。target 为 gRPC 目标地址，不能为空。
func New(target string, opts ...Option) (*Client, error) {
	var cfg Config
	for _, o := range opts {
		o(&cfg)
	}
	c := &Client{cfg: cfg}
	dialOpts := append([]grpc.DialOption{grpc.WithChainUnaryInterceptor(c.authInterceptor())}, cfg.dialOptions...)
	gc, err := conn.Dial(target, dialOpts...)
	if err != nil {
		return nil, err
	}
	c.conn = gc
	c.Health = &HealthService{c: c, api: serverv1.NewHealthServiceClient(gc)}
	return c, nil
}

// authInterceptor 注入 x-api-key 与 x-torchwood-project 头。
func (c *Client) authInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(c.authContext(ctx), method, req, reply, cc, opts...)
	}
}

func (c *Client) authContext(ctx context.Context) context.Context {
	md := metadata.MD{}
	if c.cfg.APIKey != "" {
		md.Set("x-api-key", c.cfg.APIKey)
	}
	if c.cfg.ProjectID != "" {
		md.Set("x-torchwood-project", c.cfg.ProjectID)
	}
	if len(md) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.conn.Close() }
```

最小 HealthService（Task 4 扩到完整）：

```go
// sdk/go/server/health.go
package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// HealthService 封装 Server API 的 Health 服务。
type HealthService struct {
	c   *Client
	api serverv1.HealthServiceClient
}

// Check 返回服务健康状态。
func (h *HealthService) Check(ctx context.Context) (*serverv1.HealthCheckResponse, error) {
	return h.api.Check(ctx, &serverv1.HealthCheckRequest{})
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./server/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/server/client.go sdk/go/server/health.go sdk/go/server/server_test.go
git commit -m "feat(sdk-go): add server package skeleton with auth interceptor"
```

---

### Task 3: `server.InvokeJSON` 动态分发

**Files:**
- Create: `sdk/go/server/invoke.go`
- Test: `sdk/go/server/invoke_test.go`

**Interfaces:**
- Consumes: `(*Client).conn`、`(*Client).authContext`（Task 2，注意 conn.Invoke 需自行包 authContext）。
- Produces: `(*Client).InvokeJSON(ctx context.Context, method string, reqJSON []byte) ([]byte, error)` — CLI 唯一入口（Task 10 起依赖）。
- 行为：
  - method 形如 `/torchwood.server.v1.UsersService/CreateUser`；不在 `torchwood.server.v1.*` 或属于 `APIKeysService` → `fmt.Errorf("torchwood: unknown method %q", method)`。
  - reqJSON 为空/nil 时不解码（相当于 `{}`）；非法 JSON 或未知字段 → protojson 原始错误原样返回。
  - 响应编码 `protojson.MarshalOptions{Multiline: true, Indent: "  "}`。
  - RPC 错误原样返回（gRPC status）。

- [ ] **Step 1: 写失败测试**

```go
// sdk/go/server/invoke_test.go
package server

import (
	"context"
	"strings"
	"testing"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestInvokeJSONRoundTrip(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("k"))
	out, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.HealthService/Check", nil)
	require.NoError(t, err)
	require.Contains(t, string(out), `"status": "ok"`)
	require.Equal(t, []string{"k"}, rec.md.Get("x-api-key"))
}

func TestInvokeJSONUnknownMethod(t *testing.T) {
	lis, _ := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.Nope/Check", nil)
	require.ErrorContains(t, err, `torchwood: unknown method "/torchwood.server.v1.Nope/Check"`)
	// APIKeysService 被排除
	_, err = c.InvokeJSON(context.Background(), "/torchwood.server.v1.APIKeysService/ListAPIKeys", nil)
	require.ErrorContains(t, err, "unknown method")
	// 非 server v1 包被拒绝
	_, err = c.InvokeJSON(context.Background(), "/torchwood.client.v1.AccountService/SignIn", nil)
	require.ErrorContains(t, err, "unknown method")
}

func TestInvokeJSONBadJSON(t *testing.T) {
	lis, _ := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.InvokeJSON(context.Background(), "/torchwood.server.v1.HealthService/Check", []byte(`{"nope": 1}`))
	require.Error(t, err) // 未知字段报错（DiscardUnknown=false）
}

// TestInvokeJSONCompleteness 遍历 serverv1 全部方法（排除 APIKeysService），
// 断言每个方法都能被解析并用空 JSON 构造请求——防包名白名单回归。
func TestInvokeJSONCompleteness(t *testing.T) {
	count := 0
	for _, fd := range []protoreflect.FileDescriptor{
		serverv1.File_server_v1_health_proto,
		serverv1.File_server_v1_users_proto,
		serverv1.File_server_v1_teams_proto,
		serverv1.File_server_v1_databases_proto,
		serverv1.File_server_v1_projects_proto,
		serverv1.File_server_v1_storage_proto,
		serverv1.File_server_v1_functions_proto,
		serverv1.File_server_v1_oauth_providers_proto,
		serverv1.File_server_v1_apikeys_proto,
	} {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			ms := svc.Methods()
			for j := 0; j < ms.Len(); j++ {
				m := ms.Get(j)
				_, err := findServerMethod("/" + string(m.FullName()))
				if svc.Name() == "APIKeysService" {
					require.Error(t, err)
					continue
				}
				require.NoError(t, err, "method %s", m.FullName())
				count++
			}
		}
	}
	require.Greater(t, count, 60) // 当前 72 个左右，留余量防误删
}
```

（FullName 用 `.` 分隔，gRPC 路径用 `/` 分隔——`findServerMethod` 负责转换，见实现。）

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./server/ -run InvokeJSON`
Expected: FAIL（`findServerMethod` 未定义）。

- [ ] **Step 3: 实现**

```go
// sdk/go/server/invoke.go
package server

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// invokeMarshaler 与 CLI 历史输出格式逐字节一致：缩进 2 空格、不输出零值字段。
var invokeMarshaler = protojson.MarshalOptions{Multiline: true, Indent: "  "}

// InvokeJSON 以「方法名 + protojson 请求」调用任意 Server API unary 方法
// （APIKeysService 除外），返回 protojson 响应。method 形如
// "/torchwood.server.v1.UsersService/CreateUser"。reqJSON 可为空（相当于 {}）。
func (c *Client) InvokeJSON(ctx context.Context, method string, reqJSON []byte) ([]byte, error) {
	md, err := findServerMethod(method)
	if err != nil {
		return nil, err
	}
	req := dynamicpb.NewMessage(md.Input())
	if len(reqJSON) > 0 {
		if err := protojson.Unmarshal(reqJSON, req); err != nil {
			return nil, err
		}
	}
	resp := dynamicpb.NewMessage(md.Output())
	if err := c.conn.Invoke(c.authContext(ctx), method, req, resp); err != nil {
		return nil, err
	}
	return invokeMarshaler.Marshal(resp)
}

// findServerMethod 按 gRPC 路径查找 MethodDescriptor，限定 torchwood.server.v1
// 且排除 APIKeysService（API Key 凭证被服务端禁止调用该服务）。
func findServerMethod(method string) (protoreflect.MethodDescriptor, error) {
	unknown := fmt.Errorf("torchwood: unknown method %q", method)
	name := strings.TrimPrefix(method, "/")
	// gRPC 路径 "pkg.Service/Method" -> protoreflect 全名 "pkg.Service.Method"
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return nil, unknown
	}
	full := name[:idx] + "." + name[idx+1:]
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(full))
	if err != nil {
		return nil, unknown
	}
	md, ok := d.(protoreflect.MethodDescriptor)
	if !ok {
		return nil, unknown
	}
	svc, ok := md.Parent().(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, unknown
	}
	if svc.Name() == "APIKeysService" || !strings.HasPrefix(string(svc.FullName()), "torchwood.server.v1.") {
		return nil, unknown
	}
	return md, nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./server/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/server/invoke.go sdk/go/server/invoke_test.go
git commit -m "feat(sdk-go): add server InvokeJSON via protoregistry dynamic dispatch"
```

---

### Task 4: server 类型化服务迁移（Health / Users / Teams / Databases）

**Files:**
- Modify: `sdk/go/server/health.go`（补 `Version`）
- Create: `sdk/go/server/users.go`、`sdk/go/server/teams.go`、`sdk/go/server/databases.go`
- Modify: `sdk/go/server/client.go`（挂接 Users/Teams/Databases 字段）
- Test: `sdk/go/server/services_test.go`

**Interfaces:**
- Consumes: Task 2 的 `Client`。
- Produces（供 Go SDK 用户；签名与旧 `sdk/go/server.go` 完全一致，逐方法迁移，仅把 `c.AuthContext(ctx)` 调用删掉——认证已由 interceptor 处理）:
  - `(*Client).Health`：增加 `Version(ctx) (*serverv1.GetVersionResponse, error)`
  - `(*Client).Users *UsersService`：CreateUser/GetUser/ListUsers/UpdateUser/DeleteUser/ListUserSessions/DeleteUserSession/CreateUserToken（签名同旧 sdk/go/server.go）
  - `(*Client).Teams *TeamsService`：CreateTeam/GetTeam/ListTeams/CreateMembership/ListMemberships/GetMembership/UpdateMembership/UpdateMembershipStatus/DeleteMembership（同旧 ServerTeamsService）
  - `(*Client).Databases *DatabasesService`：同旧 ServerDatabasesService 全部 17 个方法；`(*Client).UseDatabase(databaseID string) *DatabasesService`；`WithDatabaseID(databaseID string) Option`

- [ ] **Step 1: 迁移测试**

从旧 `sdk/go/torchwood_test.go`、`sdk/go/server_test.go`（若存在）中把 Server API 相关测试逐条搬进 `sdk/go/server/services_test.go`，包名改为 `server`，`NewServerClient` 改为 `New`，`WithServerAPIKey` 改为 `WithAPIKey`，fake 服务嵌入 `serverv1.Unimplemented*ServiceServer`。每个 service 至少一条 bufconn 往返断言。

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./server/`
Expected: FAIL（Users/Teams/Databases 未定义）。

- [ ] **Step 3: 迁移实现**

把旧 `sdk/go/server.go` 的 4 个 service 实现按 service 拆到对应文件，做三处机械修改：
1. 类型改名：`ServerTeamsService`→`TeamsService`、`ServerDatabasesService`→`DatabasesService`；
2. 删除每个方法里的 `c.AuthContext(ctx)` 包装，直接传 `ctx`（interceptor 已注入认证）；
3. `Config` 字段名 `ServerAPIKey`→`APIKey`（这些方法不直接读 cfg 的 key，仅 DatabasesService 读 `DatabaseID`）。

`client.go` 的 `Client` 增加：

```go
	// Users 提供用户管理（含 CreateUserToken 签发 Agent 凭证）。
	Users *UsersService
	// Teams 提供服务端团队管理。
	Teams *TeamsService
	// Databases 提供库/集合/属性/索引/文档管理，绑定默认 DatabaseID。
	Databases *DatabasesService
```

`New` 中挂接；新增 `WithDatabaseID` Option 与 `UseDatabase`：

```go
// WithDatabaseID 设置文档 API 的默认 database_id。
func WithDatabaseID(databaseID string) Option {
	return func(c *Config) { c.DatabaseID = databaseID }
}

// UseDatabase 返回绑定指定 databaseID 的文档服务副本。
func (c *Client) UseDatabase(databaseID string) *DatabasesService {
	return newDatabasesService(c, databaseID)
}
```

（`Config` 增加 `DatabaseID string` 字段；`DatabasesService` 携带 `db string`，各文档方法把 `db` 填入请求的 `database_id`，与旧实现一致。）

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./server/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/server/
git commit -m "feat(sdk-go): migrate Health/Users/Teams/Databases typed services into server package"
```

---

### Task 5: server 类型化服务新增（Projects / Storage / Functions / OAuthProviders）

**Files:**
- Create: `sdk/go/server/projects.go`、`sdk/go/server/storage.go`、`sdk/go/server/functions.go`、`sdk/go/server/oauth.go`
- Modify: `sdk/go/server/client.go`（挂接 4 个字段）
- Test: `sdk/go/server/services_ext_test.go`

**Interfaces:**
- Produces（薄封装，直接透传 genproto 请求消息——与「SDK 不封装自定义类型」一致，保持机械）：
  - `(*Client).Projects *ProjectsService`：`CreateProject/ListProjects/GetProject/UpdateProject`
  - `(*Client).Storage *StorageService`：`CreateBucket/ListBuckets/GetBucket/DeleteBucket/UpdateBucket/CreateFile/ListFiles/GetFile/DeleteFile/UpdateFile/CreateFileToken/GetStorageUsage`
  - `(*Client).Functions *FunctionsService`：`ListRuntimes/ListSpecifications/CreateFunction/ListFunctions/GetFunction/UpdateFunction/DeleteFunction/CreateDeployment/ListDeployments/GetDeployment/DeleteDeployment/SetVariables/GetVariables/CreateExecution/ListExecutions/GetExecution`
  - `(*Client).OAuthProviders *OAuthProvidersService`：`ListOAuthProviders/UpsertOAuthProvider/DeleteOAuthProvider`
- 签名模式（每个方法一行，无便利参数）：

```go
func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	return s.api.CreateProject(ctx, req)
}
```

- [ ] **Step 1: 写失败测试**

每个新 service 一条 bufconn 往返（fake 嵌入对应 `Unimplemented*ServiceServer`，断言请求字段透传与响应返回）。示例：

```go
// sdk/go/server/services_ext_test.go（片段）
type fakeProjects struct {
	serverv1.UnimplementedProjectsServiceServer
	got *serverv1.CreateProjectRequest
}

func (f *fakeProjects) CreateProject(_ context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	f.got = req
	return &serverv1.Project{Id: req.Id, Name: req.Name}, nil
}

func TestProjectsCreateProject(t *testing.T) {
	// 注册 fakeProjects 的 bufconn server，调用 c.Projects.CreateProject，
	// 断言 fake 收到的 req.Id/req.Name 与响应一致
}
```

其余三个 service 同模式（Storage 测 `CreateBucket`，Functions 测 `CreateFunction`，OAuthProviders 测 `UpsertOAuthProvider`）。

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./server/`
Expected: FAIL。

- [ ] **Step 3: 实现**

每个文件一个 service 结构体 + 透传方法，方法清单以 proto 为准（见计划头部 spec 与 `proto/server/v1/*.proto`）：

```go
// sdk/go/server/projects.go
package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// ProjectsService 封装 Server API 的 Projects 服务。
type ProjectsService struct {
	c   *Client
	api serverv1.ProjectsServiceClient
}

func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
	return s.api.CreateProject(ctx, req)
}

func (s *ProjectsService) ListProjects(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListProjectsResponse, error) {
	return s.api.ListProjects(ctx, req)
}

// GetProject / UpdateProject 同模式
```

Storage/Functions/OAuthProviders 按同样模式写完 proto 中的全部方法（方法清单见 `proto/server/v1/storage.proto`、`functions.proto`、`oauth_providers.proto`，一个不漏）。`client.go` 增加 4 个字段并在 `New` 中挂接。

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./server/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/server/
git commit -m "feat(sdk-go): add Projects/Storage/Functions/OAuthProviders typed services"
```

---

### Task 6: `sdk/go/client` TokenStore（接口 + 内存 + 文件）

**Files:**
- Create: `sdk/go/client/token.go`
- Test: `sdk/go/client/token_test.go`

**Interfaces:**
- Produces:
  - `TokenStore` 接口：`Load() (*clientv1.TokenBundle, error)` / `Save(*clientv1.TokenBundle) error` / `Clear() error`
  - `NewMemoryTokenStore() *MemoryTokenStore`（mutex 保护）
  - `NewFileTokenStore(path string) *FileTokenStore`（mutex 保护；0600；临时文件 + rename 原子写；Load 文件不存在返回 `(nil, nil)`；Clear 幂等）
- 文件格式：protojson 编码的 `clientv1.TokenBundle`。

- [ ] **Step 1: 写失败测试**

```go
// sdk/go/client/token_test.go
package client

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/stretchr/testify/require"
)

func bundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1893456000}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryTokenStore()
	got, err := s.Load()
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, s.Save(bundle()))
	got, err = s.Load()
	require.NoError(t, err)
	require.Equal(t, "a", got.AccessToken)
	require.NoError(t, s.Clear())
	got, _ = s.Load()
	require.Nil(t, got)
}

func TestFileStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	s := NewFileTokenStore(p)
	got, err := s.Load() // 文件不存在 -> (nil, nil)
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, s.Save(bundle()))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	got, err = NewFileTokenStore(p).Load()
	require.NoError(t, err)
	require.Equal(t, "r", got.RefreshToken)
	require.Equal(t, int64(1893456000), got.ExpiresAt)

	require.NoError(t, s.Clear())
	require.NoError(t, s.Clear()) // 幂等
	got, _ = s.Load()
	require.Nil(t, got)
}

func TestFileStoreCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	require.NoError(t, os.WriteFile(p, []byte("{bad"), 0o600))
	_, err := NewFileTokenStore(p).Load()
	require.Error(t, err)
}

func TestFileStoreConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	s := NewFileTokenStore(p)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Save(bundle())
			_, _ = s.Load()
		}()
	}
	wg.Wait()
	got, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, "a", got.AccessToken)
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./client/`
Expected: FAIL。

- [ ] **Step 3: 实现**

```go
// sdk/go/client/token.go
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// TokenStore 持久化 TokenBundle（access/refresh token）。
type TokenStore interface {
	// Load 返回已存 token；无 token 时返回 (nil, nil)。
	Load() (*clientv1.TokenBundle, error)
	Save(*clientv1.TokenBundle) error
	// Clear 删除已存 token；无 token 时不报错。
	Clear() error
}

// MemoryTokenStore 进程内 TokenStore。
type MemoryTokenStore struct {
	mu sync.Mutex
	t  *clientv1.TokenBundle
}

// NewMemoryTokenStore 创建空的内存 TokenStore。
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{} }

func (s *MemoryTokenStore) Load() (*clientv1.TokenBundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t, nil
}

func (s *MemoryTokenStore) Save(t *clientv1.TokenBundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t = t
	return nil
}

func (s *MemoryTokenStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t = nil
	return nil
}

// FileTokenStore JSON 文件 TokenStore：protojson 格式、0600 权限、
// 临时文件 + rename 原子写，内置 mutex 可并发使用。
type FileTokenStore struct {
	mu   sync.Mutex
	path string
}

// NewFileTokenStore 创建绑定 path 的文件 TokenStore。
func NewFileTokenStore(path string) *FileTokenStore { return &FileTokenStore{path: path} }

func (s *FileTokenStore) Load() (*clientv1.TokenBundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("torchwood: read token file: %w", err)
	}
	var t clientv1.TokenBundle
	if err := protojson.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("torchwood: parse token file: %w", err)
	}
	return &t, nil
}

func (s *FileTokenStore) Save(t *clientv1.TokenBundle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := protojson.Marshal(t)
	if err != nil {
		return fmt.Errorf("torchwood: encode token: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("torchwood: write token file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("torchwood: rename token file: %w", err)
	}
	// 确保已存在文件也被收紧权限（rename 继承 tmp 权限，此处兜底）
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("torchwood: chmod token file: %w", err)
	}
	return nil
}

func (s *FileTokenStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("torchwood: remove token file: %w", err)
	}
	return nil
}
```

（`filepath` 若未用到可删 import；Save 中 `os.Rename` 在 Windows 上目标已存在会报错——rename 前 `os.Remove(s.path)` 忽略 NotExist，保证 Windows 可用。）

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./client/`
Expected: PASS（Windows 上注意 rename 语义，若失败按上面括号内说明修正）。

- [ ] **Step 5: Commit**

```bash
git add sdk/go/client/token.go sdk/go/client/token_test.go
git commit -m "feat(sdk-go): add client TokenStore with memory and file implementations"
```

---

### Task 7: client 骨架 + 自动刷新 interceptor

**Files:**
- Create: `sdk/go/client/client.go`、`sdk/go/client/auth.go`
- Test: `sdk/go/client/client_test.go`（基建）+ `sdk/go/client/auth_test.go`

**Interfaces:**
- Consumes: `conn.Dial`（Task 1）、`TokenStore`（Task 6）。
- Produces:
  - `client.New(target string, opts ...Option) (*Client, error)`
  - Options：`WithProjectID`、`WithDatabaseID`、`WithTokenStore`、`WithOnTokensChanged`、`WithInitialTokens`、`WithDialOptions`
  - `(*Client).Close() error`
  - 内部：`c.store TokenStore`、`c.saveTokens(*clientv1.TokenBundle)`、`c.clearTokens()`、`c.refreshIfExpiring(ctx) error`、`c.refreshAfterUnauthorized(ctx, usedToken string) bool`、`c.now func() time.Time`（测试可注入）
  - 公开方法白名单（不刷新、不重试）：`AccountService/SignIn`、`SignUp`、`RefreshToken`；`SignOut` 挂 token 但不刷新不 401 重试。
- 行为（与 spec「自动刷新」一一对应）：提前 30s 主动刷新；401 且 refresh token 存在时刷新重试一次；刷新 mutex 串行 + 锁内 double-check；ExpiresAt==0 跳过主动刷新；刷新 RPC 返回 Unauthenticated 才清 token。

- [ ] **Step 1: 写失败测试**

```go
// sdk/go/client/auth_test.go
package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAccount 记录 RefreshToken 调用次数，Me 可配置首次 401。
type fakeAccount struct {
	clientv1.UnimplementedAccountServiceServer
	refreshCalls atomic.Int32
	meCalls      atomic.Int32
	failFirstMe  atomic.Bool
	lastAuth     atomic.Value // string
	tokens       *clientv1.TokenBundle
}

func (f *fakeAccount) RefreshToken(_ context.Context, req *clientv1.RefreshTokenRequest) (*clientv1.RefreshTokenResponse, error) {
	f.refreshCalls.Add(1)
	if req.RefreshToken != "refresh-1" {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	return &clientv1.RefreshTokenResponse{Tokens: f.tokens}, nil
}

func (f *fakeAccount) Me(ctx context.Context, _ *clientv1.MeRequest) (*clientv1.Account, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.lastAuth.Store(md.Get("authorization"))
	if f.failFirstMe.Load() && f.meCalls.Add(1) == 1 {
		return nil, status.Error(codes.Unauthenticated, "expired")
	}
	return &clientv1.Account{Id: "u1"}, nil
}

// 基建：newClientBufconn / newTestClient 模式同 server 包 Task 2 Step 1，
// 注册 fakeAccount 到 bufconn server。

func expiredBundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "old", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(-time.Minute).Unix()}
}

func freshBundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "fresh", RefreshToken: "refresh-1", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestProactiveRefresh(t *testing.T) {
	// store 预置过期 token；fake.tokens = freshBundle()
	// 调用 c.Account.Me；断言 refreshCalls==1，Me 收到的 authorization 为 "Bearer fresh"
}

func TestNoProactiveRefreshWhenFresh(t *testing.T) {
	// store 预置 freshBundle()；调用 Me；断言 refreshCalls==0
}

func TestRetryOnUnauthorized(t *testing.T) {
	// store 预置 freshBundle()；fake.failFirstMe=true，fake.tokens=另一个新 bundle
	// 调用 Me；断言成功、refreshCalls==1、Me 被调 2 次
}

func TestRefreshUnauthenticatedClearsTokens(t *testing.T) {
	// store 预置 expiredBundle() 但 RefreshToken="bad"
	// 调用 Me；断言返回 Unauthenticated，store.Load()==nil，回调收到 nil
}

func TestRefreshTemporaryErrorKeepsTokens(t *testing.T) {
	// fake RefreshToken 返回 codes.Unavailable；断言错误原样返回且 store 未清空
}

func TestConcurrentRefreshDedup(t *testing.T) {
	// store 预置 expiredBundle()；16 个 goroutine 并发调 Me
	// 断言 refreshCalls==1
}
```

每个测试用 bufconn + `WithTokenStore`（预置 token 的 MemoryTokenStore）+ `WithInitialTokens` 或构造后 `store.Save`。

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./client/`
Expected: FAIL。

- [ ] **Step 3: 实现**

```go
// sdk/go/client/client.go
// Package client 是 Torchwood Client API 的 Go 客户端（end-user Bearer JWT，
// 自动刷新 token）。
package client

import (
	"context"
	"sync"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/internal/conn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// refreshSkew 主动刷新的提前量。
const refreshSkew = 30 * time.Second

// Config 汇总 Client API 客户端配置，由 Option 修改。
type Config struct {
	ProjectID  string
	DatabaseID string

	tokenStore      TokenStore
	onTokensChanged func(*clientv1.TokenBundle)
	initialTokens   *clientv1.TokenBundle
	dialOptions     []grpc.DialOption
}

// Option 以函数选项模式修改 Config。
type Option func(*Config)

// WithProjectID 设置默认项目 ID（填入 Account 请求的 project_id）。
func WithProjectID(projectID string) Option { return func(c *Config) { c.ProjectID = projectID } }

// WithDatabaseID 设置文档 API 的默认 database_id（可选便利默认，
// 不知道时用 Client.UseDatabase 晚绑定）。
func WithDatabaseID(databaseID string) Option { return func(c *Config) { c.DatabaseID = databaseID } }

// WithTokenStore 设置 token 持久化；默认 MemoryTokenStore。
func WithTokenStore(store TokenStore) Option { return func(c *Config) { c.tokenStore = store } }

// WithOnTokensChanged 设置 token 变化回调（登录/刷新/清空；清空传 nil）。
func WithOnTokensChanged(fn func(*clientv1.TokenBundle)) Option {
	return func(c *Config) { c.onTokensChanged = fn }
}

// WithInitialTokens 预置已有会话的 token。
func WithInitialTokens(t *clientv1.TokenBundle) Option {
	return func(c *Config) { c.initialTokens = t }
}

// WithDialOptions 附加底层 gRPC 拨号选项。
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *Config) { c.dialOptions = append(c.dialOptions, opts...) }
}

// Client 封装 Torchwood Client API（end-user 认证，自动刷新 token）。
type Client struct {
	cfg   Config
	conn  *grpc.ClientConn
	store TokenStore

	mu  sync.Mutex // 串行化刷新
	now func() time.Time

	account clientv1.AccountServiceClient

	// Account 提供注册/登录/账户管理。
	Account *AccountService
}

// New 建立 Client API 连接。target 为 gRPC 目标地址，不能为空。
func New(target string, opts ...Option) (*Client, error) {
	var cfg Config
	for _, o := range opts {
		o(&cfg)
	}
	store := cfg.tokenStore
	if store == nil {
		store = NewMemoryTokenStore()
	}
	c := &Client{cfg: cfg, store: store, now: time.Now}
	if cfg.initialTokens != nil {
		if err := c.store.Save(cfg.initialTokens); err != nil {
			return nil, err
		}
	}
	dialOpts := append([]grpc.DialOption{grpc.WithChainUnaryInterceptor(c.authInterceptor())}, cfg.dialOptions...)
	gc, err := conn.Dial(target, dialOpts...)
	if err != nil {
		return nil, err
	}
	c.conn = gc
	c.account = clientv1.NewAccountServiceClient(gc)
	c.Account = &AccountService{c: c}
	return c, nil
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.conn.Close() }

// saveTokens 保存 token 并触发回调。
func (c *Client) saveTokens(t *clientv1.TokenBundle) error {
	if err := c.store.Save(t); err != nil {
		return err
	}
	if c.cfg.onTokensChanged != nil {
		c.cfg.onTokensChanged(t)
	}
	return nil
}

// clearTokens 清空 token 并触发回调（nil）。
func (c *Client) clearTokens() {
	_ = c.store.Clear()
	if c.cfg.onTokensChanged != nil {
		c.cfg.onTokensChanged(nil)
	}
}
```

```go
// sdk/go/client/auth.go
package client

import (
	"context"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// noRefreshMethods 不经过刷新/401 重试逻辑的方法（公开方法 + SignOut）。
var noRefreshMethods = map[string]bool{
	clientv1.AccountService_SignIn_FullMethodName:       true,
	clientv1.AccountService_SignUp_FullMethodName:       true,
	clientv1.AccountService_RefreshToken_FullMethodName: true,
	clientv1.AccountService_SignOut_FullMethodName:      true,
}

// authInterceptor 挂 Bearer token，处理主动刷新与 401 刷新重试。
func (c *Client) authInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if noRefreshMethods[method] {
			tok, _ := c.store.Load()
			return invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
		}
		if err := c.refreshIfExpiring(ctx); err != nil {
			return err
		}
		tok, _ := c.store.Load()
		err := invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
		if status.Code(err) != codes.Unauthenticated || tok == nil {
			return err
		}
		// 401：仅当 token 未被其他 goroutine 刷新过时刷新一次并重试
		if !c.refreshAfterUnauthorized(ctx, tok.AccessToken) {
			return err
		}
		tok, _ = c.store.Load()
		return invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
	}
}

func withBearer(ctx context.Context, tok *clientv1.TokenBundle) context.Context {
	if tok == nil || tok.AccessToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok.AccessToken)
}

func tokenExpiring(t *clientv1.TokenBundle, now time.Time) bool {
	return t != nil && t.ExpiresAt > 0 && time.Until(time.Unix(t.ExpiresAt, 0)) < refreshSkew
}

// refreshIfExpiring 主动刷新：token 距过期不足 refreshSkew 时用 refresh token 换新。
func (c *Client) refreshIfExpiring(ctx context.Context) error {
	tok, _ := c.store.Load()
	if !tokenExpiring(tok, c.now()) || tok.RefreshToken == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// double-check：等待锁期间可能已被其他 goroutine 刷新
	tok, _ = c.store.Load()
	if !tokenExpiring(tok, c.now()) {
		return nil
	}
	return c.doRefreshLocked(ctx, tok.RefreshToken)
}

// refreshAfterUnauthorized 401 后刷新：仅当 store 中仍是用过的那个 token 才刷新
// （否则说明已有其他 goroutine 刷新过，直接重试即可）。
func (c *Client) refreshAfterUnauthorized(ctx context.Context, usedToken string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, _ := c.store.Load()
	if tok == nil || tok.RefreshToken == "" {
		return false
	}
	if tok.AccessToken == usedToken {
		if err := c.doRefreshLocked(ctx, tok.RefreshToken); err != nil {
			return false
		}
	}
	return true
}

// doRefreshLocked 调用 RefreshToken；仅在 Unauthenticated（refresh token 失效）
// 时清空本地 token，临时错误保留。调用方须持 c.mu。
func (c *Client) doRefreshLocked(ctx context.Context, refreshToken string) error {
	resp, err := c.account.RefreshToken(ctx, &clientv1.RefreshTokenRequest{
		ProjectId:    c.cfg.ProjectID,
		RefreshToken: refreshToken,
	})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			c.clearTokens()
		}
		return err
	}
	return c.saveTokens(resp.Tokens)
}
```

`AccountService` 最小占位（Task 8 补全）：

```go
// sdk/go/client/account.go
package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
)

// AccountService 封装 Client API 的 Account 服务。
type AccountService struct{ c *Client }

// Me 返回当前登录账户信息。
func (a *AccountService) Me(ctx context.Context) (*clientv1.Account, error) {
	return a.c.account.Me(ctx, &clientv1.MeRequest{ProjectId: a.c.cfg.ProjectID})
}
```

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./client/`
Expected: PASS（含并发去重测试）

- [ ] **Step 5: Commit**

```bash
git add sdk/go/client/client.go sdk/go/client/auth.go sdk/go/client/account.go sdk/go/client/client_test.go sdk/go/client/auth_test.go
git commit -m "feat(sdk-go): add client skeleton with auto-refresh auth interceptor"
```

---

### Task 8: client Account/Teams/Databases 服务补全

**Files:**
- Modify: `sdk/go/client/account.go`（补 SignUp/SignIn/RefreshToken/SignOut）
- Create: `sdk/go/client/teams.go`、`sdk/go/client/databases.go`
- Modify: `sdk/go/client/client.go`（挂接 Teams/Databases 字段、UseDatabase）
- Test: `sdk/go/client/account_test.go`、`sdk/go/client/services_test.go`

**Interfaces:**
- Produces:
  - `AccountService`：`SignUp(ctx, email, password, name)`、`SignIn(ctx, email, password)`、`RefreshToken(ctx, refreshToken)`、`Me(ctx)`、`SignOut(ctx) error`
  - `(*Client).Teams *TeamsService`：签名同旧 sdk/go/teams.go 全部 7 个方法
  - `(*Client).Databases *DatabasesService`、`(*Client).UseDatabase(databaseID string) *DatabasesService`：签名同旧 sdk/go/databases.go 全部 7 个方法
- 行为（spec「Account 服务行为」）：
  - SignIn/SignUp：仅当 `resp.MfaRequired == false && resp.Tokens != nil && resp.Tokens.AccessToken != ""` 才 `saveTokens`。
  - SignOut：成功或 `codes.Unauthenticated` 都 `clearTokens()`；其他错误不清。
  - RefreshToken：成功后 `saveTokens`。

- [ ] **Step 1: 写失败测试**

```go
// sdk/go/client/account_test.go（要点）
func TestSignInSavesTokens(t *testing.T) {
	// fake SignIn 返回 tokens；断言 store.Load() 非空、回调收到 bundle
}

func TestSignInMFADoesNotSaveTokens(t *testing.T) {
	// fake SignIn 返回 mfa_required=true、tokens=nil；断言 store 仍为空、回调未触发
}

func TestSignOutClearsOnSuccess(t *testing.T) { /* 预置 token，SignOut 成功后 store 为空、回调 nil */ }

func TestSignOutClearsOnUnauthenticated(t *testing.T) { /* fake 返回 Unauthenticated，store 同样清空 */ }

func TestSignOutKeepsOnNetworkError(t *testing.T) { /* fake 返回 Unavailable，store 保留 */ }
```

Teams/Databases：从旧 `sdk/go/torchwood_test.go` 迁移 Client API 相关测试到 `services_test.go`（包名 `client`，`NewClient`→`New`）。

- [ ] **Step 2: 运行确认失败**

Run: `cd sdk/go && go test ./client/`
Expected: FAIL。

- [ ] **Step 3: 实现**

```go
// account.go 追加（SignIn 示例，SignUp 同模式）：

// SignIn 使用邮箱/密码登录；成功（非 MFA 分支）后自动保存 token。
func (a *AccountService) SignIn(ctx context.Context, email, password string) (*clientv1.SignInResponse, error) {
	resp, err := a.c.account.SignIn(ctx, &clientv1.SignInRequest{
		Email:     email,
		Password:  password,
		ProjectId: a.c.cfg.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	if !resp.MfaRequired && resp.Tokens != nil && resp.Tokens.AccessToken != "" {
		if err := a.c.saveTokens(resp.Tokens); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// RefreshToken 用刷新令牌换取新令牌并保存。
func (a *AccountService) RefreshToken(ctx context.Context, refreshToken string) (*clientv1.RefreshTokenResponse, error) {
	resp, err := a.c.account.RefreshToken(ctx, &clientv1.RefreshTokenRequest{
		ProjectId:    a.c.cfg.ProjectID,
		RefreshToken: refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if resp.Tokens != nil && resp.Tokens.AccessToken != "" {
		if err := a.c.saveTokens(resp.Tokens); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// SignOut 注销当前会话；成功或 token 已失效（Unauthenticated）都清空本地 token。
func (a *AccountService) SignOut(ctx context.Context) error {
	_, err := a.c.account.SignOut(ctx, &clientv1.SignOutRequest{ProjectId: a.c.cfg.ProjectID})
	if err == nil || status.Code(err) == codes.Unauthenticated {
		a.c.clearTokens()
	}
	return err
}
```

teams.go / databases.go 从旧 sdk/go 逐方法迁移，删除 `c.AuthContext(ctx)` 包装（interceptor 已处理）。`client.go` 增加 `Teams`/`Databases` 字段、`UseDatabase` 方法。

- [ ] **Step 4: 运行确认通过**

Run: `cd sdk/go && go test ./client/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/client/
git commit -m "feat(sdk-go): complete client Account/Teams/Databases services with token lifecycle"
```

---

### Task 9: 删除旧根 package，sdk/go 全量绿

**Files:**
- Delete: `sdk/go/torchwood.go`、`sdk/go/account.go`、`sdk/go/teams.go`、`sdk/go/databases.go`、`sdk/go/server.go` 及对应旧 `*_test.go`（`sdk/go/*_test.go` 中属于根 package 的全部文件）

- [ ] **Step 1: 删除旧文件**

逐一删除根 package 的全部 `.go` 与 `_test.go`（其测试已在 Task 4/8 迁移）。保留：`go.mod`、`go.sum`、`client/`、`server/`、`internal/`。

- [ ] **Step 2: 全量验证**

Run: `cd sdk/go && go build ./... && go vet ./... && go test ./... -cover`
Expected: 全部 PASS；根 package 已不存在（`go list .` 在 sdk/go 根报错 `no Go files` 属正常）。

- [ ] **Step 3: Commit**

```bash
git add sdk/go/
git commit -m "refactor(sdk-go): remove legacy root package, split into client/server packages"
```

---

### Task 10: 根 go.mod 接入 + CLI 调用核心改造（invoke/printJSON/health/rpc）

**Files:**
- Modify: `go.mod`（根）
- Modify: `cmd/client/output.go`（invoke/printJSON 重写；formatRPCError/changedBoolPtr 保留——changedBoolPtr 在 Task 11 统一处理前先留着）
- Modify: `cmd/client/cmd_health.go`、`cmd/client/cmd_rpc.go`
- Modify: `cmd/client/output_test.go`
- Delete: `cmd/client/conn.go`、`cmd/client/registry.go`、`cmd/client/registry_test.go`

**Interfaces:**
- Consumes: `server.New`、`(*Client).InvokeJSON`（Task 2/3）。
- Produces（Task 11-14 全部命令依赖）:
  - `invoke(g *globalFlags, method string, req any) ([]byte, error)`：req 支持 `nil` / `string`（原始 JSON，如 --data）/ `map[string]any`（encoding/json marshal）；`--tls` 时报错 `--tls 尚未支持：服务端当前为明文 gRPC`（沿用旧文案）；g.apiKey 非空才 `WithAPIKey`；错误经 `formatRPCError` 格式化。
  - `printJSON(w io.Writer, b []byte) error`：原样输出 + 换行。

- [ ] **Step 1: 改造测试**

`output_test.go`：protojson marshal 断言保留（现在断言 InvokeJSON 输出字节格式由 SDK 测试保证，CLI 侧改为断言 printJSON 原样写字节 + 换行）。新增：

```go
func TestInvokeTLSNotSupported(t *testing.T) {
	g := &globalFlags{tls: true}
	_, err := invoke(g, "/torchwood.server.v1.HealthService/Check", nil)
	require.ErrorContains(t, err, "--tls 尚未支持")
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./cmd/client/`
Expected: FAIL（invoke 签名变化导致编译错误）。

- [ ] **Step 3: 实现**

根 `go.mod` 追加（随后 `go mod tidy`）：

```go
require github.com/torchwoodcloud/torchwood/sdk/go v0.0.0-00010101000000-000000000000

replace github.com/torchwoodcloud/torchwood/sdk/go => ./sdk/go
```

`cmd/client/output.go` 重写核心：

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/torchwoodcloud/torchwood/sdk/go/server"
	"google.golang.org/grpc/codes"   // 注：formatRPCError 仍需 status/codes，
	"google.golang.org/grpc/status" // 见下方说明
)

// invoke 建立连接并以全局超时发起一次 InvokeJSON 调用。
// req 为 nil / string（原始 JSON）/ map[string]any。
func invoke(g *globalFlags, method string, req any) ([]byte, error) {
	if g.tls {
		return nil, errors.New("--tls 尚未支持：服务端当前为明文 gRPC")
	}
	var opts []server.Option
	if g.apiKey != "" {
		opts = append(opts, server.WithAPIKey(g.apiKey))
	}
	c, err := server.New(g.endpoint, opts...)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), g.timeoutDur)
	defer cancel()

	var reqJSON []byte
	switch v := req.(type) {
	case nil:
	case string:
		reqJSON = []byte(v)
	case []byte:
		reqJSON = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("请求编码失败：%v", err)
		}
		reqJSON = b
	}
	resp, err := c.InvokeJSON(ctx, method, reqJSON)
	if err != nil {
		return nil, errors.New(formatRPCError(err))
	}
	return resp, nil
}

// printJSON 把响应 JSON 字节原样渲染到 stdout（SDK 已按缩进格式编码）。
func printJSON(w io.Writer, b []byte) error {
	_, err := fmt.Fprintln(w, string(b))
	return err
}
```

**关于 `google.golang.org/grpc` import**：`formatRPCError` 需要 `status.FromError`/`codes.PermissionDenied`。为达成「CLI 不 import grpc」，将错误格式化下沉：SDK 的 InvokeJSON 原样返回 status 错误，CLI 改用 `server.RPCError` 辅助——在 `sdk/go/server` 增加：

```go
// sdk/go/server/errors.go
package server

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrorCode 返回错误的 gRPC code；非 status 错误返回 codes.Unknown。
func ErrorCode(err error) codes.Code {
	return status.Code(err)
}
```

但 `codes.Code` 类型本身来自 grpc 包——CLI 比较 `server.ErrorCode(err) == codes.PermissionDenied` 仍会 import grpc。因此 CLI 侧只判断文案语义：在 errors.go 再加：

```go
// IsPermissionDenied 报告错误是否为 PermissionDenied（如 API Key scope 不足）。
func IsPermissionDenied(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}
```

CLI 的 `formatRPCError` 改为：

```go
func formatRPCError(err error) string {
	if server.IsPermissionDenied(err) {
		return fmt.Sprintf("rpc failed: %v\n提示：请检查 API Key 的 scope（如 users.read / users.write，或 * / all），或用 Console 重新生成 key", err)
	}
	return fmt.Sprintf("rpc failed: %v", err)
}
```

（status 错误的 `err.Error()` 形如 `rpc error: code = PermissionDenied desc = ...`，与旧输出 code+message 等价可读；输出文案以现有测试断言为准微调。）

`cmd_health.go`：把 `invoke(g, serverv1.HealthService_Check_FullMethodName, &serverv1.HealthCheckRequest{}, resp)` 改为：

```go
resp, err := invoke(g, "/torchwood.server.v1.HealthService/Check", nil)
if err != nil {
	return err
}
return printJSON(os.Stdout, resp)
```

`cmd_rpc.go`：`--data` 直接透传：

```go
resp, err := invoke(g, method, data) // data 为 string；空串时 invoke 传 nil
if err != nil {
	return err
}
return printJSON(os.Stdout, resp)
```

（`invoke` 的 string 分支：`if v == "" { reqJSON = nil }`——在 invoke 内处理。）

删除 `conn.go`、`registry.go`、`registry_test.go`。其余 cmd_*.go 暂存编译错误，Task 11-14 逐个修（本任务允许中间状态编译不过，Step 4 只跑 output/health/rpc 相关测试前需把其他文件暂时注释？——**不允许**：因此本任务与 Task 11-14 合并提交节奏：本任务 Step 4 的验证为 `go build ./cmd/client/` 暂时失败属预期，提交点移到 Task 14 结束。**改为：Task 10-14 作为一个连续工作流，每个 Task 结束时只跑该任务覆盖文件的测试，编译验证与提交在 Task 14 统一进行**。各任务内 `git add` 的文件清单保留，统一在 Task 14 Step 4 后逐个 commit。）

- [ ] **Step 4: 局部验证**

Run: `go test ./cmd/client/ -run 'TestInvoke|TestPrintJSON|TestFormatRPCError'`
Expected: 编译错误来自尚未迁移的 cmd_teams/databases/projects/storage/functions/oauth/users/helpers——属预期，继续 Task 11。

- [ ] **Step 5: 暂缓提交**（并入 Task 14 统一提交；go.mod 改动可先单独提交）

```bash
git add go.mod go.sum
git commit -m "build: wire sdk/go into root module for CLI"
```

---

### Task 11: CLI helpers 重写 + cmd_users.go + cmd_projects.go

**Files:**
- Modify: `cmd/client/cmd_helpers.go`（全部重写为 JSON map 版本）
- Modify: `cmd/client/cmd_users.go`、`cmd/client/cmd_projects.go`
- Modify: `cmd/client/cmd_users_test.go`（及 helpers 相关测试）

**Interfaces:**
- Produces（Task 12-14 依赖）:
  - `jsonStringList(s, flagName string) ([]string, error)`（保留原语义：JSON 数组字符串）
  - `jsonStringMap(s, flagName string) (map[string]string, error)`（保留）
  - `jsonInt64Map(s, flagName string) (map[string]json.Number, error)`：**改用 UseNumber 解码，保精度**
  - `jsonObject(s, flagName string) (map[string]any, error)`：替代 `structData`，UseNumber 解码 JSON 对象为 map
  - `mergeJSON(m map[string]any, data string) error`：替代 `mergeData`；data 为空不做事；用 UseNumber 解码后**覆盖合并**（--data 优先）
  - `setChanged(cmd *cobra.Command, flag string, m map[string]any, key string, v any)`：替代 changedBoolPtr/changedInt32Ptr/changedStringPtr——`cmd.Flags().Changed(flag)` 为真时 `m[key] = v`
  - `listJSON(pageSize int32, pageToken string) map[string]any`：替代 `buildListRequest`，非零才放 `pageSize`/`pageToken` 键
  - 方法名字面量约定：每个 cmd 文件顶部 `const methodXxx = "/torchwood.server.v1.XxxService/Yyy"`

- [ ] **Step 1: 改造测试**

```go
// cmd_users_test.go 要点（builders 改为返回 map[string]any / error）：
func TestBuildCreateUserReq(t *testing.T) {
	req, err := buildCreateUserReq("a@b.c", "pw", "n", "active", `{"labels":{"x":1}}`)
	require.NoError(t, err)
	require.Equal(t, "a@b.c", req["email"])
	require.Equal(t, map[string]any{"x": json.Number("1")}, req["labels"])
}

func TestJSONInt64MapPrecision(t *testing.T) {
	m, err := jsonInt64Map(`{"big": 1234567890123456789}`, "--increment")
	require.NoError(t, err)
	require.Equal(t, json.Number("1234567890123456789"), m["big"]) // 不经 float64
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./cmd/client/ -run 'TestBuild|TestJSON'`
Expected: FAIL。

- [ ] **Step 3: 实现**

`cmd_helpers.go` 全文重写：

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// decodeJSON 以 UseNumber 解码，避免 64 位整型经 float64 丢精度。
func decodeJSON(s string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	return dec.Decode(v)
}

// jsonStringList 解析 JSON 字符串数组 flag。
func jsonStringList(s, flagName string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%v", flagName, err)
	}
	return out, nil
}

// jsonStringMap 解析 JSON 对象（string->string）flag。
func jsonStringMap(s, flagName string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]string
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%v", flagName, err)
	}
	return out, nil
}

// jsonInt64Map 解析 JSON 对象（string->int64）flag；json.Number 保持精度。
func jsonInt64Map(s, flagName string) (map[string]json.Number, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]json.Number
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%v", flagName, err)
	}
	return out, nil
}

// jsonObject 解析 JSON 对象 flag 为 map（UseNumber）。
func jsonObject(s, flagName string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var out map[string]any
	if err := decodeJSON(s, &out); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%v", flagName, err)
	}
	return out, nil
}

// mergeJSON 把 --data JSON 覆盖合并进请求 map（--data 与 flag 冲突时以 --data 为准）。
func mergeJSON(m map[string]any, data string) error {
	if data == "" {
		return nil
	}
	var dm map[string]any
	if err := decodeJSON(data, &dm); err != nil {
		return fmt.Errorf("--data 解析失败：%v", err)
	}
	for k, v := range dm {
		m[k] = v
	}
	return nil
}

// setChanged 在 flag 显式设置时写入请求 map（proto3 optional presence 用键存在性表达）。
func setChanged(cmd *cobra.Command, flag string, m map[string]any, key string, v any) {
	if cmd.Flags().Changed(flag) {
		m[key] = v
	}
}

// listJSON 构造分页请求 map（仅放非零键）。
func listJSON(pageSize int32, pageToken string) map[string]any {
	m := map[string]any{}
	if pageSize > 0 {
		m["pageSize"] = pageSize
	}
	if pageToken != "" {
		m["pageToken"] = pageToken
	}
	return m
}
```

`cmd_users.go` 迁移（每个命令模式一致，示例两个）：

```go
const (
	methodUsersList   = "/torchwood.server.v1.UsersService/ListUsers"
	methodUsersGet    = "/torchwood.server.v1.UsersService/GetUser"
	methodUsersCreate = "/torchwood.server.v1.UsersService/CreateUser"
	// …其余 6 个方法同
)

// users list
RunE: func(cmd *cobra.Command, args []string) error {
	resp, err := invoke(g, methodUsersList, listJSON(pageSize, pageToken))
	if err != nil {
		return err
	}
	return printJSON(os.Stdout, resp)
},

// users create
func buildCreateUserReq(email, password, name, status, data string) (map[string]any, error) {
	if email == "" || password == "" {
		return nil, fmt.Errorf("--email 与 --password 必填")
	}
	req := map[string]any{"email": email, "password": password}
	if name != "" {
		req["name"] = name
	}
	if status != "" {
		req["status"] = status
	}
	if err := mergeJSON(req, data); err != nil {
		return nil, err
	}
	return req, nil
}
```

`users update`：`req := map[string]any{"id": id}`；`setChanged(cmd, "email-verified", req, "emailVerified", emailVerified)`；name/email/status 非空才放；`mergeJSON(req, data)`。
`sessions delete`：`{"id": args[0], "sessionId": args[1]}`。

`cmd_projects.go`：`projects list` 用 `listJSON`；`projects get` 用 `{"id": args[0]}`；删除文件内 `buildListRequest`（被 listJSON 取代）。

- [ ] **Step 4: 局部验证**

Run: `go test ./cmd/client/ -run 'TestBuild|TestJSON'`
Expected: 相关测试 PASS（其余文件未迁移，编译若受阻可先把未迁移文件的 RunE 临时指向 `return errors.New("migrating")`？——**不允许临时桩**。改为：本任务一次性把 helpers + users + projects 改完，`go vet` 这三个文件：`go test ./cmd/client/ -run 'Users|Projects|Helpers|Build|JSON'` 在剩余文件编译错误修复前无法运行；接受该状态，验证推迟到 Task 14。）

- [ ] **Step 5: 暂缓提交**（并入 Task 14）

---

### Task 12: CLI cmd_teams.go + cmd_oauth.go 迁移

**Files:**
- Modify: `cmd/client/cmd_teams.go`、`cmd/client/cmd_oauth.go`
- Modify: `cmd/client/cmd_teams_test.go`

**Interfaces:**
- Consumes: Task 11 helpers（`jsonStringList`/`jsonObject`/`listJSON`/`mergeJSON`/`setChanged`）。

- [ ] **Step 1-3: 逐命令迁移**（模式同 Task 11，逐条对应）

- `teams create`：`{"name": name}` + permissions 非空时 `req["permissions"] = perms`（`jsonStringList`）
- `teams list`：`listJSON`；`teams get/delete`：`{"id": ...}`
- `teams prefs get`：`{"id": ...}`；`teams prefs update`：`{"id": ...}` + `jsonObject(prefs, "--prefs")` 放 `req["prefs"]`
- `teams memberships create`：`{"teamId": ...}` + userId/email/name 非空才放 + roles；`list`：`{"teamId": ...}` + queries 非空放 `req["queries"]`；`get/delete`：`{"teamId":..., "membershipId":...}`；`update`：+ roles；`update-status`：+ `{"status": ...}`
- `oauth-providers list`：`listJSON`；`upsert`：`{"provider": ...}` + 其余字段按 flag 非空/Changed 放 + scopes；`delete`：`{"provider": ...}`

字段名一律 camelCase（protojson 键）。测试 `cmd_teams_test.go` 同步改为断言 map。

- [ ] **Step 4: 局部验证**（同 Task 11，推迟到 Task 14）
- [ ] **Step 5: 暂缓提交**

---

### Task 13: CLI cmd_databases.go 迁移（int64 精度重点）

**Files:**
- Modify: `cmd/client/cmd_databases.go`
- Modify: `cmd/client/cmd_databases_test.go`

**Interfaces:**
- Consumes: Task 11 helpers。

- [ ] **Step 1-3: 逐命令迁移**

全部 22 个 DatabasesService 方法对应的命令按 Task 11 模式迁移，要点：

- `collections create/update`：`setChanged(cmd, "document-security", req, "documentSecurity", v)`、`setChanged(cmd, "disabled", req, "disabled", v)`（optional bool）
- `documents create/update/upsert/bulk-update`：`jsonObject(data, "--data")` 放 `req["data"]`；permissions/conflictColumns/documentIds 用 `jsonStringList`
- **`documents update` 的 `--increment`**：`jsonInt64Map` 返回 `map[string]json.Number` 直接放 `req["increment"]`——encoding/json marshal 时 json.Number 原样输出数字，protojson 接受，>2^53 不丢精度
- `attributes create` 的 `--default-value`：保持现语义（JSON 文本原样作为值）：`var dv any; decodeJSON(defaultValue, &dv); req["defaultValue"] = dv`
- `indexes create`：attributes/orders 用 `jsonStringList`
- 各 list：`listJSON` 或 `{"databaseId":..., "collectionId":...}` + queries

测试新增精度回归：

```go
func TestDocumentsUpdateIncrementPrecision(t *testing.T) {
	req, err := buildUpdateDocumentReq("db1", "col1", "doc1", "", `{"big": 1234567890123456789}`, "")
	require.NoError(t, err)
	b, err := json.Marshal(req)
	require.NoError(t, err)
	require.Contains(t, string(b), "1234567890123456789") // 不是 1.23...e+18
}
```

（`buildUpdateDocumentReq` 等 builder 函数名沿用现状，返回类型改 `map[string]any`。）

- [ ] **Step 4: 局部验证**（推迟到 Task 14）
- [ ] **Step 5: 暂缓提交**

---

### Task 14: CLI cmd_storage.go + cmd_functions.go 迁移（bytes 重点）+ 整体收尾

**Files:**
- Modify: `cmd/client/cmd_storage.go`、`cmd/client/cmd_functions.go`
- Modify: `cmd/client/cmd_storage_test.go`、`cmd/client/cmd_functions_test.go`
- Create: `cmd/client/import_guard_test.go`

**Interfaces:**
- Consumes: Task 11 helpers。

- [ ] **Step 1-3: 逐命令迁移**

- storage：`buckets create`（permissions 用 jsonStringList）；`buckets update` 的 `--public` 用 `setChanged`；`files update` 的 `--metadata` 用 jsonStringMap 放 `req["metadata"]`，`--name`/`--mime-type` 用 `setChanged`；`usage`：`{}` 或按 GetStorageUsageRequest 字段
- functions：`create/update` 的 `--timeout-seconds`（`setChanged(..., "timeoutSeconds", v)`）、`--spec`、`--enabled` 同；`variables set`：`jsonStringMap` 得到 map[string]string 后转为变量数组——proto `SetVariablesRequest` 的 variables 是 `[]*Variable`，protojson 形如 `[{"key":"k","value":"v"}]`，因此：

```go
vars, err := jsonStringMap(varsFlag, "--vars")
list := make([]map[string]string, 0, len(vars))
for k, v := range vars {
	list = append(list, map[string]string{"key": k, "value": v})
}
req["variables"] = list
```

- **`deployments create` 的 `--code <zip-file>`（bytes 字段）**：保留文件读取体验，base64 后放入 map：

```go
func buildCreateDeploymentReq(functionID, codePath string) (map[string]any, error) {
	code, err := os.ReadFile(codePath)
	if err != nil {
		return nil, fmt.Errorf("读取 --code 失败：%v", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("--code 为空文件")
	}
	if len(code) > 50<<20 {
		return nil, fmt.Errorf("--code 超过 50MiB，请拆分或使用对象存储")
	}
	return map[string]any{
		"functionId": functionID,
		"code":       base64.StdEncoding.EncodeToString(code),
	}, nil
}
```

- `executions create`：`--deployment-id`、`--async` 用 setChanged；`--input` 字符串放 `req["data"]`（保持现语义）

- [ ] **Step 4: 全量验证**

import 兜底测试：

```go
// cmd/client/import_guard_test.go
package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoProtoGRPCImports 兜底：CLI 源码不得直接 import genproto/grpc/protobuf。
func TestNoProtoGRPCImports(t *testing.T) {
	forbidden := []string{
		"github.com/torchwoodcloud/torchwood/genproto",
		"google.golang.org/grpc",
		"google.golang.org/protobuf",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "import_guard_test.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports forbidden package %s", name, path)
				}
			}
		}
	}
}
```

Run（仓库根）:

```bash
go mod tidy
go build ./cmd/client/
go vet ./cmd/client/
go test ./cmd/client/ -cover
```

Expected: 全部 PASS；`go test` 含 import 兜底与 int64 精度回归。

- [ ] **Step 5: 分组提交**

```bash
git add cmd/client/output.go cmd/client/cmd_health.go cmd/client/cmd_rpc.go cmd/client/output_test.go cmd/client/conn.go cmd/client/registry.go cmd/client/registry_test.go
git commit -m "refactor(cli): route rpc/health commands through sdk/go InvokeJSON"

git add cmd/client/cmd_helpers.go cmd/client/cmd_users.go cmd/client/cmd_projects.go cmd/client/cmd_users_test.go
git commit -m "refactor(cli): migrate users/projects commands to JSON map requests"

git add cmd/client/cmd_teams.go cmd/client/cmd_oauth.go cmd/client/cmd_teams_test.go
git commit -m "refactor(cli): migrate teams/oauth-providers commands to JSON map requests"

git add cmd/client/cmd_databases.go cmd/client/cmd_databases_test.go
git commit -m "refactor(cli): migrate databases commands, preserve int64 precision via json.Number"

git add cmd/client/cmd_storage.go cmd/client/cmd_functions.go cmd/client/cmd_storage_test.go cmd/client/cmd_functions_test.go cmd/client/import_guard_test.go
git commit -m "refactor(cli): migrate storage/functions commands, add proto-import guard test"
```

（删除的 conn.go/registry.go/registry_test.go 在第一个 commit 中 `git add` 即记录删除。）

---

### Task 15: 端到端验证 + Taskfile 任务确认

**Files:** 无（纯验证）

- [ ] **Step 1: sdk/go 全量**

Run: `cd sdk/go && go test ./... -cover`
Expected: PASS

- [ ] **Step 2: CLI 构建与测试（对齐 Taskfile）**

Run（仓库根）:

```bash
go build -o ./bin/torchwood.exe ./cmd/client
go test ./cmd/client/... -cover
```

Expected: 编译成功、测试 PASS。

- [ ] **Step 3: 手工冒烟（可选，需本地服务端运行）**

```bash
./bin/torchwood.exe health get --endpoint 127.0.0.1:9060
./bin/torchwood.exe rpc /torchwood.server.v1.HealthService/Check
```

Expected: 输出缩进 JSON，与改造前格式一致。无本地服务端则跳过并在任务记录中注明。

---

### Task 16: 文档更新

**Files:**
- Modify: `docs/developer/12-sdk.md`（Go SDK 部分重写：client/server 包、token 管理、InvokeJSON、CLI 依赖关系）
- Modify: `AGENTS.md`（`cmd/client/` 条目：改为「通过 sdk/go（server 包 InvokeJSON）调用 Server API；方法覆盖完整性由 sdk/go server 包测试保证，新增 RPC 无需登记」）
- Modify: `sdk/README.md`（Go SDK 快速开始示例改为 client.New/server.New）

- [ ] **Step 1: 更新三个文档**

`docs/developer/12-sdk.md` Go SDK 部分要点：
- 包结构：`sdk/go/client`（end-user，自动刷新）与 `sdk/go/server`（API Key，InvokeJSON）
- 认证头：client 为 `authorization: Bearer`，server 为 `x-api-key` + 可选 `x-torchwood-project`
- token 管理：TokenStore 接口 / MemoryTokenStore / FileTokenStore / OnTokensChanged / 主动刷新 + 401 重试语义
- InvokeJSON 用法与错误形态（unknown method / protojson 错误 / status 错误）
- CLI 依赖关系：cmd/client 只依赖 sdk/go

`AGENTS.md` 对应条目替换为：

```markdown
- `cmd/client/`：Torchwood CLI 二进制（`bin/torchwood`），cobra 实现，通过 sdk/go（server 包 InvokeJSON）以 API Key 调用 Server API；CLI 源码不直接 import genproto/grpc（有 import_guard_test 兜底），方法覆盖完整性由 `sdk/go/server` 的测试保证，新增 RPC 无需在 CLI 登记。
```

`sdk/README.md` 快速开始：

```go
// Server API（API Key）
srv, _ := server.New("127.0.0.1:9060", server.WithAPIKey("..."))
resp, _ := srv.InvokeJSON(ctx, "/torchwood.server.v1.HealthService/Check", nil)

// Client API（账号密码登录，自动刷新）
store := client.NewFileTokenStore("~/.torchwood/tokens.json")
cli, _ := client.New("127.0.0.1:9060", client.WithProjectID("default"), client.WithTokenStore(store))
_, _ = cli.Account.SignIn(ctx, "user@example.com", "password")
me, _ := cli.Account.Me(ctx) // token 过期自动刷新
```

- [ ] **Step 2: Commit**

```bash
git add docs/developer/12-sdk.md AGENTS.md sdk/README.md
git commit -m "docs: update sdk/go restructure references (12-sdk, AGENTS, sdk README)"
```

---

## Self-Review 记录

- **Spec 覆盖**：包结构（T1-9）、TokenStore（T6）、自动刷新（T7）、Account 行为（T8）、InvokeJSON 动态分发（T3）、类型化服务补齐（T4-5）、CLI 切换含 UseNumber/bytes/import 兜底（T10-14）、go.mod（T10）、文档（T16）——全覆盖。
- **类型一致性**：`invoke(g, method, req any)`、`listJSON`、`mergeJSON`、`setChanged`、`jsonInt64Map` 返回 `map[string]json.Number`、`buildCreateDeploymentReq` 返回 `map[string]any`——前后任务一致。
- **已知取舍**：Task 10-14 中间状态编译不过，验证集中在 Task 14 Step 4；SDK errors.go 的 `ErrorCode`/`IsPermissionDenied` 在 Task 10 新增（列入该任务文件清单：`sdk/go/server/errors.go`，需随 Task 10 的 go.mod commit 之后、CLI commit 之前补一个 SDK commit：`git add sdk/go/server/errors.go && git commit -m "feat(sdk-go): add IsPermissionDenied error helper"`）。
