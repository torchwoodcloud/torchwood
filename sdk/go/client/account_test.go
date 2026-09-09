package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func tokenBundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "jwt-1", RefreshToken: "rt-1", ExpiresAt: timestamppb.New(time.Unix(1893456000, 0))}
}

func TestNewClient_RequiresTarget(t *testing.T) {
	_, err := New("")
	require.Error(t, err)
}

func TestClientAccount_SignUp(t *testing.T) {
	c, _ := newTestClient(t, WithProjectID("proj-1"))

	resp, err := c.Account.SignUp(context.Background(), "alice@example.com", "s3cret", "Alice")
	require.NoError(t, err)
	require.Equal(t, "acc-1", resp.Account.Id)
	require.Equal(t, "alice@example.com", resp.Account.Email)
}

func TestSignInSavesTokens(t *testing.T) {
	var cb *clientv1.TokenBundle
	c, fake := newTestClient(t, WithProjectID("proj-1"), WithOnTokensChanged(func(b *clientv1.TokenBundle) { cb = b }))
	fake.signInResp = &clientv1.SignInResponse{
		Account: &clientv1.Account{Id: "acc-1", Email: "a@b.c"},
		Tokens:  tokenBundle(),
	}

	resp, err := c.Account.SignIn(context.Background(), "a@b.c", "pw")
	require.NoError(t, err)
	require.Equal(t, "jwt-1", resp.Tokens.AccessToken)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)
	require.Equal(t, "rt-1", got.RefreshToken)
	require.NotNil(t, cb)
	require.Equal(t, "jwt-1", cb.AccessToken)
}

func TestSignInMFADoesNotSaveTokens(t *testing.T) {
	called := false
	c, fake := newTestClient(t, WithOnTokensChanged(func(*clientv1.TokenBundle) { called = true }))
	fake.signInResp = &clientv1.SignInResponse{
		Account:     &clientv1.Account{Id: "acc-1"},
		MfaRequired: true,
	}

	resp, err := c.Account.SignIn(context.Background(), "a@b.c", "pw")
	require.NoError(t, err)
	require.True(t, resp.MfaRequired)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Nil(t, got)
	require.False(t, called)
}

func TestClientAccount_MeAttachesBearerToken(t *testing.T) {
	c, fake := newTestClient(t, WithInitialTokens(&clientv1.TokenBundle{AccessToken: "jwt-1"}))

	me, err := c.Account.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, "acc-1", me.Id)
	auth := fake.lastAuth.Load().([]string)
	require.Equal(t, []string{"Bearer jwt-1"}, auth)
}

func TestClientAccount_RefreshToken(t *testing.T) {
	c, fake := newTestClient(t)
	fake.tokens = &clientv1.TokenBundle{AccessToken: "jwt-2", RefreshToken: "rt-2"}

	resp, err := c.Account.RefreshToken(context.Background(), "refresh-1")
	require.NoError(t, err)
	require.Equal(t, "jwt-2", resp.Tokens.AccessToken)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-2", got.AccessToken)
}

func TestSignOutClearsOnSuccess(t *testing.T) {
	var cb *clientv1.TokenBundle
	c, _ := newTestClient(t, WithInitialTokens(tokenBundle()), WithOnTokensChanged(func(b *clientv1.TokenBundle) { cb = b }))

	require.NoError(t, c.Account.SignOut(context.Background()))
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Nil(t, got)
	require.Nil(t, cb)
}

