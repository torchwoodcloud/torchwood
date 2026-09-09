package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeAccount 记录 RefreshToken 调用次数，Me 可配置首次 401。
type fakeAccount struct {
	clientv1.UnimplementedAccountServiceServer
	refreshCalls atomic.Int32
	meCalls      atomic.Int32
	failFirstMe  atomic.Bool
	lastAuth     atomic.Value // []string
	mu           sync.Mutex
	lastReq      any // 最近一次 Account 请求消息（供断言）
	tokens       *clientv1.TokenBundle
	refreshErr   error
	signInResp   *clientv1.SignInResponse
	signInErr    error
	signUpResp   *clientv1.SignUpResponse
	signUpErr    error
	signOutErr   error
}

// storeReq 记录最近一次请求并返回，供测试断言。
func (f *fakeAccount) storeReq(req any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
}

// lastRequest 读取最近一次请求；断言前先 require.NotNil。
func (f *fakeAccount) lastRequest() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeAccount) SignIn(ctx context.Context, req *clientv1.SignInRequest) (*clientv1.SignInResponse, error) {
	if f.signInErr != nil {
		return nil, f.signInErr
	}
	if f.signInResp == nil {
		return &clientv1.SignInResponse{Account: &clientv1.Account{Id: "acc-1", Email: req.Email}}, nil
	}
	return f.signInResp, nil
}

func (f *fakeAccount) SignUp(ctx context.Context, req *clientv1.SignUpRequest) (*clientv1.SignUpResponse, error) {
	if f.signUpErr != nil {
		return nil, f.signUpErr
	}
	if f.signUpResp == nil {
		return &clientv1.SignUpResponse{Account: &clientv1.Account{Id: "acc-1", Email: req.Email, Name: req.Name}}, nil
	}
	return f.signUpResp, nil
}

