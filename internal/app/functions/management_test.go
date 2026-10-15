package functions

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ——管理面单测（v4 §3 target 模型 + §4 v2 双签轮换）——

func TestCreateFunctionWithSecret(t *testing.T) {
	f := newTestFunctions(t, nil, &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})

	// 服务端生成密钥：一次性明文回显 + 指纹 + 加密落库。
	fn, secret, err := f.CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
		ID: "fn_new", ProjectID: "p1", Name: "fn",
		TargetURL:      "https://fn.example.com",
		DeclaredScopes: []string{"assets:write"},
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret, domainfunctions.SecretPrefix), "服务端生成密钥带 twfnsec_ 前缀")
	require.Equal(t, domainfunctions.Fingerprint(secret), fn.TargetSecretFingerprint)
	dec, err := secretbox.Decrypt(fn.TargetSecretEnc, testEncKey)
	require.NoError(t, err)
	require.Equal(t, secret, dec, "密钥本体 secretbox 加密落库（实施勘误）")
	require.True(t, fn.TargetSecretRotatedAt.IsZero(), "创建非轮换")

	// 透传录入密钥。
	custom := "my-own-secret-material-32b"
	fn2, secret2, err := f.CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
		ID: "fn_own", ProjectID: "p1", Name: "fn", TargetURL: "https://fn.example.com", TargetSecret: custom,
	})
	require.NoError(t, err)
	require.Equal(t, custom, secret2)
	require.Equal(t, domainfunctions.Fingerprint(custom), fn2.TargetSecretFingerprint)

	// 录入密钥过短拒绝。
	_, _, err = f.CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
		ID: "fn_short", ProjectID: "p1", Name: "fn", TargetURL: "https://fn.example.com", TargetSecret: "short",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 必填校验。
	_, _, err = f.CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{ID: "fn_x", ProjectID: "p1", Name: "fn"})
	require.ErrorContains(t, err, "target_url is required")
}

func TestValidateTargetURL(t *testing.T) {
	// 生产：https 强制。
	require.NoError(t, validateTargetURL("https://fn.example.com/x", config.EnvProduction))
	err := validateTargetURL("http://fn.example.com", config.EnvProduction)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "https")

	// 开发：http 放行。
	require.NoError(t, validateTargetURL("http://127.0.0.1:18080", config.EnvDevelopment))

	// 形状：非法 scheme / 缺 host / 超长。
	require.Error(t, validateTargetURL("ftp://fn.example.com", config.EnvProduction))
	require.Error(t, validateTargetURL("https://", config.EnvProduction))
	err = validateTargetURL("https://"+strings.Repeat("x", 2100), config.EnvProduction)
	require.ErrorContains(t, err, "exceeds maximum")
}

func TestRotateFunctionSecret(t *testing.T) {
	fn, _, err := newTestFunctions(t, nil, &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{}).
		CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
			ID: "fn_rot", ProjectID: "p1", Name: "fn", TargetURL: "https://fn.example.com",
		})
	require.NoError(t, err)
	repo := &fakeRepo{fn: fn}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	oldEnc := fn.TargetSecretEnc

	// 轮换：新钥即效 + 旧钥移入 previous + rotated_at 置位。
	fn2, secret2, err := f.RotateFunctionSecret(serverCtx(), "p1", "fn_rot")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret2, domainfunctions.SecretPrefix))
	require.Equal(t, oldEnc, fn2.TargetPreviousSecretEnc)
	require.NotEqual(t, oldEnc, fn2.TargetSecretEnc)
	require.False(t, fn2.TargetSecretRotatedAt.IsZero())
	require.Equal(t, domainfunctions.Fingerprint(secret2), fn2.TargetSecretFingerprint)

	// 双签窗口：宽限内旧钥随 v2 通道携带。
	current, previous, err := f.decryptTargetSecrets(fn2)
	require.NoError(t, err)
	require.Equal(t, secret2, current)
	require.NotEmpty(t, previous, "宽限窗内旧钥应解密携带")

	// 窗外：previous 不再携带。
	fn2.TargetSecretRotatedAt = f.nowFn().Add(-25 * time.Hour)
	_, previous, err = f.decryptTargetSecrets(fn2)
	require.NoError(t, err)
	require.Empty(t, previous, "宽限窗外旧钥收敛（v2 通道关闭）")

	// 二次轮换链。
	require.NoError(t, err)
}

