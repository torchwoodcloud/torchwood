package interceptor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestAuditSummaryEligible(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		{"/torchwood.server.v1.UsersService/CreateUser", true},
		{"/torchwood.server.v1.UsersService/UpdateUser", true},
		{"/torchwood.server.v1.UsersService/DeleteUser", true},
		{"/torchwood.server.v1.OutboxService/ReplayDeadLetter", true},
		{"/torchwood.console.v1.ConsoleAuthService/SignIn", true},
		// 读方法跳过（Check 为读动词：健康检查）。
		{"/torchwood.server.v1.UsersService/ListUsers", false},
		{"/torchwood.server.v1.UsersService/GetUser", false},
		{"/torchwood.server.v1.HealthService/Check", false},
		// client 数据面不记请求内容。
		{"/torchwood.client.v1.DatabasesService/CreateDocument", false},
		{"/torchwood.client.v1.FunctionsService/InvokeFunction", false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, auditSummaryEligible(c.method), c.method)
	}
}

// TestAuditRowEligible 噪声治理准入表：日常无害操作（读浏览/框架探针/
// 数据面高频/例行刷新）不落审计行；管理面写与 client 面安全动作保留。
func TestAuditRowEligible(t *testing.T) {
	cases := []struct {
		method string
		want   bool
	}{
		// 框架内置：监控/探针轮询，不记。
		{"/grpc.health.v1.Health/Check", false},
		{"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo", false},
		// 管理面写：记。
		{"/torchwood.server.v1.UsersService/CreateUser", true},
		{"/torchwood.server.v1.UsersService/DeleteUser", true},
		{"/torchwood.server.v1.FunctionsService/UpdateFunction", true},
		{"/torchwood.console.v1.AdminsService/CreateAdmin", true},
		{"/torchwood.console.v1.ConsoleAuthService/SignIn", true},
		// 管理面读（Console/CLI 日常浏览）：不记。
		{"/torchwood.server.v1.UsersService/ListUsers", false},
		{"/torchwood.server.v1.UsersService/GetUser", false},
		{"/torchwood.server.v1.HealthService/Check", false},
		{"/torchwood.server.v1.HealthService/GetVersion", false},
		{"/torchwood.console.v1.AdminsService/ListAdmins", false},
		// client 面：仅 AccountService 非读安全动作。
		{"/torchwood.client.v1.AccountService/SignIn", true},
		{"/torchwood.client.v1.AccountService/SignUp", true},
		{"/torchwood.client.v1.AccountService/SignOut", true},
		{"/torchwood.client.v1.AccountService/DeleteAccount", true},
		{"/torchwood.client.v1.AccountService/CreateEmailOTPSession", true},
		{"/torchwood.client.v1.AccountService/DeleteSession", true},
		{"/torchwood.client.v1.AccountService/Me", false},
		{"/torchwood.client.v1.AccountService/ListSessions", false},
		{"/torchwood.client.v1.AccountService/ListLogs", false},
		// 例行刷新与偏好：高频无害，不记（异常由拒绝审计覆盖）。
		{"/torchwood.client.v1.AccountService/RefreshToken", false},
		{"/torchwood.client.v1.AccountService/UpdatePrefs", false},
		{"/torchwood.client.v1.AccountService/GetPrefs", false},
		// client 数据面：审计载体是事件流/业务记录，不记。
		{"/torchwood.client.v1.DatabasesService/CreateDocument", false},
		{"/torchwood.client.v1.DatabasesService/UpdateDocument", false},
		{"/torchwood.client.v1.FunctionsService/InvokeFunction", false},
		{"/torchwood.client.v1.FunctionsService/CreateExecution", false},
		{"/torchwood.client.v1.PaymentsService/CreateOrder", false},
		{"/torchwood.client.v1.GroupsService/ListGroupMemberships", false},
		// 未知命名空间：偏向多记。
		{"/torchwood.future.v1.ThingsService/DoThing", true},
		{"/test/Ok", true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, auditRowEligible(c.method), c.method)
	}
}