func (f *fakeAccount) SignOut(ctx context.Context, _ *clientv1.SignOutRequest) (*sharedv1.Empty, error) {
	if f.signOutErr != nil {
		return nil, f.signOutErr
	}
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) RefreshToken(_ context.Context, req *clientv1.RefreshTokenRequest) (*clientv1.RefreshTokenResponse, error) {
	f.refreshCalls.Add(1)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if req.RefreshToken != "refresh-1" {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	return &clientv1.RefreshTokenResponse{Tokens: f.tokens}, nil
}

func (f *fakeAccount) Me(ctx context.Context, _ *clientv1.MeRequest) (*clientv1.Account, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.lastAuth.Store(md.Get("authorization"))
	if f.failFirstMe.Load() && f.meCalls.Add(1) == 1 {
		return nil, status.Error(codes.Unauthenticated, "expired")
	}
	return &clientv1.Account{Id: "acc-1"}, nil
}

// signInResp 返回携带 token 的登录响应（tokenBundle 见 account_test.go）。
func signInResp() *clientv1.SignInResponse {
	return &clientv1.SignInResponse{Account: &clientv1.Account{Id: "acc-1"}, Tokens: tokenBundle()}
}

func (f *fakeAccount) UpdateAccount(ctx context.Context, req *clientv1.UpdateAccountRequest) (*clientv1.Account, error) {
	f.storeReq(req)
	name, email := "", ""
	if req.Name != nil {
		name = *req.Name
	}
	if req.Email != nil {
		email = *req.Email
	}
	return &clientv1.Account{Id: "acc-1", Name: name, Email: email}, nil
}

func (f *fakeAccount) ConfirmEmailChange(ctx context.Context, req *clientv1.ConfirmEmailChangeRequest) (*clientv1.Account, error) {
	f.storeReq(req)
	return &clientv1.Account{Id: "acc-1", Email: "changed@example.com", EmailVerified: true}, nil
}

func (f *fakeAccount) ListSessions(ctx context.Context, _ *clientv1.ListSessionsRequest) (*clientv1.ListSessionsResponse, error) {
	return &clientv1.ListSessionsResponse{Sessions: []*clientv1.Session{{Id: "s1", Current: true}, {Id: "s2"}}}, nil
}

func (f *fakeAccount) DeleteSession(ctx context.Context, req *clientv1.DeleteSessionRequest) (*sharedv1.Empty, error) {
	f.storeReq(req)
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) DeleteSessions(ctx context.Context, req *clientv1.DeleteSessionsRequest) (*sharedv1.Empty, error) {
	f.storeReq(req)
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) GetPrefs(ctx context.Context, _ *clientv1.GetPrefsRequest) (*clientv1.GetPrefsResponse, error) {
	return &clientv1.GetPrefsResponse{Prefs: &structpb.Struct{Fields: map[string]*structpb.Value{
		"locale": structpb.NewStringValue("zh"),
	}}}, nil
}

func (f *fakeAccount) UpdatePrefs(ctx context.Context, req *clientv1.UpdatePrefsRequest) (*clientv1.GetPrefsResponse, error) {
	f.storeReq(req)
	return &clientv1.GetPrefsResponse{Prefs: req.Prefs}, nil
}

func (f *fakeAccount) CreateEmailOTP(ctx context.Context, req *clientv1.CreateEmailOTPRequest) (*clientv1.ChallengeResponse, error) {
	f.storeReq(req)
	return &clientv1.ChallengeResponse{ChallengeId: "ch-email"}, nil
}

func (f *fakeAccount) CreateEmailOTPSession(ctx context.Context, req *clientv1.CreateEmailOTPSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreateOAuth2Session(ctx context.Context, req *clientv1.CreateOAuth2SessionRequest) (*clientv1.CreateOAuth2SessionResponse, error) {
	f.storeReq(req)
	return &clientv1.CreateOAuth2SessionResponse{RedirectUrl: "https://oauth/authorize?state=s"}, nil
}

func (f *fakeAccount) CreateOAuth2TokenSession(ctx context.Context, req *clientv1.CreateOAuth2TokenSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreatePhoneOTP(ctx context.Context, req *clientv1.CreatePhoneOTPRequest) (*clientv1.ChallengeResponse, error) {
	f.storeReq(req)
	return &clientv1.ChallengeResponse{ChallengeId: "ch-phone"}, nil
}

func (f *fakeAccount) CreatePhoneOTPSession(ctx context.Context, req *clientv1.CreatePhoneOTPSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreateWeChatMiniProgramSession(ctx context.Context, req *clientv1.CreateWeChatMiniProgramSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreateAnonymousSession(ctx context.Context, req *clientv1.CreateAnonymousSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreateOAuth2LinkSession(ctx context.Context, req *clientv1.CreateOAuth2LinkSessionRequest) (*clientv1.CreateOAuth2SessionResponse, error) {
	f.storeReq(req)
	return &clientv1.CreateOAuth2SessionResponse{RedirectUrl: "https://oauth/link"}, nil
}

func (f *fakeAccount) CreateOAuth2LinkTokenSession(ctx context.Context, req *clientv1.CreateOAuth2LinkTokenSessionRequest) (*clientv1.Account, error) {
	f.storeReq(req)
	return &clientv1.Account{Id: "acc-1"}, nil
}

func (f *fakeAccount) CreateVerification(ctx context.Context, req *clientv1.CreateVerificationRequest) (*clientv1.CreateVerificationResponse, error) {
	f.storeReq(req)
	return &clientv1.CreateVerificationResponse{UserId: "acc-1"}, nil
}

func (f *fakeAccount) UpdateVerification(ctx context.Context, req *clientv1.UpdateVerificationRequest) (*clientv1.Account, error) {
	f.storeReq(req)
	return &clientv1.Account{Id: "acc-1"}, nil
}

func (f *fakeAccount) CreateRecovery(ctx context.Context, req *clientv1.CreateRecoveryRequest) (*sharedv1.Empty, error) {
	f.storeReq(req)
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) UpdateRecovery(ctx context.Context, req *clientv1.UpdateRecoveryRequest) (*sharedv1.Empty, error) {
	f.storeReq(req)
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) ListFactors(ctx context.Context, _ *clientv1.ListFactorsRequest) (*clientv1.ListFactorsResponse, error) {
	return &clientv1.ListFactorsResponse{Factors: []*clientv1.Factor{{Id: "f1", Type: "totp", Status: "verified"}}}, nil
}

func (f *fakeAccount) CreateTOTPFactor(ctx context.Context, _ *clientv1.CreateTOTPFactorRequest) (*clientv1.TOTPFactor, error) {
	return &clientv1.TOTPFactor{
		Factor:     &clientv1.Factor{Id: "f1", Type: "totp", Status: "pending"},
		Secret:     "JBSWY3DPEHPK3PXP",
		OtpauthUrl: "otpauth://totp/Torchwood:u@example.com?secret=JBSWY3DPEHPK3PXP",
	}, nil
}

func (f *fakeAccount) VerifyTOTPFactor(ctx context.Context, req *clientv1.VerifyTOTPFactorRequest) (*clientv1.Factor, error) {
	f.storeReq(req)
	return &clientv1.Factor{Id: req.FactorId, Type: "totp", Status: "verified"}, nil
}

// DeleteFactor 模拟真实服务端语义：verified 因子（本桩中 f1）缺 code 时拒绝。
func (f *fakeAccount) DeleteFactor(ctx context.Context, req *clientv1.DeleteFactorRequest) (*sharedv1.Empty, error) {
	f.storeReq(req)
	if req.FactorId == "f1" && req.Code == "" {
		return nil, status.Error(codes.InvalidArgument, "code is required for verified factor")
	}
	return &sharedv1.Empty{}, nil
}

func (f *fakeAccount) CreateMFASession(ctx context.Context, req *clientv1.CreateMFASessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) CreateJWT(ctx context.Context, _ *clientv1.CreateJWTRequest) (*clientv1.CreateJWTResponse, error) {
	return &clientv1.CreateJWTResponse{Token: "jwt-one-time"}, nil
}

func (f *fakeAccount) CreateMagicURLSession(ctx context.Context, req *clientv1.CreateMagicURLSessionRequest) (*clientv1.ChallengeResponse, error) {
	f.storeReq(req)
	return &clientv1.ChallengeResponse{ChallengeId: "ch-magic"}, nil
}

func (f *fakeAccount) UpdateMagicURLSession(ctx context.Context, req *clientv1.UpdateMagicURLSessionRequest) (*clientv1.SignInResponse, error) {
	f.storeReq(req)
	return signInResp(), nil
}

func (f *fakeAccount) ListLogs(ctx context.Context, req *clientv1.ListLogsRequest) (*clientv1.ListLogsResponse, error) {
	f.storeReq(req)
	return &clientv1.ListLogsResponse{Logs: []*clientv1.LogEntry{{Id: "log-1", Action: "SignIn", Status: "success"}}}, nil
}

// newBufconn 启动注册了 AccountService fake 的 bufconn gRPC 服务。
func newBufconn(t *testing.T) (*bufconn.Listener, *fakeAccount) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	fake := &fakeAccount{}
	srv := grpc.NewServer()
	clientv1.RegisterAccountServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis, fake
}

func dialer(lis *bufconn.Listener) grpc.DialOption {
	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
}

func newTestClient(t *testing.T, opts ...Option) (*Client, *fakeAccount) {
	t.Helper()
	lis, fake := newBufconn(t)
	opts = append(opts, WithDialOptions(dialer(lis)))
	c, err := New("passthrough:///bufconn", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c, fake
}
