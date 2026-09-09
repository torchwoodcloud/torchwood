package client

import (
	"context"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const oneTimeJWTTTL = 5 * time.Minute

// CreateJWT 用当前会话换取 5 分钟 TTL 的一次性 JWT（与 end-user token 同一
// 派生 key，validator 可直接验证）。claims 绑定 jti 与 SessionID（登出/撤销
// 会话后立即失效），并在 Redis 记录一次性消费标记（验证方原子 GETDEL 消费）。
func (a *Account) CreateJWT(ctx context.Context) (string, error) {
	p, err := a.requireUser(ctx)
	if err != nil {
		return "", err
	}
	user, err := a.requireAccountUser(ctx, p.ProjectID, p.UserID)
	if err != nil {
		return "", err
	}
	email := user.Email

	var roles []string
	if a.roles != nil {
		// 签发路径手头是 app 层投影（*client.User）而非 domain users 行，
		// 传 nil 由解析器兜底单查（查询次数与修复前一致；热路径省查在
		// validator 侧完成——P0.5 LoadUserRoles 重复查询修复的主战场）。
		roles, err = a.roles.LoadUserRoles(ctx, p.ProjectID, p.UserID, nil)
		if err != nil {
			return "", err
		}
	}
	now := time.Now()
	jti := idgen.UUID().String()
	claims := jwtparser.Claims{
		TokenID:   jti,
		UserID:    p.UserID,
		Username:  email,
		ActorKind: "end_user",
		ProjectID: p.ProjectID,
		SessionID: p.SessionID,
		TokenType: jwtparser.TokenTypeAccess,
		Roles:     roles,
		OneTime:   true,
		ExpiresAt: now.Add(oneTimeJWTTTL).Unix(),
		IssuedAt:  now.Unix(),
	}
	key := jwtparser.DeriveKey(a.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeEndUserJWT)
	token, err := jwtparser.Generate(key, claims)
	if err != nil {
		return "", status.Error(codes.Internal, "jwt generation failed")
	}
	// 一次性消费记录：jti 在 TTL 内只能被消费一次。存储值不能为空——
	// 验证方以「GETDEL 返回空 = 未签发/已消费」做 fail-closed 判定，SessionID
	// 为空时若原样写入空值会导致首次验证即被误拒。
	value := p.SessionID
	if value == "" {
		value = jti
	}
	if a.oneTimeTokens != nil {
		ok, err := a.oneTimeTokens.Register(ctx, domainauth.OneTimeJWTKeyPrefix+jti, value, oneTimeJWTTTL)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", status.Error(codes.Internal, "jwt id collision")
		}
	}
	return token, nil
}
