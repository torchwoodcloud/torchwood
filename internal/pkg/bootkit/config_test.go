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

// TestValidateFunctionsDispatchConfig 分发通路校验（IMPL-T2-3 后仅 url 必填
// ——路由模式/多节点键已随细胞模型退役）：缺 url 拒绝；有 url 通过。
func TestValidateFunctionsDispatchConfig(t *testing.T) {
	t.Parallel()

	t.Run("url 缺失拒绝", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{},
		}}
		require.ErrorContains(t, ValidateFunctionsDispatchConfig(cfg), "functions.dispatcher.url is required")
	})

	t.Run("url 就位通过", func(t *testing.T) {
		t.Parallel()
		cfg := &config.AppConfig{Functions: &config.Functions{
			Dispatcher: &config.Functions_Dispatcher{Url: "http://dispatcher:9070"},
		}}
		require.NoError(t, ValidateFunctionsDispatchConfig(cfg))
	})
}

// TestValidateFunctionsFleetlyConfig fleetly 控制面配置校验（IMPL-T2-3）：
// endpoint/令牌必填；network_members 非空时 app 必填；完整配置通过。
func TestValidateFunctionsFleetlyConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fleetly *config.Functions_Fleetly
		wantErr string
	}{
		{name: "缺 endpoint 拒绝", fleetly: &config.Functions_Fleetly{Token: "tok"}, wantErr: "functions.fleetly.endpoint is required"},
		{name: "缺令牌拒绝", fleetly: &config.Functions_Fleetly{Endpoint: "fleetlyd:8421"}, wantErr: "functions.fleetly.token is required"},
		{
			name:    "有成员无 app 拒绝",
			fleetly: &config.Functions_Fleetly{Endpoint: "fleetlyd:8421", Token: "tok", NetworkMembers: []string{"dispatcher"}},
			wantErr: "functions.fleetly.app is required",
		},
		{name: "仅 endpoint+令牌通过（不声明挂靠）", fleetly: &config.Functions_Fleetly{Endpoint: "fleetlyd:8421", Token: "tok"}},
		{
			name:    "完整配置通过",
			fleetly: &config.Functions_Fleetly{Endpoint: "fleetlyd:8421", Token: "tok", App: "torchwood", NetworkMembers: []string{"dispatcher", "server"}},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.AppConfig{Functions: &config.Functions{Fleetly: tc.fleetly}}
			err := ValidateFunctionsFleetlyConfig(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestValidateFunctionsFleetlyConfig_PlatformAddrFallback 平台物化地址回落：
// 显式 endpoint 缺席但 FLEETLY_CONTROL_GRPC_ADDR（engine ctrlinject 注入的
// 任务 env）在场时合法——零配置集群内回拨通路（Setenv 禁并行，独立子测试）。
func TestValidateFunctionsFleetlyConfig_PlatformAddrFallback(t *testing.T) {
	t.Setenv("FLEETLY_CONTROL_GRPC_ADDR", "10.124.0.3:8421")
	cfg := &config.AppConfig{Functions: &config.Functions{
		Fleetly: &config.Functions_Fleetly{Token: "tok"},
	}}
	require.NoError(t, ValidateFunctionsFleetlyConfig(cfg))
}

// TestValidateFunctionsDriverConfig 执行底座驱动选择校验（IMPL-T2-5）：
// 未设/未知值 fail-closed 且点名两选项与配置键；按驱动分发——fleetly 分支
// 要求 endpoint/令牌，docker 分支无必填键（host 缺省由驱动回落）。
func TestValidateFunctionsDriverConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		driver  string
		fleetly *config.Functions_Fleetly
		wantErr string
	}{
		{
			name:    "未设置拒绝（点名两选项与配置键）",
			wantErr: "functions.driver is required",
		},
		{
			name:    "未设置拒绝（fleetly 选项）",
			wantErr: `for the fleetly platform (requires functions.fleetly.endpoint and functions.fleetly.token`,
		},
		{
			name:    "未设置拒绝（docker 选项）",
			wantErr: `or "docker" for direct docker execution (requires docker.sock access`,
		},
		{
			name:    "未知值拒绝",
			driver:  "nomad",
			wantErr: `functions.driver "nomad" is unknown`,
		},
		{
			name:    "未知值拒绝（列出合法值）",
			driver:  "nomad",
			wantErr: `supported values are "fleetly"`,
		},
		{
			name:    "fleetly 分支缺 endpoint 拒绝",
			driver:  "fleetly",
			fleetly: &config.Functions_Fleetly{Token: "tok"},
			wantErr: "functions.fleetly.endpoint is required",
		},
		{
			name:    "fleetly 分支完整配置通过",
			driver:  "fleetly",
			fleetly: &config.Functions_Fleetly{Endpoint: "fleetlyd:8421", Token: "tok"},
		},
		// docker 分支零必填键：host 由驱动回落缺省，fleetly 段无需存在。
		{name: "docker 分支零必填键通过（无 fleetly 段）", driver: "docker"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := &config.Functions{Driver: tc.driver}
			if tc.driver == config.FunctionsDriverDocker {
				fn.Docker = &config.Functions_Docker{Host: "unix:///var/run/docker.sock"}
			}
			fn.Fleetly = tc.fleetly
			cfg := &config.AppConfig{Functions: fn}
			err := ValidateFunctionsDriverConfig(cfg)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
	// 未设 driver 且 fleetly 段为 nil 的纯零值配置同样 fail-closed（最常见
	// 的存量配置漂移形态：既无 driver 也无 fleetly 段）。
	err := ValidateFunctionsDriverConfig(&config.AppConfig{Functions: &config.Functions{}})
	require.ErrorContains(t, err, "functions.driver is required")
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
