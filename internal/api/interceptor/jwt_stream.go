package interceptor

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// StreamAuthMiddleware 是流式面的认证拦截器（B17）：与 UnaryAuthMiddleware
// 同语义——PUBLIC 面可选认证（无效 X-API-Key 401 + 频控）、非 PUBLIC 强制
// 认证、admin 业务校验（X-Torchwood-Project 单值 + ValidateAdminProject
// Access）、双 principal 注入（torchwood contexts + 库 principal）。授权
// 执行在链上紧随其后的库 authz 拦截器（open-time / per-message 由方法的
// StreamAuth 声明决定）。
func (i *AuthInterceptor) StreamAuthMiddleware(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if i.policies.IsAnonymous(info.FullMethod) {
		return i.servePublicStream(srv, ss, info, handler)
	}
	ctx := ss.Context()
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		i.logAuthFailure(ctx, info.FullMethod, "metadata_missing", "", nil)
		return status.Error(codes.Unauthenticated, "metadata is not provided")
	}
	authn := authnRequestFromMD(md)
	principal, err := i.validator.Authenticate(ctx, authn)
	if err != nil {
		ct, _, parseErr := shared.ParseAuthnRequest(authn)
		if parseErr != nil {
			i.logAuthFailure(ctx, info.FullMethod, parseFailureReason(parseErr), "", nil)
			return status.Error(codes.Unauthenticated, parseErr.Error())
		}
		// T-02：API key 认证失败按来源 IP 计数，超限 429（与 unary 同语义）。
		if ct == shared.CredentialTypeAPIKey {
			if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" {
				if throttleErr := i.recordAPIKeyAuthFailure(ctx, ci.IP); throttleErr != nil {
					i.logAuthFailure(ctx, info.FullMethod, "apikey_auth_throttled", ct, nil)
					return throttleErr
				}
			}
		}
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", ct, nil)
		return err
	}
	if principal == nil {
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", "", nil)
		return status.Error(codes.Unauthenticated, "invalid or expired credential")
	}
	ctx, err = i.finalizePrincipal(ctx, md, info.FullMethod, principal)
	if err != nil {
		return err
	}
	return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
}

// servePublicStream 处理 PUBLIC 流式方法：凭证尽力注入、无效 X-API-Key
// 401 + 频控（T-02 复扫，与 unary 同语义），授权面 anonymous 树恒放行。
func (i *AuthInterceptor) servePublicStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := ss.Context()
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return handler(srv, ss)
	}
	authn := authnRequestFromMD(md)
	principal, authnErr := i.validator.Authenticate(ctx, authn)
	if authnErr != nil {
		if ct, _, parseErr := shared.ParseAuthnRequest(authn); parseErr == nil && ct == shared.CredentialTypeAPIKey {
			if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" {
				if throttleErr := i.recordAPIKeyAuthFailure(ctx, ci.IP); throttleErr != nil {
					i.logAuthFailure(ctx, info.FullMethod, "apikey_auth_throttled", ct, nil)
					return throttleErr
				}
			}
			i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", ct, nil)
			return status.Error(codes.Unauthenticated, "invalid api key")
		}
	}
	if authnErr == nil && principal != nil {
		ctx = injectPrincipals(ctx, principal)
	}
	return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
}

// ctxStream 把注入后的 ctx 透传给 handler（grpc.ServerStream.Context
// 不可变，须包装）。
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
