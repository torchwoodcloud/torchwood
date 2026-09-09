package interceptor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// memDenyAudit 是拦截器拒绝审计的内存 sink（M5 C6 测试用）。
type memDenyAudit struct {
	entries []*audit.Entry
}

func (m *memDenyAudit) Insert(_ context.Context, entry *audit.Entry) error {
	m.entries = append(m.entries, entry)
	return nil
}

var _ DenyAuditor = (*memDenyAudit)(nil)

const denyAuditTestMethod = "/torchwood.server.v1.UsersService/ListUsers"

// M5 C6：拦截器拒绝后必须落一条 denied 审计行，Metadata["reason"] 为拒绝
// 原因类别；Actor/Project 从已解析 principal 取。
func TestAuthInterceptor_DenyAudit(t *testing.T) {
	t.Parallel()

	t.Run("credential_missing", func(t *testing.T) {
		t.Parallel()
		sink := &memDenyAudit{}
		ic, err := newTestInterceptor(stubValidator{}, nil, []string{denyAuditTestMethod}, nil)
		requireNoError(t, err)
		ic.WithDenyAuditSink(sink)

		err = invokeAuth(ic, metadata.NewIncomingContext(context.Background(), metadata.MD{}), denyAuditTestMethod)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.Len(t, sink.entries, 1)
		e := sink.entries[0]
		require.Equal(t, "denied", e.Status)
		require.Equal(t, denyAuditTestMethod, e.Action)
		require.Equal(t, true, e.Metadata["denied"])
		require.Equal(t, "credential_missing", e.Metadata["reason"])
		require.Empty(t, e.ActorID, "认证前拒绝无 principal，Actor 应为空")
	})

	t.Run("admin_role_denied_carries_principal", func(t *testing.T) {
		t.Parallel()
		sink := &memDenyAudit{}
		principal := &shared.Principal{
			ActorID:        "admin-1",
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeToken,
			ProjectID:      "proj-1",
		}
		ic, err := newTestInterceptor(stubValidator{principal: principal}, nil, nil,
			map[string][]string{denyAuditTestMethod: {"owner"}})
		requireNoError(t, err)
		ic.WithDenyAuditSink(sink)

		// 端用户主体调 PERMISSION 面：principal 解析成功但角色不匹配。
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer tok"))
		err = invokeAuth(ic, ctx, denyAuditTestMethod)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Len(t, sink.entries, 1)
		e := sink.entries[0]
		require.Equal(t, "permission_denied", e.Metadata["reason"])
		require.Equal(t, "admin-1", e.ActorID, "拒绝审计应携带已解析 principal 的 Actor")
		require.Equal(t, "proj-1", e.ProjectID)
	})

	t.Run("nil_sink_writes_nothing", func(t *testing.T) {
		t.Parallel()
		ic, err := newTestInterceptor(stubValidator{}, nil, []string{denyAuditTestMethod}, nil)
		requireNoError(t, err)

		// WithDenyAuditSink(nil) 保持纯日志行为（既有测试不受影响）。
		ic.WithDenyAuditSink(nil)
		err = invokeAuth(ic, metadata.NewIncomingContext(context.Background(), metadata.MD{}), denyAuditTestMethod)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}

// WithDenyAuditSink 注入的 clientInfo 应进入审计行的 IP/UserAgent。
func TestAuthInterceptor_DenyAuditCarriesClientInfo(t *testing.T) {
	t.Parallel()
	sink := &memDenyAudit{}
	ic, err := newTestInterceptor(stubValidator{}, nil, []string{denyAuditTestMethod}, nil)
	requireNoError(t, err)
	ic.WithDenyAuditSink(sink)

	ctx := contexts.WithClientInfo(
		metadata.NewIncomingContext(context.Background(), metadata.MD{}),
		contexts.ClientInfo{IP: "203.0.113.7", UserAgent: "agent/9"},
	)
	_ = invokeAuth(ic, ctx, denyAuditTestMethod)
	require.Len(t, sink.entries, 1)
	require.Equal(t, "203.0.113.7", sink.entries[0].IP)
	require.Equal(t, "agent/9", sink.entries[0].UserAgent)
}
