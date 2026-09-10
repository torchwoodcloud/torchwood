package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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

// genSelfSigned 签发一张 127.0.0.1 的自编服务端证书（系统根证书不信任），
// 供 TestWithTLS 验证客户端真的走了 TLS 握手。
func genSelfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// TestWithTLS 固化连接层安全不变量：WithTLS 注入的凭据必须覆盖 conn.Dial
// 的 insecure 缺省（附加 dialOptions 在缺省之后生效）。若该顺序被破坏，
// 客户端会以明文 h2c 完成对 TLS 端口的调用——对自编证书 TLS 服务端的
// 一次健康检查即可区分两种情况：真 TLS 报证书信任错误，明文则成功。
func TestWithTLS(t *testing.T) {
	// 不用 newBufconn：它自带明文 fake server，会与本测试的 TLS server
	// 争抢同一 listener 上的连接。
	lis := bufconn.Listen(1 << 20)
	cert := genSelfSigned(t)
	srv := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	serverv1.RegisterHealthServiceServer(srv, &fakeServer{rec: &recorder{}})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := New("passthrough:///127.0.0.1", WithTLS(), WithRetryDisabled(),
		WithDialOptions(dialer(lis)))
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = c.Health.Check(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "x509")
}
