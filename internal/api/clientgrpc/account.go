package clientgrpc

import (
	"context"
	"fmt"
	"net/http"
	"time"

	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/app/client"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type AccountService struct {
	clientv1.UnimplementedAccountServiceServer
	account *client.Account
}

func NewAccountService(account *client.Account) *AccountService {
	return &AccountService{account: account}
}

func (s *AccountService) SignUp(ctx context.Context, req *clientv1.SignUpRequest) (*clientv1.SignUpResponse, error) {
	user, tokens, cookie, mfa, err := s.account.SignUp(ctx, client.SignUpCommand{
		ProjectID: req.GetProjectId(),
		Email:     req.GetEmail(),
		Password:  req.GetPassword(),
		Name:      req.GetName(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return &clientv1.SignUpResponse{
		Account:        mapUser(user),
		Tokens:         mapTokens(tokens),
		MfaRequired:    mfa != nil,
		ChallengeToken: mapChallengeToken(mfa),
		Factors:        mapFactors(mfa),
	}, nil
}

func (s *AccountService) SignIn(ctx context.Context, req *clientv1.SignInRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.SignIn(ctx, client.SignInCommand{
		ProjectID: req.GetProjectId(),
		Email:     req.GetEmail(),
		Password:  req.GetPassword(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) SignOut(ctx context.Context, _ *clientv1.SignOutRequest) (*sharedv1.Empty, error) {
	// R04-P2-4：use-case 已容忍无 principal（幂等登出），这里不再重复校验。
	if err := s.account.SignOut(ctx); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) Me(ctx context.Context, _ *clientv1.MeRequest) (*clientv1.Account, error) {
	user, err := s.account.Me(ctx)
	if err != nil {
		return nil, err
	}
	return mapUser(user), nil
}

func (s *AccountService) RefreshToken(ctx context.Context, req *clientv1.RefreshTokenRequest) (*clientv1.RefreshTokenResponse, error) {
	tokens, cookie, err := s.account.RefreshToken(ctx, client.RefreshTokenCommand{
		ProjectID:    req.GetProjectId(),
		RefreshToken: req.GetRefreshToken(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && req.GetProjectId() != "" {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return &clientv1.RefreshTokenResponse{Tokens: mapTokens(tokens)}, nil
}

func (s *AccountService) UpdateAccount(ctx context.Context, req *clientv1.UpdateAccountRequest) (*clientv1.Account, error) {
	// D-1 presence 语义（本仓生成物无 HasXxx，用非 nil 判断）：
	// name/email 未设置=不修改；设置（含空串）=更新/清空。
	cmd := client.UpdateAccountCommand{
		URL:         req.GetUrl(),
		Password:    req.GetPassword(),
		OldPassword: req.GetOldPassword(),
	}
	if req.Name != nil {
		cmd.Name = req.Name
	}
	if req.Email != nil {
		cmd.Email = req.Email
	}
	user, err := s.account.UpdateAccount(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return mapUser(user), nil
}

func (s *AccountService) ConfirmEmailChange(ctx context.Context, req *clientv1.ConfirmEmailChangeRequest) (*clientv1.Account, error) {
	user, err := s.account.ConfirmEmailChange(ctx, client.ConfirmEmailChangeCommand{
		ProjectID: req.GetProjectId(),
		UserID:    req.GetUserId(),
		Secret:    req.GetSecret(),
	})
	if err != nil {
		return nil, err
	}
	return mapUser(user), nil
}

func (s *AccountService) ListSessions(ctx context.Context, _ *clientv1.ListSessionsRequest) (*clientv1.ListSessionsResponse, error) {
	sessions, err := s.account.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.Session, len(sessions))
	for i := range sessions {
		out[i] = mapSession(&sessions[i])
	}
	return &clientv1.ListSessionsResponse{Sessions: out}, nil
}

func (s *AccountService) DeleteSession(ctx context.Context, req *clientv1.DeleteSessionRequest) (*sharedv1.Empty, error) {
	// R04-P3-2 不变量（空 session_id 直接 InvalidArgument，不落到 use-case）
	// 由 DeleteSessionRequest 的 buf.validate required 注解在 validate
	// 拦截器承担（docs/developer/09-api-guide.md §1.6）。
	if err := s.account.DeleteSession(ctx, req.GetSessionId()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) DeleteSessions(ctx context.Context, req *clientv1.DeleteSessionsRequest) (*sharedv1.Empty, error) {
	if err := s.account.DeleteSessions(ctx, req.GetKeepCurrent()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) GetPrefs(ctx context.Context, _ *clientv1.GetPrefsRequest) (*clientv1.GetPrefsResponse, error) {
	prefs, err := s.account.GetPrefs(ctx)
	if err != nil {
		return nil, err
	}
	data, err := structpb.NewStruct(prefs)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is not serializable")
	}
	return &clientv1.GetPrefsResponse{Prefs: data}, nil
}

func (s *AccountService) UpdatePrefs(ctx context.Context, req *clientv1.UpdatePrefsRequest) (*clientv1.GetPrefsResponse, error) {
	// prefs required 由 UpdatePrefsRequest 的 buf.validate 注解在 validate
	// 拦截器承担；app 层保留自身防线（内部调用方不经过 gRPC 面）。
	prefs, err := s.account.UpdatePrefs(ctx, req.GetPrefs().AsMap())
	if err != nil {
		return nil, err
	}
	data, err := structpb.NewStruct(prefs)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "prefs is not serializable")
	}
	return &clientv1.GetPrefsResponse{Prefs: data}, nil
}

func (s *AccountService) CreateEmailOTP(ctx context.Context, req *clientv1.CreateEmailOTPRequest) (*clientv1.ChallengeResponse, error) {
	challenge, err := s.account.CreateEmailOTP(ctx, client.CreateEmailOTPCommand{
		ProjectID: req.GetProjectId(),
		Email:     req.GetEmail(),
	})
	if err != nil {
		return nil, err
	}
	return &clientv1.ChallengeResponse{
		ChallengeId: challenge.ChallengeID,
		ExpireAt:    timestamppb.New(challenge.ExpireAt),
	}, nil
}

func (s *AccountService) CreateEmailOTPSession(ctx context.Context, req *clientv1.CreateEmailOTPSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.CreateEmailOTPSession(ctx, client.CreateEmailOTPSessionCommand{
		ProjectID:   req.GetProjectId(),
		Email:       req.GetEmail(),
		ChallengeID: req.GetChallengeId(),
		OTP:         req.GetOtp(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) CreateOAuth2Session(ctx context.Context, req *clientv1.CreateOAuth2SessionRequest) (*clientv1.CreateOAuth2SessionResponse, error) {
	redirectURL, nonce, err := s.account.CreateOAuth2Session(ctx, client.CreateOAuth2SessionCommand{
		ProjectID: req.GetProjectId(),
		Provider:  req.GetProvider(),
		Success:   req.GetSuccess(),
		Failure:   req.GetFailure(),
	})
	if err != nil {
		return nil, err
	}
	// M5 C5：nonce 种入浏览器（Set-Cookie 经 gateway metadata 透传），
	// 回调端点与 state 配对校验（login CSRF）。
	setOAuth2NonceCookie(ctx, s.account, req.GetProjectId(), nonce)
	return &clientv1.CreateOAuth2SessionResponse{RedirectUrl: redirectURL}, nil
}

func (s *AccountService) CreateOAuth2TokenSession(ctx context.Context, req *clientv1.CreateOAuth2TokenSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.CreateOAuth2TokenSession(ctx, client.CreateOAuth2TokenSessionCommand{
		ProjectID: req.GetProjectId(),
		Provider:  req.GetProvider(),
		Success:   req.GetSuccess(),
		Failure:   req.GetFailure(),
		Code:      req.GetCode(),
		State:     req.GetState(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) CreatePhoneOTP(ctx context.Context, req *clientv1.CreatePhoneOTPRequest) (*clientv1.ChallengeResponse, error) {
	challenge, err := s.account.CreatePhoneOTP(ctx, client.CreatePhoneOTPCommand{
		ProjectID: req.GetProjectId(),
		Phone:     req.GetPhone(),
	})
	if err != nil {
		return nil, err
	}
	return &clientv1.ChallengeResponse{
		ChallengeId: challenge.ChallengeID,
		ExpireAt:    timestamppb.New(challenge.ExpireAt),
	}, nil
}

func (s *AccountService) CreatePhoneOTPSession(ctx context.Context, req *clientv1.CreatePhoneOTPSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.CreatePhoneOTPSession(ctx, client.CreatePhoneOTPSessionCommand{
		ProjectID:   req.GetProjectId(),
		Phone:       req.GetPhone(),
		ChallengeID: req.GetChallengeId(),
		OTP:         req.GetOtp(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) CreateWeChatMiniProgramSession(ctx context.Context, req *clientv1.CreateWeChatMiniProgramSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.CreateWeChatMiniProgramSession(ctx, client.CreateWeChatMiniProgramSessionCommand{
		ProjectID: req.GetProjectId(),
		Code:      req.GetCode(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) CreateAnonymousSession(ctx context.Context, req *clientv1.CreateAnonymousSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.CreateAnonymousSession(ctx, client.CreateAnonymousSessionCommand{
		ProjectID: req.GetProjectId(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) CreateOAuth2LinkSession(ctx context.Context, req *clientv1.CreateOAuth2LinkSessionRequest) (*clientv1.CreateOAuth2SessionResponse, error) {
	redirectURL, nonce, err := s.account.CreateOAuth2LinkSession(ctx, client.CreateOAuth2LinkSessionCommand{
		ProjectID: req.GetProjectId(),
		Provider:  req.GetProvider(),
		Success:   req.GetSuccess(),
		Failure:   req.GetFailure(),
	})
	if err != nil {
		return nil, err
	}
	// link 发起同样绑定 nonce cookie（M5 C5）；project 回退到调用者本人的
	// 项目（与 use-case 内 projectID 缺省逻辑一致）。
	projectID := req.GetProjectId()
	if projectID == "" {
		if p, ok := contexts.Principal(ctx); ok {
			projectID = p.ProjectID
		}
	}
	setOAuth2NonceCookie(ctx, s.account, projectID, nonce)
	return &clientv1.CreateOAuth2SessionResponse{RedirectUrl: redirectURL}, nil
}

func (s *AccountService) CreateOAuth2LinkTokenSession(ctx context.Context, req *clientv1.CreateOAuth2LinkTokenSessionRequest) (*clientv1.Account, error) {
	user, err := s.account.CreateOAuth2LinkTokenSession(ctx, client.CreateOAuth2LinkTokenSessionCommand{
		ProjectID: req.GetProjectId(),
		Provider:  req.GetProvider(),
		Code:      req.GetCode(),
		State:     req.GetState(),
	})
	if err != nil {
		return nil, err
	}
	return mapUser(user), nil
}

func (s *AccountService) CreateVerification(ctx context.Context, req *clientv1.CreateVerificationRequest) (*clientv1.CreateVerificationResponse, error) {
	challenge, err := s.account.CreateVerification(ctx, client.CreateVerificationCommand{
		ProjectID: req.GetProjectId(),
		URL:       req.GetUrl(),
	})
	if err != nil {
		return nil, err
	}
	return &clientv1.CreateVerificationResponse{
		UserId:   challenge.UserID,
		ExpireAt: timestamppb.New(time.Unix(challenge.ExpireAt, 0)),
	}, nil
}

func (s *AccountService) UpdateVerification(ctx context.Context, req *clientv1.UpdateVerificationRequest) (*clientv1.Account, error) {
	user, err := s.account.UpdateVerification(ctx, client.UpdateVerificationCommand{
		ProjectID: req.GetProjectId(),
		UserID:    req.GetUserId(),
		Secret:    req.GetSecret(),
	})
	if err != nil {
		return nil, err
	}
	return mapUser(user), nil
}

func (s *AccountService) CreateRecovery(ctx context.Context, req *clientv1.CreateRecoveryRequest) (*sharedv1.Empty, error) {
	if err := s.account.CreateRecovery(ctx, client.CreateRecoveryCommand{
		ProjectID: req.GetProjectId(),
		Email:     req.GetEmail(),
		URL:       req.GetUrl(),
	}); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) UpdateRecovery(ctx context.Context, req *clientv1.UpdateRecoveryRequest) (*sharedv1.Empty, error) {
	if err := s.account.UpdateRecovery(ctx, client.UpdateRecoveryCommand{
		ProjectID: req.GetProjectId(),
		UserID:    req.GetUserId(),
		Secret:    req.GetSecret(),
		Password:  req.GetPassword(),
	}); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) ListFactors(ctx context.Context, _ *clientv1.ListFactorsRequest) (*clientv1.ListFactorsResponse, error) {
	ctx = contexts.WithAuditResource(ctx, "")
	factors, err := s.account.ListFactors(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.Factor, 0, len(factors))
	for i := range factors {
		out = append(out, mapFactor(&factors[i]))
	}
	return &clientv1.ListFactorsResponse{Factors: out}, nil
}

func (s *AccountService) CreateTOTPFactor(ctx context.Context, _ *clientv1.CreateTOTPFactorRequest) (*clientv1.TOTPFactor, error) {
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, p.UserID)
	factor, plainSecret, otpauthURL, err := s.account.CreateTOTPFactor(ctx, p.ProjectID, p.UserID, p.Email)
	if err != nil {
		return nil, err
	}
	return &clientv1.TOTPFactor{
		Factor:     mapFactor(factor),
		Secret:     plainSecret,
		OtpauthUrl: otpauthURL,
	}, nil
}

func (s *AccountService) VerifyTOTPFactor(ctx context.Context, req *clientv1.VerifyTOTPFactorRequest) (*clientv1.Factor, error) {
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, p.UserID)
	factor, err := s.account.VerifyTOTPFactor(ctx, p.ProjectID, p.UserID, req.GetFactorId(), req.GetCode())
	if err != nil {
		return nil, err
	}
	return mapFactor(factor), nil
}

func (s *AccountService) DeleteFactor(ctx context.Context, req *clientv1.DeleteFactorRequest) (*sharedv1.Empty, error) {
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, p.UserID)
	// code 为删除 verified 因子时的 TOTP 二次验证码（REST 经 query 传 ?code=...）；
	// 未携带时 use-case 对 verified 因子以 InvalidArgument 拒绝（fail-closed，R05-P1-4）。
	if err := s.account.DeleteFactor(ctx, p.ProjectID, p.UserID, req.GetFactorId(), req.GetCode()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *AccountService) CreateMFASession(ctx context.Context, req *clientv1.CreateMFASessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, err := s.account.CompleteMFASession(ctx, req.GetProjectId(), req.GetChallengeToken(), req.GetFactorId(), req.GetCode())
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, nil), nil
}

func (s *AccountService) CreateJWT(ctx context.Context, _ *clientv1.CreateJWTRequest) (*clientv1.CreateJWTResponse, error) {
	token, err := s.account.CreateJWT(ctx)
	if err != nil {
		return nil, err
	}
	return &clientv1.CreateJWTResponse{Token: token}, nil
}

func (s *AccountService) CreateMagicURLSession(ctx context.Context, req *clientv1.CreateMagicURLSessionRequest) (*clientv1.ChallengeResponse, error) {
	challenge, err := s.account.CreateMagicURLSession(ctx, client.CreateMagicURLSessionCommand{
		ProjectID: req.GetProjectId(),
		Email:     req.GetEmail(),
		URL:       req.GetUrl(),
	})
	if err != nil {
		return nil, err
	}
	return &clientv1.ChallengeResponse{
		ChallengeId: challenge.ChallengeID,
		ExpireAt:    timestamppb.New(challenge.ExpireAt),
	}, nil
}

func (s *AccountService) UpdateMagicURLSession(ctx context.Context, req *clientv1.UpdateMagicURLSessionRequest) (*clientv1.SignInResponse, error) {
	user, tokens, cookie, mfa, err := s.account.UpdateMagicURLSession(ctx, client.UpdateMagicURLSessionCommand{
		ProjectID: req.GetProjectId(),
		UserID:    req.GetUserId(),
		Secret:    req.GetSecret(),
	})
	if err != nil {
		return nil, err
	}
	if tokens != nil && cookie != "" && mfa == nil {
		setEndUserSessionCookie(ctx, s.account, req.GetProjectId(), cookie)
	}
	return mapSignInResult(user, tokens, mfa), nil
}

func (s *AccountService) ListLogs(ctx context.Context, req *clientv1.ListLogsRequest) (*clientv1.ListLogsResponse, error) {
	// P3-9：ListLogs 补分页（page_size/page_token/meta），兼容旧 limit。
	pageSize := req.GetPageSize()
	if pageSize == 0 {
		pageSize = req.GetLimit()
	}
	params, err := s.parseLogsListParams(pageSize, req.GetPageToken())
	if err != nil {
		return nil, err
	}
	// 过度拉取以支持 offset（audit 仅支持 limit，不支持 offset，内存分页足够，limit≤100）。
	fetchLimit := int32(params.Offset + int(params.PageSize))
	if fetchLimit > 100 {
		fetchLimit = 100
	}
	if fetchLimit <= 0 {
		fetchLimit = params.PageSize
	}
	entries, err := s.account.ListLogs(ctx, fetchLimit)
	if err != nil {
		return nil, err
	}
	// 内存分页：offset + pageSize 切片
	start := params.Offset
	if start > len(entries) {
		start = len(entries)
	}
	end := start + int(params.PageSize)
	if end > len(entries) {
		end = len(entries)
	}
	page := entries[start:end]
	hasMore := end < len(entries)
	info := crud.BuildPaginationInfo(params, 0, hasMore)
	var nextToken, prevToken string
	if info.HasNext {
		if nextToken, err = crud.EncodePageToken(info.NextOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if info.HasPrevious {
		if prevToken, err = crud.EncodePageToken(info.PreviousOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	out := make([]*clientv1.LogEntry, 0, len(page))
	for i := range page {
		out = append(out, mapLogEntry(&page[i]))
	}
	meta := &sharedv1.ListResponseMeta{
		PageSize:      info.PageSize,
		NextPageToken: nextToken,
		PrevPageToken: prevToken,
		TotalCount:    0,
	}
	return &clientv1.ListLogsResponse{Logs: out, Meta: meta}, nil
}

func (s *AccountService) parseLogsListParams(pageSize int32, pageToken string) (crud.ListParams, error) {
	// 复用 crud 分页能力，filter/order_by 为空（logs 不支持）。
	p, err := crud.ParseListParams(pageSize, pageToken, "", "")
	if err != nil {
		return crud.ListParams{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return p, nil
}

func (s *AccountService) requirePrincipal(ctx context.Context) (*shared.Principal, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || p.ActorKind != shared.ActorKindEndUser || p.UserID == "" {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return p, nil
}

func mapSignInResult(user *client.User, tokens *client.TokenBundle, mfa *client.MFASignInChallenge) *clientv1.SignInResponse {
	resp := &clientv1.SignInResponse{
		Account: mapUser(user),
		Tokens:  mapTokens(tokens),
	}
	if mfa != nil {
		resp.MfaRequired = true
		resp.ChallengeToken = mfa.Token
		resp.Factors = mapFactors(mfa)
	}
	return resp
}

func mapChallengeToken(mfa *client.MFASignInChallenge) string {
	if mfa == nil {
		return ""
	}
	return mfa.Token
}

func mapFactors(mfa *client.MFASignInChallenge) []*clientv1.Factor {
	if mfa == nil {
		return nil
	}
	out := make([]*clientv1.Factor, 0, len(mfa.Factors))
	for i := range mfa.Factors {
		out = append(out, mapFactor(&mfa.Factors[i]))
	}
	return out
}

func mapFactor(f *domainauth.Factor) *clientv1.Factor {
	if f == nil {
		return nil
	}
	out := &clientv1.Factor{
		Id:     f.ID,
		Type:   f.Type,
		Status: f.Status,
	}
	if !f.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(f.CreatedAt)
	}
	return out
}

func mapLogEntry(e *audit.Entry) *clientv1.LogEntry {
	if e == nil {
		return nil
	}
	out := &clientv1.LogEntry{
		Id:         e.ID,
		Action:     e.Action,
		Status:     e.Status,
		ResourceId: e.ResourceID,
		Ip:         e.IP,
		UserAgent:  e.UserAgent,
	}
	if !e.CreatedAt.IsZero() {
		out.CreatedAt = timestamppb.New(e.CreatedAt)
	}
	return out
}

func mapUser(u *client.User) *clientv1.Account {
	if u == nil {
		return nil
	}
	return &clientv1.Account{
		Id:            u.ID,
		Email:         u.Email,
		Name:          u.Name,
		Status:        u.Status,
		EmailVerified: u.EmailVerified,
		CreatedAt:     timestamppb.New(u.CreatedAt),
		UpdatedAt:     timestamppb.New(u.UpdatedAt),
	}
}

func mapSession(s *client.Session) *clientv1.Session {
	if s == nil {
		return nil
	}
	out := &clientv1.Session{
		Id:        s.ID,
		UserId:    s.UserID,
		Provider:  s.Provider,
		UserAgent: s.UserAgent,
		Ip:        s.IP,
		CreatedAt: timestamppb.New(s.CreatedAt),
		Current:   s.Current,
	}
	if !s.ExpireAt.IsZero() {
		out.ExpireAt = timestamppb.New(s.ExpireAt)
	}
	return out
}

func mapTokens(t *client.TokenBundle) *clientv1.TokenBundle {
	if t == nil {
		return nil
	}
	return &clientv1.TokenBundle{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    timestamppb.New(time.Unix(t.ExpiresAt, 0)),
	}
}

// setEndUserSessionCookie 以与 Console 相同机制把 HMAC session cookie 写入 Set-Cookie gateway metadata。
// Cookie 名保持 TORCHWOOD_session_<project>，复用与 SessionService 相同的 HMAC 签名。
func setEndUserSessionCookie(ctx context.Context, account *client.Account, projectID, cookie string) {
	if projectID == "" || cookie == "" || account == nil {
		return
	}
	secure := account.SecureCookies()
	name := fmt.Sprintf("TORCHWOOD_session_%s", projectID)
	c := &http.Cookie{
		Name:     name,
		Value:    cookie,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs("set-cookie", c.String()))
}

// setOAuth2NonceCookie 种 OAuth 发起一次性 nonce cookie（M5 C5 login CSRF）：
// 回调端点要求与 state 记录配对；TTL 与 oauth state 的 10min 对齐。
func setOAuth2NonceCookie(ctx context.Context, account *client.Account, projectID, nonce string) {
	if projectID == "" || nonce == "" || account == nil {
		return
	}
	c := &http.Cookie{
		Name:     fmt.Sprintf("TORCHWOOD_oauth_nonce_%s", projectID),
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		Secure:   account.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	}
	_ = grpc.SetHeader(ctx, metadata.Pairs("set-cookie", c.String()))
}
