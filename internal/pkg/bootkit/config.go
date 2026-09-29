package bootkit

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
)

// ValidateAppConfig 校验安全相关配置并按需告警，server 与 worker 共用同一
// fail-closed 口径（Round4 J4-1 前仅 server 校验、worker 静默跳过）：
//   - 显式 security.encryption_key 套用与 jwt.secret 相同的强度规则（不合规
//     拒绝启动）；未配置时回退 jwt.secret 仅告警（历史行为，W-I）；
//   - jwt.secret 强度校验。worker 虽不签发用户 JWT，但会用主密钥派生页 token
//     验签密钥并消费 server 签发的 page_token（见 InitPageTokenSigning），
//     弱主密钥属同一攻击面，故一并拒绝启动。
func ValidateAppConfig(logger *slog.Logger, c *config.AppConfig) error {
	if key, fallback := config.EncryptionSecret(c); fallback {
		logger.Warn("security.encryption_key is not set: static encryption (OAuth/TOTP secrets) falls back to security.jwt.secret; configure a dedicated key (env TORCHWOOD_SECURITY_ENCRYPTION_KEY)")
	} else if err := ValidateSecret("security.encryption_key", "TORCHWOOD_SECURITY_ENCRYPTION_KEY", key); err != nil {
		return err
	}
	// M5 C8：setup_token 非空即套用主密钥强度下界（≥32 字节 + 弱子串拒绝）
	// ——引导凭证与主密钥同一攻击面；空值表示未启用 setup 面，跳过。
	if token := c.GetSecurity().GetSetupToken(); strings.TrimSpace(token) != "" {
		if err := ValidateSecret("security.setup_token", "TORCHWOOD_SECURITY_SETUP_TOKEN", token); err != nil {
			return err
		}
	}
	return ValidateJWTSecret(c.GetSecurity().GetJwt().GetSecret())
}

// ValidateFunctionsDispatchConfig 校验函数分发通路配置：v1 docker 执行器已
// 移除，函数执行统一经 dispatcher 分发，functions.dispatcher.url
// 必填（启动期 fail-fast，不留到首次执行）。仅 server/worker 组合根调用本
// 函数——dispatcher 进程自身是通路终点，不消费 url 键。
func ValidateFunctionsDispatchConfig(c *config.AppConfig) error {
	if c.GetFunctions().GetDispatcher().GetUrl() == "" {
		return fmt.Errorf("functions.dispatcher.url is required (function execution requires the dispatcher service; env TORCHWOOD_FUNCTIONS_DISPATCHER_URL)")
	}
	return nil
}

// ValidateFunctionsFleetlyConfig 校验 dispatcher 进程的 fleetly 控制面配置
// （IMPL-T2-3）：endpoint 与机具令牌必填（缺失即拒绝启动——否则构建/执行在
// 首次调用才以模糊错误暴露）；network_members 非空时 app 必填（挂靠声明的
// 归属 app 不可省）。endpoint 二选一：显式 functions.fleetly.endpoint（形态
// 经 config.ParseFleetlyEndpoint 校验：裸 host:port 明文 / tls:// /
// tls-insecure://，未知 scheme 启动期拒绝）或平台物化的
// FLEETLY_CONTROL_GRPC_ADDR（engine ctrlinject 注入任务 spec 的控制面地址，
// 集群内工作负载零配置回落）。IMPL-T2-5 起由 ValidateFunctionsDriverConfig
// 在 driver=fleetly 分支调用。
func ValidateFunctionsFleetlyConfig(c *config.AppConfig) error {
	f := c.GetFunctions().GetFleetly()
	explicit := strings.TrimSpace(f.GetEndpoint())
	if explicit == "" && strings.TrimSpace(os.Getenv("FLEETLY_CONTROL_GRPC_ADDR")) == "" {
		return fmt.Errorf("functions.fleetly.endpoint is required (the dispatcher runs function tasks and builds on the fleetly platform; env TORCHWOOD_FUNCTIONS_FLEETLY_ENDPOINT, or the platform-materialized FLEETLY_CONTROL_GRPC_ADDR for in-cluster workloads)")
	}
	if explicit != "" {
		if _, _, err := config.ParseFleetlyEndpoint(explicit); err != nil {
			return err
		}
	}
	if strings.TrimSpace(f.GetToken()) == "" {
		return fmt.Errorf("functions.fleetly.token is required (machine token with the tasks,build scopes; inject it via env TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN, never commit it to a config file)")
	}
	if len(f.GetNetworkMembers()) > 0 && strings.TrimSpace(f.GetApp()) == "" {
		return fmt.Errorf("functions.fleetly.app is required when functions.fleetly.network_members is set (task-network members belong to this fleetly app; env TORCHWOOD_FUNCTIONS_FLEETLY_APP)")
	}
	return nil
}

