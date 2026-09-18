package bootkit

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

func TestValidateJWTSecret(t *testing.T) {
	t.Parallel()

	// 40 字符的强随机串，不含任何弱子串。
	const strong = "B9f2kQx7LmZp4RtW8vNc2hJ6xKq3sM5uA1eD7gYp"

	cases := []struct {
		name    string
		secret  string
		wantErr string // 空串表示期望通过
	}{
		{"empty", "", "must be set"},
		{"whitespace only", "   \t  ", "must be set"},
		{"short", "abcdefgh", "too short"},
		{"short but exact weak value", "change-me", "too short"},
		{"weak value padded to length", strong[:20] + "changeme" + strong[28:], "known weak"},
		{"weak substring in middle", "B9f2kQ-x7Lm-change-me-Zp4RtW8vNc2hJ6xKq3sM", "known weak"},
		{"weak substring case-insensitive", "MINIOADMIN" + strong[10:], "known weak"},
		{"substring resembling secret", "Xy1" + "secret" + strings.Repeat("z", 29), "known weak"},
		{"strong value", strong, ""},
		{"strong value with surrounding spaces", "  " + strong + "  ", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateJWTSecret(tc.secret)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidateJWTSecret_WeakSubstringNeverPasses(t *testing.T) {
	t.Parallel()
	for _, w := range WeakSecretTokens {
		secret := "K7pQ2xR9mW4tZ8vC3hN6jB1sL5dF0gY" + w + "H8uE2iA4oP6qS7rT9wX"
		require.Error(t, ValidateJWTSecret(secret), "weak token %q must be rejected", w)
	}
}

// TestValidateFunctionsRoutingConfig 执行路由模式校验（四期 4b M7）表驱动：
// local（缺省/显式）通过；registry 全要素通过；非法值 / registry 缺
// registry_push / 缺 node_url 拒绝启动（fail-fast 优于静默降级）。
func TestValidateFunctionsRoutingConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		routingMode  string
		registryPush bool
		nodeURL      string
		wantErr      string // 空串表示期望通过
	}{
		{name: "local 缺省通过（向后兼容）", routingMode: ""},
		{name: "local 显式通过", routingMode: "local"},
		{name: "registry 全要素通过", routingMode: "registry", registryPush: true, nodeURL: "http://dispatcher-1:9070"},
		{name: "非法值拒绝（replicated 实验档不在实现范围）", routingMode: "replicated", registryPush: true, nodeURL: "http://d:9070", wantErr: `routing_mode "replicated" is invalid`},
		{name: "非法值拒绝（任意串）", routingMode: "registory", wantErr: `routing_mode "registory" is invalid`},
		{name: "registry 缺 registry_push 拒绝", routingMode: "registry", registryPush: false, nodeURL: "http://d:9070", wantErr: "registry_push must be true"},
		{name: "registry 缺 node_url 拒绝", routingMode: "registry", registryPush: true, nodeURL: "", wantErr: "node_url is required"},
		{name: "registry node_url 空白拒绝", routingMode: "registry", registryPush: true, nodeURL: "   ", wantErr: "node_url is required"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.AppConfig{Functions: &config.Functions{
				Dispatcher: &config.Functions_Dispatcher{
					Url:          "http://dispatcher:9070",
					RoutingMode:  tc.routingMode,
					RegistryPush: tc.registryPush,
					NodeUrl:      tc.nodeURL,
				},
			}}
			err := ValidateFunctionsRoutingConfig(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestValidateFunctionsDispatchConfig_Composition 组合口径：url 必填保持在
// 前；路由模式校验并入后非法 routing_mode 同样经 DispatchConfig 入口拒绝
// （server/worker 组合根单一调用面）。
func TestValidateFunctionsDispatchConfig_Composition(t *testing.T) {
	t.Parallel()

	t.Run("url 缺失仍最先拒绝", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{RoutingMode: "bogus"},
		}}
		err := ValidateFunctionsDispatchConfig(cfg)
		require.ErrorContains(t, err, "functions.dispatcher.url is required")
	})

	t.Run("非法 routing_mode 经组合入口拒绝", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{Url: "http://dispatcher:9070", RoutingMode: "bogus"},
		}}
		require.ErrorContains(t, ValidateFunctionsDispatchConfig(cfg), "is invalid")
	})

	t.Run("local 缺省仅 url 必填即通过", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{Url: "http://dispatcher:9070"},
		}}
		require.NoError(t, ValidateFunctionsDispatchConfig(cfg))
	})
}