func TestUpdateFunction_TargetAndClientPolicy(t *testing.T) {
	fn, _, err := newTestFunctions(t, nil, &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{}).
		CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
			ID: "fn_upd", ProjectID: "p1", Name: "fn", TargetURL: "https://old.example.com",
		})
	require.NoError(t, err)
	repo := &fakeRepo{fn: fn}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})

	// url 可变；密钥不动（轮换独立 RPC——与 webhooks Update/Rotate 分工一致）。
	fn2, err := f.UpdateFunction(serverCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_upd",
		Name:      strptr("renamed"),
		TargetURL: strptr("https://new.example.com"),
	})
	require.NoError(t, err)
	require.Equal(t, "renamed", fn2.Name)
	require.Equal(t, "https://new.example.com", fn2.TargetURL)
	require.Equal(t, fn.TargetSecretFingerprint, fn2.TargetSecretFingerprint)

	// 客户端调用面：callable 要求 limit>=1。
	_, err = f.UpdateFunction(serverCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_upd", ClientCallable: boolptr(true),
	})
	require.ErrorContains(t, err, "client_per_user_limit >= 1")

	fn3, err := f.UpdateFunction(serverCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_upd",
		ClientCallable: boolptr(true), ClientPerUserLimit: intptr(10), ClientLimitWindow: strptr("hour"),
	})
	require.NoError(t, err)
	require.True(t, fn3.ClientCallable)
	require.Equal(t, 10, fn3.ClientPerUserLimit)
	require.Equal(t, domainfunctions.ClientLimitWindowHour, fn3.ClientLimitWindow)

	// anonymous 一期禁用。
	_, err = f.UpdateFunction(serverCtx(), UpdateFunctionCommand{
		ProjectID: "p1", FunctionID: "fn_upd", ClientAnonymousAllowed: boolptr(true),
	})
	require.ErrorContains(t, err, "not open in this release")
}

func TestClientInvoke_QuotaFallbackAndIdempotency(t *testing.T) {
	fn := v4TestFunction()
	fn.ClientCallable = true
	fn.ClientPerUserLimit = 2
	repo := &fakeRepo{}
	inv := &fakeInvoker{}
	f := newTestFunctions(t, fn, repo, inv, &fakeNotifier{}, &fakeQueue{})
	userCtx := endUserCtx("user-1")

	// DB 降级计数路径（clientQuota 未装配）：前两次放行。
	for i := 0; i < 2; i++ {
		res, err := f.ClientInvoke(userCtx, ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
		require.NoError(t, err)
		require.False(t, res.Reused)
	}
	// 第三次：窗口内计数达上限 → ResourceExhausted + ErrorInfo.Reason。
	_, err := f.ClientInvoke(userCtx, ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Len(t, inv.invocations, 2, "超限请求不得触达函数调用")

	// 幂等冲突回读：repo 返回哨兵错误 + 既有行。
	repo2 := &fakeRepo{}
	repo2.fn = fn
	createErr := domainfunctions.ErrInvocationIdempotencyConflict
	repo2.createErr = createErr
	existing := &domainfunctions.InvocationRecord{ID: "inv_existing", Status: domainfunctions.InvocationStatusRunning}
	repo2.byIdem = existing
	f2 := newTestFunctions(t, fn, repo2, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})
	res, err := f2.ClientInvoke(userCtx, ClientInvokeCommand{ProjectID: "p1", FunctionID: "fn_1", IdempotencyKey: "k1"})
	require.NoError(t, err)
	require.True(t, res.Reused, "幂等命中：既有行原样返回")
	require.Equal(t, "inv_existing", res.Record.ID)
}

func TestSetFunctionScopes(t *testing.T) {
	fn, _, err := newTestFunctions(t, nil, &fakeRepo{}, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{}).
		CreateFunctionWithSecret(serverCtx(), CreateFunctionCommand{
			ID: "fn_scopes", ProjectID: "p1", Name: "fn", TargetURL: "https://fn.example.com",
			DeclaredScopes: []string{"assets:write"},
		})
	require.NoError(t, err)
	repo := &fakeRepo{fn: fn}
	f := newTestFunctions(t, fn, repo, &fakeInvoker{}, &fakeNotifier{}, &fakeQueue{})

	// 空集 = 撤销全部平台访问（fail-closed 默认）。
	fn2, err := f.SetFunctionScopes(serverCtx(), "p1", "fn_scopes", nil)
	require.NoError(t, err)
	require.Empty(t, fn2.DeclaredScopes)

	// 非法词表拒绝。
	_, err = f.SetFunctionScopes(serverCtx(), "p1", "fn_scopes", []string{"bogus"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func strptr(s string) *string { return &s }
func boolptr(b bool) *bool    { return &b }
func intptr(i int) *int       { return &i }
