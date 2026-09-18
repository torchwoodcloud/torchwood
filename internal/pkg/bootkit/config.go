package bootkit

import (
	"fmt"
	"log/slog"
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
// 必填（启动期 fail-fast，不留到首次执行）；执行路由模式校验见
// ValidateFunctionsRoutingConfig（四期 4b 起并入本函数，server/worker 与
// dispatcher 进程同一口径）。仅 server/worker 组合根调用本函数
// ——dispatcher 进程自身是通路终点，不消费 url 键，只调用路由模式校验。
func ValidateFunctionsDispatchConfig(c *config.AppConfig) error {
	if c.GetFunctions().GetDispatcher().GetUrl() == "" {
		return fmt.Errorf("functions.dispatcher.url is required (function execution requires the dispatcher service; env TORCHWOOD_FUNCTIONS_DISPATCHER_URL)")
	}
	return ValidateFunctionsRoutingConfig(c)
}

// ValidateFunctionsRoutingConfig 校验执行路由模式配置（四期 4b，M7；设计
// docs/design/functions-runtimes-and-sources.md §4）：
//   - routing_mode 仅 "local"（缺省）/ "registry" 两值，其他值拒绝启动
//     （字符串笔误静默降级 local = 镜像全局化从未发生而路由语义看似成立，
//     排障面远劣于 fail-fast）；
//   - registry 模式必须 registry_push=true（镜像全局化是 registry 路由的
//     硬前提——push 未开则他节点永远拉不到镜像，冷启动必败）；
//   - registry 模式必须 node_url 非空（对等必须能反连本节点；端口推导的
//     "http://127.0.0.1:<port>" 跨节点不可达，registry 模式下不再有
//     「转发 BuildNode」兜底，反连地址错 = 全部冷启动失败）。
//
// server/worker（经 ValidateFunctionsDispatchConfig）与 dispatcher 进程
// （cmd/dispatcher NewAppConfig 直调）共用同一 fail-closed 口径。
func ValidateFunctionsRoutingConfig(c *config.AppConfig) error {
	d := c.GetFunctions().GetDispatcher()
	switch mode := d.GetRoutingMode(); mode {
	case "", config.FunctionsRoutingModeLocal, config.FunctionsRoutingModeRegistry:
	default:
		return fmt.Errorf("functions.dispatcher.routing_mode %q is invalid: must be \"local\" or \"registry\" (env TORCHWOOD_FUNCTIONS_DISPATCHER_ROUTING_MODE)", mode)
	}
	if d.GetRoutingMode() != config.FunctionsRoutingModeRegistry {
		return nil
	}
	if !d.GetRegistryPush() {
		return fmt.Errorf("functions.dispatcher.registry_push must be true when routing_mode is \"registry\": registry routing requires image globalization (builds must push to functions.docker.registry; env TORCHWOOD_FUNCTIONS_DISPATCHER_REGISTRY_PUSH)")
	}
	if strings.TrimSpace(d.GetNodeUrl()) == "" {
		return fmt.Errorf("functions.dispatcher.node_url is required when routing_mode is \"registry\": peer nodes must be able to reach this node (env TORCHWOOD_FUNCTIONS_DISPATCHER_NODE_URL)")
	}
	return nil
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
