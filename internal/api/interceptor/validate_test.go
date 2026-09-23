package interceptor

import (
	"context"
	"testing"

	grpcapiinterceptor "github.com/lynx-go/grpcapi/interceptor"
	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// validate 拦截器行为矩阵（grpcapi 阶段 1 换库后）：实现本体在
// grpcapi/interceptor（同一 protovalidate 求值与错误格式化），本文件锁定
// torchwood 注解面消息契约不变（error.message 文案 = CLI 退出码 2 契约的
// 上游）。
func newValidateInterceptorForTest(t *testing.T) *grpcapiinterceptor.ValidateInterceptor {
	t.Helper()
	return grpcapiinterceptor.NewValidate()
}

// TestValidateInterceptorRejectsShapeViolations：buf.validate required 注解
// 违规 → InvalidArgument，消息以字段路径开头（经 HTTPErrorHandler 原样进
// error.message，CLI 退出码 2 契约依赖 code 而非文案）。
func TestValidateInterceptorRejectsShapeViolations(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/DeleteSession"}

	_, err := v.Unary()(context.Background(),
		&clientv1.DeleteSessionRequest{SessionId: ""}, info,
		func(ctx context.Context, req any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	// 精确契约：字段路径 + protovalidate required 默认文案（value is required）。
	require.Equal(t, "session_id: value is required", status.Convert(err).Message())

	_, err = v.Unary()(context.Background(),
		&clientv1.UpdatePrefsRequest{}, info,
		func(ctx context.Context, req any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "prefs: value is required", status.Convert(err).Message())
}

// TestValidateInterceptorRejectsShapeRules：数值范围（gte）与 repeated
// 数量（min_items）注解的求值（对应 client/server ListChanges.since_seq、
// ExecuteTransactions.ops、AggregateDocuments.aggregations 的上收）。
func TestValidateInterceptorRejectsShapeRules(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)

	cases := []struct {
		name    string
		method  string
		req     any
		wantMsg string
	}{
		{
			name:    "since_seq negative",
			method:  "/torchwood.client.v1.DatabasesService/ListChanges",
			req:     &clientv1.ListChangesRequest{SinceSeq: -1},
			wantMsg: "since_seq: must be greater than or equal to 0",
		},
		{
			name:    "ops empty",
			method:  "/torchwood.server.v1.DatabasesService/ExecuteTransactions",
			req:     &serverv1.ExecuteTransactionsRequest{},
			wantMsg: "ops: must contain at least 1 item(s)",
		},
		{
			name:    "aggregations empty",
			method:  "/torchwood.server.v1.DatabasesService/AggregateDocuments",
			req:     &serverv1.AggregateDocumentsRequest{},
			wantMsg: "aggregations: must contain at least 1 item(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := v.Unary()(context.Background(), tc.req,
				&grpc.UnaryServerInfo{FullMethod: tc.method},
				func(ctx context.Context, req any) (any, error) {
					t.Fatal("handler must not be reached on violation")
					return nil, nil
				})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Equal(t, tc.wantMsg, status.Convert(err).Message())
		})
	}
}

// TestValidateInterceptorPassesValidRequests：合规请求原样透传 handler。
func TestValidateInterceptorPassesValidRequests(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/DeleteSession"}

	prefs, err := structpb.NewStruct(map[string]any{"theme": "dark"})
	require.NoError(t, err)
	called := false
	_, err = v.Unary()(context.Background(),
		&clientv1.UpdatePrefsRequest{Prefs: prefs}, info,
		func(ctx context.Context, req any) (any, error) {
			called = true
			return "ok", nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// TestValidateInterceptorExemptsFrameworkServices：health/reflection 不校验
// （与 rateLimit/usage 白名单一致），非法形状也直达 handler。
func TestValidateInterceptorExemptsFrameworkServices(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	called := false
	_, err := v.Unary()(context.Background(),
		&clientv1.DeleteSessionRequest{SessionId: ""}, info,
		func(ctx context.Context, req any) (any, error) {
			called = true
			return "ok", nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// TestValidateInterceptorIgnoresNonProtoMessages：非 proto 消息（理论不可达）
// 不校验、直接放行。
func TestValidateInterceptorIgnoresNonProtoMessages(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/DeleteSession"}
	called := false
	_, err := v.Unary()(context.Background(),
		"not-a-proto-message", info,
		func(ctx context.Context, req any) (any, error) {
			called = true
			return "ok", nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// TestFrameworkExempt：白名单与 rateLimit/usage 维持同一口径（实现换库后
// 经库导出的 FrameworkExempt 断言）。
func TestFrameworkExempt(t *testing.T) {
	t.Parallel()
	require.False(t, grpcapiinterceptor.FrameworkExempt("/torchwood.client.v1.AccountService/DeleteSession"))
	require.True(t, grpcapiinterceptor.FrameworkExempt("/grpc.health.v1.Health/Check"))
	require.True(t, grpcapiinterceptor.FrameworkExempt("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"))
}

// 多条违规以 "; " 连接为单行（原 TestFormatViolations 直测私有格式化函数，
// 该函数已随实现上收进库，库侧 TestValidateRejectsInvalidRequest 覆盖同一
// 拼接行为）；本文件的 message 精确断言继续锁定 torchwood 链路对外文案。
