package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestConfigTemplateValid 防回归（round4 P1-1）：config.yaml.template 必须
// 是合法 YAML 且键结构与 config.proto 对齐——曾因 access_ttl/refresh_ttl
// 缩进漂移到 security.encryption_key 之下导致整个文件解析失败。
func TestConfigTemplateValid(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../../configs/config.yaml.template")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc), "template must be valid YAML")

	security, ok := doc["security"].(map[string]any)
	require.True(t, ok, "security must be a mapping, got %T", doc["security"])

	jwt, ok := security["jwt"].(map[string]any)
	require.True(t, ok, "security.jwt must be a mapping, got %T", security["jwt"])
	require.Contains(t, jwt, "secret")
	require.Contains(t, jwt, "access_ttl", "access_ttl 必须位于 jwt 块内")
	require.Contains(t, jwt, "refresh_ttl", "refresh_ttl 必须位于 jwt 块内")

	require.Contains(t, security, "encryption_key", "encryption_key 属于 security 层而非 jwt 块")
}