// M5 C8：setup_token 非空时套用主密钥强度下界（≥32 字节 + 弱子串拒绝）；
// 空值（未启用 setup 面）跳过。
func TestValidateAppConfig_SetupToken(t *testing.T) {
	t.Parallel()

	// 40 字符的强随机串，不含任何弱子串。
	const strong = "B9f2kQx7LmZp4RtW8vNc2hJ6xKq3sM5uA1eD7gYp"
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	t.Run("empty token allowed", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: strong},
		}}
		require.NoError(t, ValidateAppConfig(logger, cfg))
	})

	t.Run("short token rejected", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Security: &config.Security{
			SetupToken: "short-setup-token",
			Jwt:        &config.Security_Jwt{Secret: strong},
		}}
		err := ValidateAppConfig(logger, cfg)
		require.ErrorContains(t, err, "security.setup_token")
		require.ErrorContains(t, err, "too short")
	})

	t.Run("weak token rejected", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Security: &config.Security{
			SetupToken: strong[:20] + "changeme" + strong[28:],
			Jwt:        &config.Security_Jwt{Secret: strong},
		}}
		err := ValidateAppConfig(logger, cfg)
		require.ErrorContains(t, err, "security.setup_token contains known weak value")
	})

	t.Run("strong token allowed", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Security: &config.Security{
			SetupToken: strong,
			Jwt:        &config.Security_Jwt{Secret: strong},
		}}
		require.NoError(t, ValidateAppConfig(logger, cfg))
	})
}

func TestValidateAppConfig_EncryptionKey(t *testing.T) {
	t.Parallel()

	// 40 字符的强随机串，不含任何弱子串。
	const strong = "B9f2kQx7LmZp4RtW8vNc2hJ6xKq3sM5uA1eD7gYp"

	newLogger := func() (*slog.Logger, *bytes.Buffer) {
		var buf bytes.Buffer
		return slog.New(slog.NewTextHandler(&buf, nil)), &buf
	}

	t.Run("explicit weak value rejected", func(t *testing.T) {
		t.Parallel()
		logger, _ := newLogger()
		cfg := &config.AppConfig{Security: &config.Security{
			EncryptionKey: strong[:20] + "changeme" + strong[28:],
			Jwt:           &config.Security_Jwt{Secret: strong},
		}}
		err := ValidateAppConfig(logger, cfg)
		require.ErrorContains(t, err, "security.encryption_key contains known weak value")
		require.ErrorContains(t, err, "TORCHWOOD_SECURITY_ENCRYPTION_KEY")
	})

	t.Run("explicit short value rejected", func(t *testing.T) {
		t.Parallel()
		logger, _ := newLogger()
		cfg := &config.AppConfig{Security: &config.Security{
			EncryptionKey: "too-short-key",
			Jwt:           &config.Security_Jwt{Secret: strong},
		}}
		err := ValidateAppConfig(logger, cfg)
		require.ErrorContains(t, err, "security.encryption_key is too short")
		require.ErrorContains(t, err, "TORCHWOOD_SECURITY_ENCRYPTION_KEY")
	})

	t.Run("explicit strong value passes", func(t *testing.T) {
		t.Parallel()
		logger, buf := newLogger()
		cfg := &config.AppConfig{Security: &config.Security{
			EncryptionKey: " " + strong + " ",
			Jwt:           &config.Security_Jwt{Secret: strong},
		}}
		require.NoError(t, ValidateAppConfig(logger, cfg))
		require.NotContains(t, buf.String(), "encryption_key is not set")
	})

	t.Run("fallback to jwt.secret warns but passes", func(t *testing.T) {
		t.Parallel()
		logger, buf := newLogger()
		cfg := &config.AppConfig{Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: strong},
		}}
		require.NoError(t, ValidateAppConfig(logger, cfg))
		logs := buf.String()
		require.Contains(t, logs, "encryption_key is not set")
		require.Contains(t, logs, "falls back to security.jwt.secret")
	})
}
