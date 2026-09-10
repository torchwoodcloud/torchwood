package config

import "strings"

// EncryptionSecret 返回静态数据加密密钥（OAuth client secret / TOTP secret
// 等 secretbox 加密）。显式 security.encryption_key 优先；未配置时回退
// security.jwt.secret 并返回 fallback=true（调用方启动期告警，W-I）。
// 回退路径下行为与历史版本一致；配置独立密钥后，读取侧以双密钥兼容
// 存量密文（见 oauth_provider_repo / totp 的 decrypt 回退）。
func EncryptionSecret(cfg *AppConfig) (string, bool) {
	if cfg == nil {
		return "", false
	}
	if k := strings.TrimSpace(cfg.GetSecurity().GetEncryptionKey()); k != "" {
		return k, false
	}
	return cfg.GetSecurity().GetJwt().GetSecret(), true
}