func TestSignOutClearsOnUnauthenticated(t *testing.T) {
	c, fake := newTestClient(t, WithInitialTokens(tokenBundle()))
	fake.signOutErr = status.Error(codes.Unauthenticated, "session expired")

	err := c.Account.SignOut(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSignOutKeepsOnNetworkError(t *testing.T) {
	c, fake := newTestClient(t, WithInitialTokens(tokenBundle()))
	fake.signOutErr = status.Error(codes.Unavailable, "network down")

	err := c.Account.SignOut(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	got, err := c.store.Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "jwt-1", got.AccessToken)
}

func TestClientAccount_MeWithoutTokenOmitsAuthHeader(t *testing.T) {
	c, fake := newTestClient(t)

	_, err := c.Account.Me(context.Background())
	require.NoError(t, err)
	require.Nil(t, fake.lastAuth.Load())
}

func TestClientAccount_UpdateAccount(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	name := "New Name"
	email := "new@example.com"

	acc, err := c.Account.UpdateAccount(context.Background(), &name, &email, "", "", "https://app.example.com/confirm")
	require.NoError(t, err)
	require.Equal(t, "New Name", acc.Name)

	req, ok := fake.lastRequest().(*clientv1.UpdateAccountRequest)
	require.True(t, ok)
	require.NotNil(t, req.Name)
	require.Equal(t, "New Name", *req.Name)
	require.NotNil(t, req.Email)
	require.Equal(t, "new@example.com", *req.Email)
	require.Equal(t, "https://app.example.com/confirm", req.Url, "改邮箱时 url 必须透传")

	// nil 字段表示不修改
	_, err = c.Account.UpdateAccount(context.Background(), nil, nil, "pw", "old", "")
	require.NoError(t, err)
	req, ok = fake.lastRequest().(*clientv1.UpdateAccountRequest)
	require.True(t, ok)
	require.Nil(t, req.Name)
	require.Nil(t, req.Email)
	require.Equal(t, "pw", req.Password)
	require.Equal(t, "old", req.OldPassword)
}

func TestClientAccount_ConfirmEmailChange(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))

	acc, err := c.Account.ConfirmEmailChange(context.Background(), "acc-1", "secret-1")
	require.NoError(t, err)
	require.Equal(t, "changed@example.com", acc.Email)
	require.True(t, acc.EmailVerified)

	req, ok := fake.lastRequest().(*clientv1.ConfirmEmailChangeRequest)
	require.True(t, ok)
	require.Equal(t, "proj-1", req.ProjectId)
	require.Equal(t, "acc-1", req.UserId)
	require.Equal(t, "secret-1", req.Secret)
}

func TestClientAccount_SessionsLifecycle(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	resp, err := c.Account.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, resp.Sessions, 2)
	require.True(t, resp.Sessions[0].Current)

	require.NoError(t, c.Account.DeleteSession(ctx, "s2"))
	req, ok := fake.lastRequest().(*clientv1.DeleteSessionRequest)
	require.True(t, ok)
	require.Equal(t, "s2", req.SessionId)

	require.NoError(t, c.Account.DeleteSessions(ctx, true))
	req2, ok := fake.lastRequest().(*clientv1.DeleteSessionsRequest)
	require.True(t, ok)
	require.True(t, req2.KeepCurrent)

	require.NoError(t, c.Account.DeleteSessions(ctx, false))
	req2, ok = fake.lastRequest().(*clientv1.DeleteSessionsRequest)
	require.True(t, ok)
	require.False(t, req2.KeepCurrent)
}

func TestClientAccount_Prefs(t *testing.T) {
	c, _ := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	prefs, err := c.Account.GetPrefs(ctx)
	require.NoError(t, err)
	require.Equal(t, "zh", prefs.Prefs.GetFields()["locale"].GetStringValue())

	updated, err := c.Account.UpdatePrefs(ctx, map[string]any{"locale": "en", "n": 1})
	require.NoError(t, err)
	require.Equal(t, "en", updated.Prefs.GetFields()["locale"].GetStringValue())
	require.Equal(t, float64(1), updated.Prefs.GetFields()["n"].GetNumberValue())
}

func TestClientAccount_OTPLogin(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	ch, err := c.Account.CreateEmailOTP(ctx, "u@example.com")
	require.NoError(t, err)
	require.Equal(t, "ch-email", ch.ChallengeId)
	req, ok := fake.lastRequest().(*clientv1.CreateEmailOTPRequest)
	require.True(t, ok)
	require.Equal(t, "proj-1", req.ProjectId)
	require.Equal(t, "u@example.com", req.Email)

	resp, err := c.Account.CreateEmailOTPSession(ctx, "u@example.com", "ch-email", "123456")
	require.NoError(t, err)
	require.Equal(t, "acc-1", resp.Account.Id)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)
	req2, ok := fake.lastRequest().(*clientv1.CreateEmailOTPSessionRequest)
	require.True(t, ok)
	require.Equal(t, "ch-email", req2.ChallengeId)
	require.Equal(t, "123456", req2.Otp)

	chp, err := c.Account.CreatePhoneOTP(ctx, "+8613800138000")
	require.NoError(t, err)
	require.Equal(t, "ch-phone", chp.ChallengeId)
	_, err = c.Account.CreatePhoneOTPSession(ctx, "+8613800138000", "ch-phone", "654321")
	require.NoError(t, err)
	req3, ok := fake.lastRequest().(*clientv1.CreatePhoneOTPSessionRequest)
	require.True(t, ok)
	require.Equal(t, "654321", req3.Otp)
}

