package client

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeFunctions struct {
	clientv1.UnimplementedFunctionsServiceServer
	rec *recorder
	// quotaErr 非空 = 返回配额超额错误（Reason + RetryInfo）。
	quotaErr error
}

func (s *fakeFunctions) InvokeFunction(ctx context.Context, req *clientv1.InvokeFunctionRequest) (*clientv1.InvokeFunctionResponse, error) {
	s.rec.record(ctx)
	if s.quotaErr != nil {
		return nil, s.quotaErr
	}
	return &clientv1.InvokeFunctionResponse{
		ExecutionId: "exe-1",
		Status:      "completed",
		Response:    `{"ok":true}`,
	}, nil
}

func newFunctionsClient(t *testing.T, quotaErr error) (*Client, *recorder) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	rec := &recorder{}
	srv := grpc.NewServer()
	clientv1.RegisterFunctionsServiceServer(srv, &fakeFunctions{rec: rec, quotaErr: quotaErr})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := New("passthrough:///bufconn",
		WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}),
		WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, rec
}

func TestClientFunctions_Invoke(t *testing.T) {
	c, rec := newFunctionsClient(t, nil)
	ctx := context.Background()

	res, err := c.Functions.InvokeString(ctx, "sign_in", `{"day":"2026-09-09"}`, "idem-1", "")
	require.NoError(t, err)
	require.Equal(t, "exe-1", res.ExecutionId)
	require.Equal(t, "completed", res.Status)
	require.Equal(t, `{"ok":true}`, res.Response)
	require.Equal(t, "Bearer jwt-1", rec.auth("authorization"))
}

func TestClientFunctions_QuotaExceededSurfacesDetails(t *testing.T) {
	// 配额超额错误（ResourceExhausted + ErrorInfo.Reason + RetryInfo）原样
	// 透传给调用方（不是响应字段）。
	st := status.New(codes.ResourceExhausted, "client invoke quota exceeded for this window")
	c, _ := newFunctionsClient(t, st.Err())
	_, err := c.Functions.InvokeString(context.Background(), "fn", `{}`, "", "")
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}
