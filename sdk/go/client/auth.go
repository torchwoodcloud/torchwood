package client

import (
	"context"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// noRefreshMethods 不经过刷新/401 重试逻辑的方法（公开方法 + SignOut）。
// 必须名副其实：与 proto/client/v1/account.proto 的 ACCESS_PUBLIC 方法集合
// 保持一致（Round3 H4-2）——公开方法凭 user_id+secret/邮箱即可调用，本地
// 有过期 refresh token 时也不得被刷新逻辑拦截。client databases 的
// ACCESS_PUBLIC 方法（ListDocuments/GetDocument/CountDocuments）同样纳入。
var noRefreshMethods = map[string]bool{
	clientv1.AccountService_SignIn_FullMethodName:                         true,
	clientv1.AccountService_SignUp_FullMethodName:                         true,
	clientv1.AccountService_RefreshToken_FullMethodName:                   true,
	clientv1.AccountService_SignOut_FullMethodName:                        true,
	clientv1.AccountService_ConfirmEmailChange_FullMethodName:             true,
	clientv1.AccountService_CreateEmailOTP_FullMethodName:                 true,
	clientv1.AccountService_CreateEmailOTPSession_FullMethodName:          true,
	clientv1.AccountService_CreateOAuth2Session_FullMethodName:            true,
	clientv1.AccountService_CreateOAuth2TokenSession_FullMethodName:       true,
	clientv1.AccountService_CreatePhoneOTP_FullMethodName:                 true,
	clientv1.AccountService_CreatePhoneOTPSession_FullMethodName:          true,
	clientv1.AccountService_CreateWeChatMiniProgramSession_FullMethodName: true,
	clientv1.AccountService_CreateAnonymousSession_FullMethodName:         true,
	clientv1.AccountService_UpdateVerification_FullMethodName:             true,
	clientv1.AccountService_CreateRecovery_FullMethodName:                 true,
	clientv1.AccountService_UpdateRecovery_FullMethodName:                 true,
	clientv1.AccountService_CreateMFASession_FullMethodName:               true,
	clientv1.AccountService_CreateMagicURLSession_FullMethodName:          true,
	clientv1.AccountService_UpdateMagicURLSession_FullMethodName:          true,
	clientv1.DatabasesService_ListDocuments_FullMethodName:                true,
	clientv1.DatabasesService_GetDocument_FullMethodName:                  true,
	clientv1.DatabasesService_CountDocuments_FullMethodName:               true,
}

// authInterceptor 挂 Bearer token，处理主动刷新与 401 刷新重试。
func (c *Client) authInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if noRefreshMethods[method] {
			tok, _ := c.store.Load()
			return invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
		}
		if err := c.refreshIfExpiring(ctx); err != nil {
			return err
		}
		tok, _ := c.store.Load()
		err := invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
		if status.Code(err) != codes.Unauthenticated || tok == nil {
			return err
		}
		// 401：仅当 token 未被其他 goroutine 刷新过时刷新一次并重试
		if !c.refreshAfterUnauthorized(ctx, tok.AccessToken) {
			return err
		}
		tok, _ = c.store.Load()
		return invoker(withBearer(ctx, tok), method, req, reply, cc, opts...)
	}
}

func withBearer(ctx context.Context, tok *clientv1.TokenBundle) context.Context {
	if tok == nil || tok.AccessToken == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok.AccessToken)
}

func tokenExpiring(t *clientv1.TokenBundle, now time.Time) bool {
	return t != nil && t.ExpiresAt != nil && t.ExpiresAt.AsTime().Before(now.Add(refreshSkew))
}

// refreshIfExpiring 主动刷新：token 距过期不足 refreshSkew 时用 refresh token 换新。
func (c *Client) refreshIfExpiring(ctx context.Context) error {
	tok, _ := c.store.Load()
	if !tokenExpiring(tok, c.now()) || tok.RefreshToken == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// double-check：等待锁期间可能已被其他 goroutine 刷新
	tok, _ = c.store.Load()
	if !tokenExpiring(tok, c.now()) {
		return nil
	}
	return c.doRefreshLocked(ctx, tok.RefreshToken)
}

// refreshAfterUnauthorized 401 后刷新：仅当 store 中仍是用过的那个 token 才刷新
// （否则说明已有其他 goroutine 刷新过，直接重试即可）。
func (c *Client) refreshAfterUnauthorized(ctx context.Context, usedToken string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tok, _ := c.store.Load()
	if tok == nil || tok.RefreshToken == "" {
		return false
	}
	if tok.AccessToken == usedToken {
		if err := c.doRefreshLocked(ctx, tok.RefreshToken); err != nil {
			return false
		}
	}
	return true
}

// doRefreshLocked 调用 RefreshToken；仅在 Unauthenticated（refresh token 失效）
// 时清空本地 token，临时错误保留。调用方须持 c.mu。
func (c *Client) doRefreshLocked(ctx context.Context, refreshToken string) error {
	resp, err := c.account.RefreshToken(ctx, &clientv1.RefreshTokenRequest{
		ProjectId:    c.cfg.ProjectID,
		RefreshToken: refreshToken,
	})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			c.clearTokens()
		}
		return err
	}
	return c.saveTokens(resp.Tokens)
}
