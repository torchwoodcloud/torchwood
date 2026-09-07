package interceptor

import (
	"context"
	"testing"

	validatepb "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func newValidateInterceptorForTest(t *testing.T) *ValidateInterceptor {
	t.Helper()
	v, err := NewValidateInterceptor()
	require.NoError(t, err)
	return v
}

// TestValidateInterceptorRejectsShapeViolations：buf.validate required 注解
// 违规 → InvalidArgument，消息以字段路径开头（经 HTTPErrorHandler 原样进
// error.message，CLI 退出码 2 契约依赖 code 而非文案）。
func TestValidateInterceptorRejectsShapeViolations(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/DeleteSession"}

	_, err := v.UnaryValidateMiddleware(context.Background(),
		&clientv1.DeleteSessionRequest{SessionId: ""}, info,
		func(ctx context.Context, req any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	// 精确契约：字段路径 + protovalidate required 默认文案（value is required）。
	require.Equal(t, "session_id: value is required", status.Convert(err).Message())

	_, err = v.UnaryValidateMiddleware(context.Background(),
		&clientv1.UpdatePrefsRequest{}, info,
		func(ctx context.Context, req any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "prefs: value is required", status.Convert(err).Message())
}

// TestValidateInterceptorPassesValidRequests：合规请求原样透传 handler。
func TestValidateInterceptorPassesValidRequests(t *testing.T) {
	t.Parallel()
	v := newValidateInterceptorForTest(t)
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.client.v1.AccountService/DeleteSession"}

	prefs, err := structpb.NewStruct(map[string]any{"theme": "dark"})
	require.NoError(t, err)
	called := false
	_, err = v.UnaryValidateMiddleware(context.Background(),
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
	_, err := v.UnaryValidateMiddleware(context.Background(),
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
	_, err := v.UnaryValidateMiddleware(context.Background(),
		"not-a-proto-message", info,
		func(ctx context.Context, req any) (any, error) {
			called = true
			return "ok", nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// TestValidateExemptPrefixes：白名单与 rateLimit/usage 维持同一口径。
func TestValidateExemptPrefixes(t *testing.T) {
	t.Parallel()
	require.False(t, validateExempt("/torchwood.client.v1.AccountService/DeleteSession"))
	require.True(t, validateExempt("/grpc.health.v1.Health/Check"))
	require.True(t, validateExempt("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"))
}

// TestFormatViolations：多条违规以 "; " 连接为单行（error.message 不引入
// 多行文本）。
func TestFormatViolations(t *testing.T) {
	t.Parallel()
	joined := formatViolations([]*protovalidate.Violation{
		{Proto: (&validatepb.Violation_builder{Message: proto.String("value is required")}).Build()},
		{Proto: (&validatepb.Violation_builder{Message: proto.String("must be a valid id")}).Build()},
	})
	require.Equal(t, "value is required; must be a valid id", joined)
}
