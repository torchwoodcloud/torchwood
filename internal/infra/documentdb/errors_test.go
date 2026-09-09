package documentdb

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// sqlstateStub 是 pgdriver.Error 的本地替身（pgdriver.Error 无导出构造函数，
// 单测不得直接构造），实现最小接口 Field(byte) string（J4-6 下沉后 infra 层测试）。
type sqlstateStub struct{ state string }

func (s sqlstateStub) Error() string { return "sqlstate " + s.state }
func (s sqlstateStub) Field(b byte) string {
	if b == 'C' {
		return s.state
	}
	return ""
}

func TestMapPGError_SQLState(t *testing.T) {
	cases := []struct {
		state string
		code  codes.Code
	}{
		{"22P02", codes.InvalidArgument},
		{"22001", codes.InvalidArgument},
		{"23502", codes.InvalidArgument},
		{"42703", codes.InvalidArgument},
		{"42601", codes.InvalidArgument},
		{"42804", codes.InvalidArgument},
		{"23503", codes.InvalidArgument},
		{"42883", codes.InvalidArgument},
		{"42P10", codes.InvalidArgument},
		{"53100", codes.ResourceExhausted},
		{"53200", codes.ResourceExhausted},
		{"54000", codes.ResourceExhausted},
		{"53400", codes.ResourceExhausted},
	}
	for _, tc := range cases {
		err := MapError(sqlstateStub{state: tc.state})
		require.Equal(t, tc.code, status.Code(err), "state %s", tc.state)
		// 域码体系：消息携带稳定域码前缀（区分参数类与资源类），ErrorInfo
		// reason 同源并带 sqlstate/error_id。
		st := status.Convert(err)
		wantDomainCode := databases.ErrCodeInvalidArgument
		if tc.code == codes.ResourceExhausted {
			wantDomainCode = databases.ErrCodeExhausted
		}
		require.Contains(t, st.Message(), wantDomainCode, "state %s", tc.state)
		found := false
		for _, d := range st.Details() {
			info, ok := d.(*errdetails.ErrorInfo)
			if !ok {
				continue
			}
			found = true
			require.Equal(t, wantDomainCode, info.Reason, "state %s", tc.state)
			require.Equal(t, tc.state, info.Metadata["sqlstate"], "state %s", tc.state)
			require.NotEmpty(t, info.Metadata["error_id"], "state %s", tc.state)
		}
		require.True(t, found, "state %s: ErrorInfo detail missing", tc.state)
	}

	// 23505 特殊：映射为领域哨兵 ErrDuplicateKey，而非 generic status
	err23505 := MapError(sqlstateStub{state: "23505"})
	require.ErrorIs(t, err23505, databases.ErrDuplicateKey)
	require.Equal(t, databases.ErrDuplicateKey, err23505)

	// %w 包装链可穿透（errors.As）
	wrapped := fmt.Errorf("pg error: %w", sqlstateStub{state: "42703"})
	require.Equal(t, codes.InvalidArgument, status.Code(MapError(wrapped)))

	// 未收录 SQLSTATE → 原样透传
	unknown := sqlstateStub{state: "99999"}
	require.Equal(t, unknown, MapError(unknown))

	// 普通错误（无 Field 方法）→ 原样透传
	plain := errors.New("SQLSTATE 23505 unique constraint")
	require.Equal(t, plain, MapError(plain))

	// nil → nil
	require.Nil(t, MapError(nil))
}

func TestMapPGError_DuplicateKeyViaIsUniqueViolation(t *testing.T) {
	// isUniqueViolation 的 23505 也应被 MapError 转为 ErrDuplicateKey（兜底路径）
	err := MapError(fmt.Errorf("insert: %w", sqlstateStub{state: "23505"}))
	require.ErrorIs(t, err, databases.ErrDuplicateKey)
}

func TestMapError_PassthroughDomainErrors(t *testing.T) {
	// 领域哨兵原样透传（由 app 层统一映射为 status，此处仅验证不被 SQLSTATE 逻辑误转）
	for _, e := range []error{databases.ErrVersionMismatch, databases.ErrVersionRequired, databases.ErrPermissionDenied} {
		require.Equal(t, e, MapError(e))
		wrapped := fmt.Errorf("wrapped: %w", e)
		require.ErrorIs(t, MapError(wrapped), e)
		require.Equal(t, wrapped.Error(), MapError(wrapped).Error())
	}
}
