package functions

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"regexp"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// functionIDPattern 限制 Function ID 字符集与长度（防路径穿越与逻辑名歧义；
// 须以小写字母/数字开头，仅含小写字母/数字/下划线/连字符，最长 64）。
var functionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const (
	// minTimeoutSeconds / maxTimeoutSeconds 是函数超时允许范围（§5.2）。
	minTimeoutSeconds = 1
	maxTimeoutSeconds = 300
	// maxSyncTimeoutSeconds 限制同步调用超时（grpc-gateway WriteTimeout 余量）。
	maxSyncTimeoutSeconds = 30
	// defaultTimeoutSeconds 是创建函数未显式指定 timeout_seconds 时的服务端默认值
	//（与 Console 前端默认值 15 及 DB 列 DEFAULT 15 保持一致）。
	defaultTimeoutSeconds = 15

	// targetSecretMinLen 是自定义 target 签名密钥的最小长度（protovalidate
	// 同源；服务端生成值 43+ 字符恒满足）。
	targetSecretMinLen = 16
	// maxTargetURLBytes 是 target url 的字节上限（protovalidate 同源）。
	maxTargetURLBytes = 2048
)

// CreateFunctionCommand 创建函数命令（v4：target 字段进、池字段退）。
type CreateFunctionCommand struct {
	ID        string
	ProjectID string
	Name      string
	// TimeoutSeconds 可选（nil = 服务端默认 15）。
	TimeoutSeconds *int
	Enabled        *bool
	DeclaredScopes []string
	// ——endpoint target（v4 §3）——TargetURL 必填（https-or-dev 校验）；
	// TargetSecret 可选（空 = 服务端生成；明文仅在本请求/响应出现一次）。
	TargetURL    string
	TargetSecret string
	// ——客户端调用面策略（P2，设计 §4；nil = 未设置）——
	ClientCallable         *bool
	ClientAnonymousAllowed *bool
	ClientPerUserLimit     *int
	ClientLimitWindow      *string
}

// UpdateFunctionCommand 更新函数命令（nil = 不修改）。
type UpdateFunctionCommand struct {
	ProjectID      string
	FunctionID     string
	Name           *string
	TimeoutSeconds *int
	Enabled        *bool
	// TargetURL 可变（v4 §3：执行位置是一等配置）；密钥变更走
	// RotateFunctionSecret（与 webhooks Update/Rotate 分工一致）。
	TargetURL *string
	// ——客户端调用面策略（P2，设计 §4；nil = 未设置，不修改）——
	ClientCallable         *bool
	ClientAnonymousAllowed *bool
	ClientPerUserLimit     *int
	ClientLimitWindow      *string
}