func TestClientAccount_OAuthSessions(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	resp, err := c.Account.CreateOAuth2Session(ctx, "google", "https://ok", "https://fail")
	require.NoError(t, err)
	require.Contains(t, resp.RedirectUrl, "https://oauth/")
	req, ok := fake.lastRequest().(*clientv1.CreateOAuth2SessionRequest)
	require.True(t, ok)
	require.Equal(t, "google", req.Provider)
	require.Equal(t, "https://ok", req.Success)

	_, err = c.Account.CreateOAuth2TokenSession(ctx, "google", "https://ok", "https://fail", "code-1", "state-1")
	require.NoError(t, err)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)
	req2, ok := fake.lastRequest().(*clientv1.CreateOAuth2TokenSessionRequest)
	require.True(t, ok)
	require.Equal(t, "code-1", req2.Code)
	require.Equal(t, "state-1", req2.State)

	link, err := c.Account.CreateOAuth2LinkSession(ctx, "github", "https://ok", "https://fail")
	require.NoError(t, err)
	require.Contains(t, link.RedirectUrl, "/link")
	_, err = c.Account.CreateOAuth2LinkTokenSession(ctx, "github", "code-2", "state-2")
	require.NoError(t, err)
	req4, ok := fake.lastRequest().(*clientv1.CreateOAuth2LinkTokenSessionRequest)
	require.True(t, ok)
	require.Equal(t, "code-2", req4.Code)
}

func TestClientAccount_WeChatAndAnonymousLogin(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	_, err := c.Account.CreateWeChatMiniProgramSession(ctx, "wx-code")
	require.NoError(t, err)
	req, ok := fake.lastRequest().(*clientv1.CreateWeChatMiniProgramSessionRequest)
	require.True(t, ok)
	require.Equal(t, "wx-code", req.Code)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)

	c2, fake2 := newTestClient(t, WithProjectID("proj-1"))
	_, err = c2.Account.CreateAnonymousSession(ctx)
	require.NoError(t, err)
	req2, ok := fake2.lastRequest().(*clientv1.CreateAnonymousSessionRequest)
	require.True(t, ok)
	require.Equal(t, "proj-1", req2.ProjectId)
	got2, err := c2.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got2.AccessToken)
}

func TestClientAccount_VerificationAndRecovery(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	ver, err := c.Account.CreateVerification(ctx, "https://x/{{code}}")
	require.NoError(t, err)
	require.Equal(t, "acc-1", ver.UserId)
	req, ok := fake.lastRequest().(*clientv1.CreateVerificationRequest)
	require.True(t, ok)
	require.Equal(t, "https://x/{{code}}", req.Url)

	acc, err := c.Account.UpdateVerification(ctx, "acc-1", "sec-1")
	require.NoError(t, err)
	require.Equal(t, "acc-1", acc.Id)
	req2, ok := fake.lastRequest().(*clientv1.UpdateVerificationRequest)
	require.True(t, ok)
	require.Equal(t, "sec-1", req2.Secret)

	require.NoError(t, c.Account.CreateRecovery(ctx, "u@example.com", "https://x/{{code}}"))
	req3, ok := fake.lastRequest().(*clientv1.CreateRecoveryRequest)
	require.True(t, ok)
	require.Equal(t, "u@example.com", req3.Email)

	require.NoError(t, c.Account.UpdateRecovery(ctx, "acc-1", "sec-2", "new-pw"))
	req4, ok := fake.lastRequest().(*clientv1.UpdateRecoveryRequest)
	require.True(t, ok)
	require.Equal(t, "sec-2", req4.Secret)
	require.Equal(t, "new-pw", req4.Password)
}

