package server

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// recorder 记录 fake 服务收到的 metadata 与关键请求，供断言使用。
type recorder struct {
	mu                   sync.Mutex
	md                   metadata.MD
	lastCollection       *serverv1.CreateCollectionRequest
	lastCollectionUpdate *serverv1.UpdateCollectionRequest
	createdUser          *serverv1.CreateUserRequest
	lastUserPassword     *serverv1.UpdateUserPasswordRequest
	lastGroupPrefs       *serverv1.UpdateGroupPrefsRequest
	deletedAttributeKey  string
	deletedIndexID       string
	upserts              []*serverv1.UpsertDocumentRequest
	errs                 map[string]error // RPC 名 → 注入错误（fake 方法据此返回）
}

// setErr 为指定 RPC 注入错误（传 nil 清除）。
func (r *recorder) setErr(rpc string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.errs, rpc)
		return
	}
	if r.errs == nil {
		r.errs = make(map[string]error)
	}
	r.errs[rpc] = err
}

// fail 返回该 RPC 注入的错误；无注入返回 nil。
func (r *recorder) fail(rpc string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errs[rpc]
}

type fakeServer struct {
	serverv1.UnimplementedHealthServiceServer
	rec *recorder
}

func (f *fakeServer) Check(ctx context.Context, _ *serverv1.HealthCheckRequest) (*serverv1.HealthCheckResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.rec.mu.Lock()
	f.rec.md = md
	f.rec.mu.Unlock()
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
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, []string{"secret"}, rec.md.Get("x-api-key"))
	require.Equal(t, []string{"proj-1"}, rec.md.Get("x-torchwood-project"))
}

func TestNoHeadersWithoutConfig(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.Health.Check(context.Background())
	require.NoError(t, err)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Empty(t, rec.md.Get("x-api-key"))
	require.Empty(t, rec.md.Get("x-torchwood-project"))
}