// CreateFunctionWithSecret 创建函数：endpoint target 录入（url 校验）+ 签名
// 密钥（服务端生成或透传录入）加密落库、指纹投影、明文一次性回显（v4 §3
// 实施勘误：签名方每次调用需明文计算 HMAC——密钥本体 secretbox 加密落库，
// 模式照抄 webhooks CreateWebhook）。返回函数视图与**一次性明文密钥**。
func (f *Functions) CreateFunctionWithSecret(ctx context.Context, cmd CreateFunctionCommand) (*domainfunctions.Function, string, error) {
	// 纵深防御（G2-1/R06-P0，G12 产品决策 B 调整）：函数写操作允许 console
	// admin 会话与 API key（拦截器分别以 adminRoleMethodRules 管角色、
	// apiKeyScopeRules 管 scope），此处兜底拒绝端用户/匿名的直接调用。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, "", err
	}
	if !idgen.ID(cmd.ID).IsValid() {
		return nil, "", status.Error(codes.InvalidArgument, "id is required")
	}
	if !functionIDPattern.MatchString(cmd.ID) {
		return nil, "", status.Error(codes.InvalidArgument, "invalid function id: must match ^[a-z0-9][a-z0-9_-]{0,63}$")
	}
	if strings.TrimSpace(cmd.Name) == "" {
		return nil, "", status.Error(codes.InvalidArgument, "name is required")
	}
	if err := validateTargetURL(cmd.TargetURL, config.CurrentRuntimeEnv()); err != nil {
		return nil, "", err
	}
	timeoutSeconds := defaultTimeoutSeconds
	if cmd.TimeoutSeconds != nil {
		timeoutSeconds = *cmd.TimeoutSeconds
	}
	if timeoutSeconds < minTimeoutSeconds || timeoutSeconds > maxTimeoutSeconds {
		return nil, "", status.Errorf(codes.InvalidArgument, "timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds)
	}
	enabled := true
	if cmd.Enabled != nil {
		enabled = *cmd.Enabled
	}
	// 执行身份声明（P0）：词表/形态校验 + 去重排序；空集合法（= 无平台
	// 访问权限，fail-closed 默认）。
	declaredScopes, err := domainfunctions.NormalizeDeclaredScopes(cmd.DeclaredScopes)
	if err != nil {
		return nil, "", status.Error(codes.InvalidArgument, err.Error())
	}
	// target 签名密钥（v4 §3）：透传录入（≥16 字节，调用方自管强度）或
	// 服务端生成（twfnsec_ + 32 字节 base64url）。明文仅本响应出现一次。
	secret := cmd.TargetSecret
	if secret == "" {
		secret, err = generateTargetSecret()
		if err != nil {
			return nil, "", status.Errorf(codes.Internal, "generate target secret: %v", err)
		}
	} else if len(secret) < targetSecretMinLen {
		return nil, "", status.Errorf(codes.InvalidArgument, "target_secret must be at least %d bytes", targetSecretMinLen)
	}
	enc, err := secretbox.Encrypt(secret, f.encKey)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "encrypt target secret: %v", err)
	}
	now := f.nowFn()
	fn := &domainfunctions.Function{
		ID:                      cmd.ID,
		ProjectID:               cmd.ProjectID,
		Name:                    cmd.Name,
		TimeoutSeconds:          timeoutSeconds,
		Enabled:                 enabled,
		DeclaredScopes:          declaredScopes,
		TargetURL:               cmd.TargetURL,
		TargetSecretEnc:         enc,
		TargetSecretFingerprint: domainfunctions.Fingerprint(secret),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	// 客户端调用面策略（P2）：anonymous 一期禁用、callable 要求 limit>=1、
	// 窗口词表校验（设计 §4；存量全 FALSE ⇒ fail-closed）。
	if err := applyClientPolicy(fn, cmd.ClientCallable, cmd.ClientAnonymousAllowed, cmd.ClientPerUserLimit, cmd.ClientLimitWindow); err != nil {
		return nil, "", err
	}
	if err := f.repo.CreateFunction(ctx, fn); err != nil {
		return nil, "", err
	}
	f.cache.invalidate(cmd.ProjectID, cmd.ID)
	return fn, secret, nil
}

func (f *Functions) ListFunctions(ctx context.Context, projectID string) ([]domainfunctions.Function, error) {
	return f.repo.ListFunctions(ctx, projectID)
}

func (f *Functions) GetFunction(ctx context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	return fn, nil
}

func (f *Functions) UpdateFunction(ctx context.Context, cmd UpdateFunctionCommand) (*domainfunctions.Function, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, cmd.ProjectID, cmd.FunctionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	before := *fn
	if cmd.Name != nil {
		if strings.TrimSpace(*cmd.Name) == "" {
			return nil, status.Error(codes.InvalidArgument, "name is required")
		}
		fn.Name = *cmd.Name
	}
	if cmd.TimeoutSeconds != nil {
		if *cmd.TimeoutSeconds < minTimeoutSeconds || *cmd.TimeoutSeconds > maxTimeoutSeconds {
			return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds must be between %d and %d", minTimeoutSeconds, maxTimeoutSeconds)
		}
		fn.TimeoutSeconds = *cmd.TimeoutSeconds
	}
	if cmd.TargetURL != nil {
		if err := validateTargetURL(*cmd.TargetURL, config.CurrentRuntimeEnv()); err != nil {
			return nil, err
		}
		fn.TargetURL = *cmd.TargetURL
	}
	if cmd.Enabled != nil {
		fn.Enabled = *cmd.Enabled
	}
	// 客户端调用面策略（P2）：未设置字段不修改；anonymous 一期禁用。
	if err := applyClientPolicy(fn, cmd.ClientCallable, cmd.ClientAnonymousAllowed, cmd.ClientPerUserLimit, cmd.ClientLimitWindow); err != nil {
		return nil, err
	}
	fn.UpdatedAt = f.nowFn()
	// before/after diff 落审计 metadata.changes（成功/失败均记变更尝试）。
	recordAuditChanges(ctx, &before, fn)
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, err
	}
	// 缓存失效（P0.5）：同进程即时、跨实例 30s 收敛（cache.go 注释）。
	f.cache.invalidate(cmd.ProjectID, cmd.FunctionID)
	return fn, nil
}