func TestClientAccount_MFAFactors(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	factors, err := c.Account.ListFactors(ctx)
	require.NoError(t, err)
	require.Len(t, factors.Factors, 1)
	require.Equal(t, "verified", factors.Factors[0].Status)

	totp, err := c.Account.CreateTOTPFactor(ctx)
	require.NoError(t, err)
	require.Equal(t, "pending", totp.Factor.Status)
	require.NotEmpty(t, totp.Secret)
	require.Contains(t, totp.OtpauthUrl, "otpauth://")

	verified, err := c.Account.VerifyTOTPFactor(ctx, "f1", "123456")
	require.NoError(t, err)
	require.Equal(t, "verified", verified.Status)
	req, ok := fake.lastRequest().(*clientv1.VerifyTOTPFactorRequest)
	require.True(t, ok)
	require.Equal(t, "f1", req.FactorId)
	require.Equal(t, "123456", req.Code)

	resp, err := c.Account.CreateMFASession(ctx, "ch-1", "f1", "123456")
	require.NoError(t, err)
	require.Equal(t, "acc-1", resp.Account.Id)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)
	req2, ok := fake.lastRequest().(*clientv1.CreateMFASessionRequest)
	require.True(t, ok)
	require.Equal(t, "ch-1", req2.ChallengeToken)
	require.Equal(t, "f1", req2.FactorId)
}

// TestClientAccount_DeleteFactor（G11-4）：DeleteFactor 透传 factor_id/code；
// verified 因子缺 code 的错误路径原样透传给调用方。
func TestClientAccount_DeleteFactor(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	require.NoError(t, c.Account.DeleteFactor(ctx, "f1", "123456"))
	req, ok := fake.lastRequest().(*clientv1.DeleteFactorRequest)
	require.True(t, ok)
	require.Equal(t, "f1", req.FactorId)
	require.Equal(t, "123456", req.Code)

	// pending 因子 code 可选（传空串）。
	require.NoError(t, c.Account.DeleteFactor(ctx, "f2", ""))
	req2, ok := fake.lastRequest().(*clientv1.DeleteFactorRequest)
	require.True(t, ok)
	require.Equal(t, "f2", req2.FactorId)
	require.Empty(t, req2.Code)

	// 错误路径：verified 因子缺 code → 服务端 InvalidArgument 透传。
	err := c.Account.DeleteFactor(ctx, "f1", "")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "code is required")
}

func TestClientAccount_JWTAndMagicURL(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))
	ctx := context.Background()

	jwt, err := c.Account.CreateJWT(ctx)
	require.NoError(t, err)
	require.Equal(t, "jwt-one-time", jwt.Token)

	ch, err := c.Account.CreateMagicURLSession(ctx, "u@example.com", "https://x/login")
	require.NoError(t, err)
	require.Equal(t, "ch-magic", ch.ChallengeId)
	req, ok := fake.lastRequest().(*clientv1.CreateMagicURLSessionRequest)
	require.True(t, ok)
	require.Equal(t, "u@example.com", req.Email)
	require.Equal(t, "https://x/login", req.Url)

	resp, err := c.Account.UpdateMagicURLSession(ctx, "acc-1", "sec-1")
	require.NoError(t, err)
	require.Equal(t, "acc-1", resp.Account.Id)
	got, err := c.store.Load()
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.AccessToken)
	req2, ok := fake.lastRequest().(*clientv1.UpdateMagicURLSessionRequest)
	require.True(t, ok)
	require.Equal(t, "sec-1", req2.Secret)
}

func TestClientAccount_ListLogs(t *testing.T) {
	c, fake := newTestClient(t, WithProjectID("proj-1"))

	logs, err := c.Account.ListLogs(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, logs.Logs, 1)
	require.Equal(t, "SignIn", logs.Logs[0].Action)
	req, ok := fake.lastRequest().(*clientv1.ListLogsRequest)
	require.True(t, ok)
	require.Equal(t, int32(10), req.Limit)
}
