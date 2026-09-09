package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubExecutionTokens 是 ExecutionTokenService 的最小测试桩：返回预置 info
// 或错误（区分「token 不存在」与「基础设施故障」两条拒绝路径）。
type stubExecutionTokens struct {
	info *domainfunctions.ExecutionTokenInfo
	err  error
}

func (s *stubExecutionTokens) Mint(context.Context, domainfunctions.ExecutionTokenInfo, time.Duration) (string, error) {
	return "", nil
}

func (s *stubExecutionTokens) Validate(context.Context, string) (*domainfunctions.ExecutionTokenInfo, error) {
	return s.info, s.err
}

func (s *stubExecutionTokens) Revoke(context.Context, string) error { return nil }

func executionValidator(exec domainfunctions.ExecutionTokenService) *auth.Validator {
	return auth.NewValidatorWithOneTimeTokens(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil, nil, exec)
}

func TestValidateCredential_ExecutionTokenPrincipal(t *testing.T) {
	v := executionValidator(&stubExecutionTokens{info: &domainfunctions.ExecutionTokenInfo{
		ProjectID:      "p1",
		FunctionID:     "fn1",
		ExecutionID:    "e1",
		Scopes:         []string{"assets:write", "databases:read"},
		InvokingUserID: "u1",
	}})

	p, err := v.ValidateCredential(context.Background(), shared.ExecutionTokenPrefix+"abc", shared.CredentialTypeExecution)
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindExecution, p.ActorKind)
	require.Equal(t, shared.CredentialTypeExecution, p.CredentialType)
	require.Equal(t, "p1", p.ProjectID)
	require.Equal(t, "fn1", p.FunctionID)
	require.Equal(t, "e1", p.ExecutionID)
	require.Equal(t, "u1", p.InvokingUserID)
	require.True(t, p.IsAuthenticated())
	// Permissions 与 API key 同款权限串格式（<res>.<op>）——scope 门原样生效。
	require.Equal(t, []string{"assets.write", "databases.read"}, p.Permissions)
	// 数据面角色：keys 族 + key:function:<id>（DocRole 词表形态）。
	require.Equal(t, []string{databases.RoleKeys, "key:function:fn1"}, p.Roles)
	doc := p.DocPrincipal()
	require.Contains(t, doc.Roles, "key:function:fn1")
	require.Contains(t, doc.Roles, databases.RoleKeys)
}

func TestValidateCredential_ExecutionTokenRejected(t *testing.T) {
	// 未知 token → 401。
	v := executionValidator(&stubExecutionTokens{info: nil})
	_, err := v.ValidateCredential(context.Background(), shared.ExecutionTokenPrefix+"abc", shared.CredentialTypeExecution)
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 基础设施故障 → Internal（fail-closed，与 401 可区分）。
	v = executionValidator(&stubExecutionTokens{err: context.DeadlineExceeded})
	_, err = v.ValidateCredential(context.Background(), shared.ExecutionTokenPrefix+"abc", shared.CredentialTypeExecution)
	require.Equal(t, codes.Internal, status.Code(err))

	// 未装配服务（nil）→ 401。
	v = executionValidator(nil)
	_, err = v.ValidateCredential(context.Background(), shared.ExecutionTokenPrefix+"abc", shared.CredentialTypeExecution)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}