// applyClientPolicy 校验并把客户端调用面策略应用到函数记录（P2，设计 §4）：
//   - client_anonymous_allowed=true 一期显式报错（Q4 拍板：匿名会话可无限
//     新造，per-user 限频对匿名形同虚设；字段保留，匿名 IP 兜底做好后再开）；
//   - client_callable=true 要求有效配额 client_per_user_limit >= 1
//     （无配额限频的公开调用面等于不限）；
//   - client_limit_window 词表 minute|hour|day（DB CHECK 与 proto 校验之外的
//     app 层兜底）；未设置时缺省 day。
//
// nil 字段 = 未设置（不修改现有值；Create 路径现有值即零值 FALSE）。
func applyClientPolicy(fn *domainfunctions.Function, callable, anonymous *bool, limit *int, window *string) error {
	if anonymous != nil && *anonymous {
		return status.Error(codes.InvalidArgument, "client_anonymous_allowed is not open in this release")
	}
	if limit != nil {
		if *limit < 0 {
			return status.Error(codes.InvalidArgument, "client_per_user_limit must be >= 0")
		}
		fn.ClientPerUserLimit = *limit
	}
	if window != nil {
		if !domainfunctions.IsValidClientLimitWindow(*window) {
			return status.Errorf(codes.InvalidArgument, "client_limit_window must be one of %q, %q, %q",
				domainfunctions.ClientLimitWindowMinute, domainfunctions.ClientLimitWindowHour, domainfunctions.ClientLimitWindowDay)
		}
		fn.ClientLimitWindow = *window
	}
	if callable != nil {
		fn.ClientCallable = *callable
	}
	if fn.ClientCallable {
		if fn.ClientPerUserLimit < 1 {
			return status.Error(codes.InvalidArgument, "client_callable requires client_per_user_limit >= 1")
		}
		if fn.ClientLimitWindow == "" {
			fn.ClientLimitWindow = domainfunctions.ClientLimitWindowDay
		}
	}
	return nil
}

func (f *Functions) DeleteFunction(ctx context.Context, projectID, functionID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return err
	}
	if fn == nil {
		return status.Error(codes.NotFound, "function not found")
	}
	if err := f.repo.DeleteFunction(ctx, projectID, functionID); err != nil {
		return err
	}
	f.cache.invalidate(projectID, functionID)
	return nil
}

// SetFunctionScopes 全量替换函数 declared_scopes（P0 执行身份，镜像
// RotateFunctionSecret 的敏感独立 RPC 先例；UpdateFunctionRequest 无
// repeated 字段的未设置语义，故独立 RPC）。校验与去重同 CreateFunction；
// 空集合法（撤销全部平台访问）。
func (f *Functions) SetFunctionScopes(ctx context.Context, projectID, functionID string, scopes []string) (*domainfunctions.Function, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, status.Error(codes.NotFound, "function not found")
	}
	normalized, err := domainfunctions.NormalizeDeclaredScopes(scopes)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	fn.DeclaredScopes = normalized
	fn.UpdatedAt = f.nowFn()
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, err
	}
	f.cache.invalidate(projectID, functionID)
	return fn, nil
}