// TestAuditRequestSummary_RedactsSensitive：敏感字段（password 等）整值打码，
// 非敏感字段保真；摘要可被 JSON 解析（结构化、非文本约定）。
func TestAuditRequestSummary_RedactsSensitive(t *testing.T) {
	req := &serverv1.CreateUserRequest{
		Email:    "dev@example.com",
		Password: "super-secret-pw",
		Name:     "Dev",
	}
	summary := auditRequestSummary("/torchwood.server.v1.UsersService/CreateUser", req)
	require.NotEmpty(t, summary)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(summary), &parsed))
	require.Equal(t, "dev@example.com", parsed["email"])
	require.Equal(t, "[REDACTED]", parsed["password"])
	require.NotContains(t, summary, "super-secret-pw")
}

// TestAuditRequestSummary_PresenceSemantics：更新类请求（proto3 optional）
// 摘要只含被设置的字段——"改了什么"直接可读。
func TestAuditRequestSummary_PresenceSemantics(t *testing.T) {
	req := &serverv1.UpdateFunctionRequest{
		FunctionId:         "fn-1",
		ClientPerUserLimit: proto.Int32(20),
	}
	summary := auditRequestSummary("/torchwood.server.v1.FunctionsService/UpdateFunction", req)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(summary), &parsed))
	require.Equal(t, "fn-1", parsed["function_id"])
	require.Equal(t, float64(20), parsed["client_per_user_limit"])
	// 未设置的字段不出现（name/enabled/timeout_seconds 均未改）。
	require.NotContains(t, parsed, "name")
	require.NotContains(t, parsed, "enabled")
	require.NotContains(t, parsed, "timeout_seconds")
}

// TestAuditRequestSummary_TruncatesHugeStrings：超长字符串截断 + 标记，
// 整体不超过 8KB（覆盖 base64 后的 bytes 部署包形态）。
func TestAuditRequestSummary_TruncatesHugeStrings(t *testing.T) {
	req := &serverv1.UpdateFunctionRequest{
		FunctionId: "fn-1",
		Name:       proto.String(strings.Repeat("x", 32*1024)),
	}
	summary := auditRequestSummary("/torchwood.server.v1.FunctionsService/UpdateFunction", req)
	require.LessOrEqual(t, len(summary), 8*1024+len(auditTruncationSuffix))
	require.Contains(t, summary, auditTruncationSuffix)
	require.NotContains(t, summary, "[REDACTED]")
}

func TestAuditClientChannel(t *testing.T) {
	// Console 会话。
	c := auditClientChannel(&shared.Principal{CredentialType: shared.CredentialTypeSession}, "Mozilla/5.0")
	require.Equal(t, map[string]any{"channel": "console"}, c)

	// API key + CLI 自报 UA（grpc 会追加自身 token，取首段）。
	c = auditClientChannel(&shared.Principal{CredentialType: shared.CredentialTypeAPIKey}, "torchwood-cli/0.4.0 grpc-go/1.6")
	require.Equal(t, map[string]any{"channel": "cli", "product": "torchwood-cli", "version": "0.4.0"}, c)

	// API key + SDK UA。
	c = auditClientChannel(&shared.Principal{CredentialType: shared.CredentialTypeAPIKey}, "torchwood-sdk-go/0.4.0 grpc-go/1.6")
	require.Equal(t, map[string]any{"channel": "sdk", "product": "torchwood-sdk-go", "version": "0.4.0"}, c)

	// API key 无自报 → api。
	c = auditClientChannel(&shared.Principal{CredentialType: shared.CredentialTypeAPIKey}, "grpc-go/1.6")
	require.Equal(t, map[string]any{"channel": "api"}, c)

	// 函数执行身份。
	c = auditClientChannel(&shared.Principal{CredentialType: shared.CredentialTypeExecution}, "")
	require.Equal(t, map[string]any{"channel": "function"}, c)
}

// auditCaptureRepo 捕获插入的审计行（含 metadata）。
type auditCaptureRepo struct{ entries []*audit.Entry }

func (r *auditCaptureRepo) Insert(_ context.Context, e *audit.Entry) error {
	r.entries = append(r.entries, e)
	return nil
}
func (r *auditCaptureRepo) ListByActor(context.Context, string, string, int) ([]audit.Entry, error) {
	return nil, nil
}
func (r *auditCaptureRepo) List(context.Context, audit.ListFilter) ([]audit.Entry, int, error) {
	return nil, 0, nil
}