// ValidateFunctionsDriverConfig 校验函数执行底座驱动选择并按驱动分发细节
// 校验（IMPL-T2-5 双执行底座）：
//   - functions.driver 未设置/未知值 fail-closed（启动期拒绝，错误文案列出
//     两选项与各自配置键——不留到首次构建/执行才暴露）；
//   - driver=fleetly → ValidateFunctionsFleetlyConfig（endpoint/机具令牌
//     必填，IMPL-T2-3 口径不变）；
//   - driver=docker → docker 底座段无必填键（host 可空，dockerdriver 侧
//     回落缺省 unix:///var/run/docker.sock；docker.sock 可达性与 fleetly
//     端点拨号同策略——首次调用暴露）。
//
// 仅 dispatcher 组合根调用（server/worker 零执行底座消费）。
func ValidateFunctionsDriverConfig(c *config.AppConfig) error {
	driver := strings.TrimSpace(c.GetFunctions().GetDriver())
	switch driver {
	case config.FunctionsDriverFleetly:
		return ValidateFunctionsFleetlyConfig(c)
	case config.FunctionsDriverDocker:
		return nil
	case "":
		return fmt.Errorf("functions.driver is required: set it to %q for the fleetly platform (requires functions.fleetly.endpoint and functions.fleetly.token; env TORCHWOOD_FUNCTIONS_DRIVER=fleetly) or %q for direct docker execution (requires docker.sock access; env TORCHWOOD_FUNCTIONS_DRIVER=docker)",
			config.FunctionsDriverFleetly, config.FunctionsDriverDocker)
	default:
		return fmt.Errorf("functions.driver %q is unknown: supported values are %q (fleetly platform; requires functions.fleetly.endpoint and functions.fleetly.token) and %q (direct docker execution; requires docker.sock access)",
			driver, config.FunctionsDriverFleetly, config.FunctionsDriverDocker)
	}
}

// WeakSecretTokens 是已知弱默认值/常见占位密钥的子串黑名单。
var WeakSecretTokens = []string{
	"change-me",
	"changeme",
	"minioadmin",
	"secret",
	"password",
	"torchwood",
}

// MinSecretLen 是主密钥的最小长度（HS256 / secretbox 密钥熵下界）。
const MinSecretLen = 32

// ValidateSecret 拒绝空值、过短密钥与任何含已知弱子串的密钥：命中弱
// 子串即整体拒绝（而不只是 Warn），否则 "change-me" 之类的占位默认值只要
// 拼够长度就能绕过长度检查，弱密钥子串是实际绕过手法中最常见的一类。
func ValidateSecret(fieldPath, envName, secret string) error {
	s := strings.TrimSpace(secret)
	if s == "" {
		return fmt.Errorf("%s must be set (env %s)", fieldPath, envName)
	}
	if len(s) < MinSecretLen {
		return fmt.Errorf("%s is too short (%d chars): must be at least %d characters (env %s)", fieldPath, len(s), MinSecretLen, envName)
	}
	lower := strings.ToLower(s)
	for _, w := range WeakSecretTokens {
		if strings.Contains(lower, w) {
			return fmt.Errorf("%s contains known weak value %q; generate a strong random secret (env %s)", fieldPath, w, envName)
		}
	}
	return nil
}

// ValidateJWTSecret 按主密钥强度规则校验 JWT 主密钥。
func ValidateJWTSecret(secret string) error {
	return ValidateSecret("security.jwt.secret", "TORCHWOOD_SECURITY_JWT_SECRET", secret)
}

// InitPageTokenSigning 启用页 token HMAC 签名（R4-J2-4）：purpose 从
// security.jwt.secret 派生。server 签发、worker 的 outbox dead-letter 列表
// 验签消费同一主密钥；未配置主密钥即拒绝启动，与 JWT 校验同一 fail-closed 口径。
func InitPageTokenSigning(c *config.AppConfig) error {
	if err := crud.InitPageTokenSigning(c.GetSecurity().GetJwt().GetSecret()); err != nil {
		return fmt.Errorf("init page token signing: %w", err)
	}
	return nil
}

// InitRolesSigSigning 启用 roles GUC 签名密钥的进程内派生（阶段③-b 包 C，A2：
// HMAC-SHA256(jwt.secret, "tw-roles-guc-v1")，page-token 同模式）。派生钥供
// tw_app 身份注入 app.roles_sig GUC 时签名——注入用的是进程内钥，无需读库，
// server/worker 均注入。tw_secrets 落库由部署期 owner 一次性作业完成
// （`torchwood admin sync-roles-sig`，转出 POC 门禁 B15：运行 DSN 对
// tw_secrets 零权限，启动钩子已退役）。密钥未落库（部署时序未跑作业）时
// tw_app 查询 fail-closed（零角色）属预期，见 13-operations §4.5。
func InitRolesSigSigning(c *config.AppConfig) error {
	if err := clients.InitRolesSigKey(c.GetSecurity().GetJwt().GetSecret()); err != nil {
		return fmt.Errorf("init roles sig signing: %w", err)
	}
	return nil
}