// RotateFunctionSecret 轮换 endpoint target 签名密钥（v4 §3/§4 v2 双签语义，
// 与 webhooks RotateWebhookSecret 同构）：新钥即刻生效（v1 签名），旧钥移入
// previous 槽、宽限窗内调用侧同时携带 v2 旧钥签名——函数侧持旧钥的 FDK
// 过渡期不断流。返回**一次性新明文**。
func (f *Functions) RotateFunctionSecret(ctx context.Context, projectID, functionID string) (*domainfunctions.Function, string, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, "", err
	}
	fn, err := f.repo.GetFunction(ctx, projectID, functionID)
	if err != nil {
		return nil, "", err
	}
	if fn == nil {
		return nil, "", status.Error(codes.NotFound, "function not found")
	}
	secret, err := generateTargetSecret()
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "generate target secret: %v", err)
	}
	enc, err := secretbox.Encrypt(secret, f.encKey)
	if err != nil {
		return nil, "", status.Errorf(codes.Internal, "encrypt target secret: %v", err)
	}
	now := f.nowFn()
	fn.TargetPreviousSecretEnc = fn.TargetSecretEnc
	fn.TargetSecretRotatedAt = now
	fn.TargetSecretEnc = enc
	fn.TargetSecretFingerprint = domainfunctions.Fingerprint(secret)
	fn.UpdatedAt = now
	if err := f.repo.UpdateFunction(ctx, fn); err != nil {
		return nil, "", err
	}
	f.cache.invalidate(projectID, functionID)
	return fn, secret, nil
}

// decryptTargetSecrets 解密生效密钥对（当前钥 + 宽限窗内的旧钥；窗外的
// previous 不再携带——调用侧 v2 签名通道收敛）。解密失败原样上抛（调用方
// 按平台失败终态化当次调用——密钥材料配置错误的可恢复语义）。
func (f *Functions) decryptTargetSecrets(fn *domainfunctions.Function) (current, previous string, err error) {
	current, err = secretbox.Decrypt(fn.TargetSecretEnc, f.encKey)
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "decrypt target secret %s: %v", fn.ID, err)
	}
	if fn.TargetPreviousSecretEnc != "" && effectivePreviousSecret(f.nowFn(), fn.TargetSecretRotatedAt, f.secretGrace) {
		previous, err = secretbox.Decrypt(fn.TargetPreviousSecretEnc, f.encKey)
		if err != nil {
			// 旧钥解密失败不阻断调用（新钥仍有效）：降级为单钥签名。
			f.logger().Warn("functions: decrypt previous target secret failed; invoking with current only",
				"function_id", fn.ID, "error", err)
			previous = ""
		}
	}
	return current, previous, nil
}

// effectivePreviousSecret 报告旧钥是否仍在宽限窗内（rotatedAt 为零值 =
// 从未轮换，不在窗内；window <=0 用缺省窗）。
func effectivePreviousSecret(now, rotatedAt time.Time, window time.Duration) bool {
	if rotatedAt.IsZero() {
		return false
	}
	if window <= 0 {
		window = domainfunctions.TargetSecretRotationWindow
	}
	return now.Sub(rotatedAt) < window
}

// validateTargetURL 校验 endpoint target（创建/更新期应用层校验；protovalidate
// 管形状上限，业务规则在此）：URL 形状合法（绝对 http(s) URL + host 非空）、
// 生产强制 https（开发环境放行 http——TORCHWOOD_ENV 裁决，v4 §3）。
func validateTargetURL(u string, env config.RuntimeEnv) error {
	if u == "" {
		return status.Error(codes.InvalidArgument, "target_url is required")
	}
	if len(u) > maxTargetURLBytes {
		return status.Errorf(codes.InvalidArgument, "target_url exceeds maximum of %d bytes", maxTargetURLBytes)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "target_url: %v", err)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if env != config.EnvDevelopment {
			return status.Error(codes.InvalidArgument, "target_url must use https scheme (http is only allowed in development)")
		}
	default:
		return status.Error(codes.InvalidArgument, "target_url must be an absolute http(s) URL")
	}
	if parsed.Host == "" {
		return status.Error(codes.InvalidArgument, "target_url: host is required")
	}
	return nil
}

// generateTargetSecret 生成签名密钥明文（twfnsec_ + 32 字节 base64url，
// 43 字符不可猜；与 webhooks generateSecret 同构）。
func generateTargetSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return domainfunctions.SecretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