// TestUnaryAuditMiddleware_StructuredMetadata：管理面写操作的审计行合并三类
// 结构化 metadata——client（通道推导）、request（脱敏摘要）、changes（app
// 用例经 holder 回填的 before/after diff）；失败路径同样记录。
func TestUnaryAuditMiddleware_StructuredMetadata(t *testing.T) {
	repo := &auditCaptureRepo{}
	a := NewAuditInterceptor(repo)
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{
		IP:        "203.0.113.7",
		UserAgent: "torchwood-cli/0.4.0 grpc-go/1.6",
	})
	// UA/IP 读取以 incoming metadata 存在为前提（真实链路恒有）。
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("user-agent", "torchwood-cli/0.4.0 grpc-go/1.6"))
	ctx = contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:        "key-1",
		ActorKind:      shared.ActorKindService,
		ProjectID:      "proj-1",
		CredentialType: shared.CredentialTypeAPIKey,
	})
	req := &serverv1.CreateUserRequest{Email: "dev@example.com", Password: "pw"}
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.server.v1.UsersService/CreateUser"}

	_, err := a.UnaryAuditMiddleware(ctx, req, info, func(ctx context.Context, _ any) (any, error) {
		// handler/app 经 holder 回填 diff（试点：Functions Update 的通道）。
		contexts.SetAuditMetadata(ctx, "changes", map[string]any{
			"client_per_user_limit": map[string]any{"from": 10, "to": 20},
		})
		return nil, status.Error(codes.ResourceExhausted, "quota")
	})
	require.Error(t, err)

	require.Len(t, repo.entries, 1)
	e := repo.entries[0]
	require.Equal(t, "ResourceExhausted", e.Status)
	require.Equal(t, "203.0.113.7", e.IP)
	require.Equal(t, "key-1", e.ActorID)

	client, ok := e.Metadata["client"].(map[string]any)
	require.True(t, ok, "metadata.client 应为结构化 map")
	require.Equal(t, "cli", client["channel"])

	request, ok := e.Metadata["request"].(string)
	require.True(t, ok)
	require.Contains(t, request, "dev@example.com")
	require.Contains(t, request, "[REDACTED]")

	changes, ok := e.Metadata["changes"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"from": 10, "to": 20}, changes["client_per_user_limit"])
}

// TestUnaryAuditMiddleware_NoiseOpsNotAudited：日常无害操作（管理面读浏览、
// client 面 Me/数据面、例行刷新）不落审计行——噪声治理的中间件级验证。
func TestUnaryAuditMiddleware_NoiseOpsNotAudited(t *testing.T) {
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		CredentialType: shared.CredentialTypeSession,
	})
	for _, method := range []string{
		"/torchwood.server.v1.UsersService/ListUsers", // 管理面读
		"/torchwood.server.v1.HealthService/Check",    // 健康检查
		"/grpc.health.v1.Health/Check",                // 框架探针
		"/torchwood.client.v1.AccountService/Me",      // 高频自查
		"/torchwood.client.v1.AccountService/RefreshToken",
		"/torchwood.client.v1.DatabasesService/CreateDocument", // 数据面
	} {
		repo := &auditCaptureRepo{}
		a := NewAuditInterceptor(repo)
		info := &grpc.UnaryServerInfo{FullMethod: method}
		_, err := a.UnaryAuditMiddleware(ctx, &sharedv1.ListRequest{}, info, func(context.Context, any) (any, error) {
			return "resp", nil
		})
		require.NoError(t, err)
		require.Empty(t, repo.entries, "%s 不应落审计行", method)
	}
}

// TestUnaryAuditMiddleware_ClientSecurityOpsAudited：client 面安全动作
// （登录族）保留审计行——action 级（无 request 摘要）+ channel 推导照常，
// 端用户账号日志（GET /v1/account/logs）的数据来源。
func TestUnaryAuditMiddleware_ClientSecurityOpsAudited(t *testing.T) {
	repo := &auditCaptureRepo{}
	a := NewAuditInterceptor(repo)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		CredentialType: shared.CredentialTypeToken,
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/SignIn"}
	_, err := a.UnaryAuditMiddleware(ctx, &sharedv1.ListRequest{}, info, func(context.Context, any) (any, error) {
		return "resp", nil
	})
	require.NoError(t, err)
	require.Len(t, repo.entries, 1)
	e := repo.entries[0]
	require.Equal(t, "success", e.Status)
	require.NotContains(t, e.Metadata, "request", "client 面无请求摘要")
	require.Equal(t, map[string]any{"channel": "user"}, e.Metadata["client"])
}
