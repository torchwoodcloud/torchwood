package idgen

// APIKeySecretPrefix 是 API key 明文 secret 的固定前缀，用于在客户端配置与
// 日志中一眼识别 Torchwood API key（对齐 OpenAI 风格的 sk- 惯例）。
const APIKeySecretPrefix = "sk-"

// APIKeySecret 生成带 sk- 前缀的 API key 明文 secret（两个 UUID 拼接）。
// 明文仅在创建响应返回一次；持久化侧只存 SHA-256，校验按哈希反查、
// 不解析明文形状，因此前缀是纯标识、不参与认证判定。
func APIKeySecret() string {
	return APIKeySecretPrefix + UUID().String() + UUID().String()
}
