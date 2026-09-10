package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"
)

// isolateConfig 把配置文件指向临时目录的缺失路径并清空 TORCHWOOD_CLI_*
// 环境，保证用例不受开发机真实 ~/.torchwood/config.yaml 与 shell 环境影响。
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("TORCHWOOD_CLI_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("TORCHWOOD_CLI_ENDPOINT", "")
	t.Setenv("TORCHWOOD_CLI_API_KEY", "")
	t.Setenv("TORCHWOOD_CLI_TIMEOUT", "")
	t.Setenv("TORCHWOOD_CLI_OUTPUT", "")
	t.Setenv("TORCHWOOD_CLI_TLS", "")
	t.Setenv("TORCHWOOD_CLI_PROFILE", "")
}

// writeConfig 便捷写入一份配置文件并返回路径（不经过 CLI 写路径，专供
// arrange 阶段构造磁盘状态）。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	t.Setenv("TORCHWOOD_CLI_CONFIG", path)
	return path
}

func TestLoadConfigFile(t *testing.T) {
	t.Run("文件不存在返回 nil,nil", func(t *testing.T) {
		isolateConfig(t)
		path := os.Getenv("TORCHWOOD_CLI_CONFIG")
		cfg, err := loadConfigFile(path)
		require.NoError(t, err)
		require.Nil(t, cfg)
	})

	t.Run("正常解析", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod:
    endpoint: grpc.example.com:443
    api-key: secret-prod
    tls: true
    timeout: 1m
`)
		cfg, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
		require.NoError(t, err)
		require.Equal(t, "prod", cfg.Default)
		p := cfg.Profiles["prod"]
		require.NotNil(t, p)
		require.Equal(t, "grpc.example.com:443", p.Endpoint)
		require.Equal(t, "secret-prod", p.APIKey)
		require.True(t, p.TLS)
		require.Equal(t, "1m", p.Timeout)
	})

	t.Run("未知键严格报错（防拼错静默失效）", func(t *testing.T) {
		writeConfig(t, `
profiles:
  local:
    endpoint: 127.0.0.1:9060
    api_key: oops-snake-case
`)
		_, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "parse config")
	})

	t.Run("悬空 default 报错", func(t *testing.T) {
		writeConfig(t, `
default: gone
profiles:
  local:
    endpoint: 127.0.0.1:9060
`)
		_, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
		require.Error(t, err)
		require.Contains(t, err.Error(), `default profile "gone" does not exist`)
	})

	t.Run("非法 profile 名报错", func(t *testing.T) {
		writeConfig(t, `
profiles:
  "bad name!":
    endpoint: 127.0.0.1:9060
`)
		_, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid")
	})

	t.Run("非法 timeout 报错", func(t *testing.T) {
		writeConfig(t, `
profiles:
  local:
    timeout: soon
`)
		_, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid timeout")
	})
}

func TestResolveProfileSelection(t *testing.T) {
	g := func(name string) *globalFlags {
		return &globalFlags{profile: name}
	}
	t.Run("无配置文件无 profile", func(t *testing.T) {
		isolateConfig(t)
		p, err := g("").resolveProfile()
		require.NoError(t, err)
		require.Equal(t, profile{}, p)
	})

	t.Run("缺省走配置 default", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "a:1"}
  dev: {endpoint: "b:2"}
`)
		p, err := g("").resolveProfile()
		require.NoError(t, err)
		require.Equal(t, "a:1", p.Endpoint)
	})

	t.Run("显式 profile 覆盖 default", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "a:1"}
  dev: {endpoint: "b:2"}
`)
		p, err := g("dev").resolveProfile()
		require.NoError(t, err)
		require.Equal(t, "b:2", p.Endpoint)
	})

	t.Run("env 选 profile", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "a:1"}
  dev: {endpoint: "b:2"}
`)
		t.Setenv("TORCHWOOD_CLI_PROFILE", "dev")
		p, err := g("").resolveProfile()
		require.NoError(t, err)
		require.Equal(t, "b:2", p.Endpoint)
	})

	t.Run("显式旗标优先于 env", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "a:1"}
  dev: {endpoint: "b:2"}
`)
		t.Setenv("TORCHWOOD_CLI_PROFILE", "dev")
		gg := g("prod")
		gg.explicit.profile = true
		p, err := gg.resolveProfile()
		require.NoError(t, err)
		require.Equal(t, "a:1", p.Endpoint)
	})

	t.Run("未命中 profile 硬报错并列出可选项", func(t *testing.T) {
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "a:1"}
`)
		_, err := g("staging").resolveProfile()
		require.Error(t, err)
		require.Contains(t, err.Error(), `profile "staging" not found`)
		require.Contains(t, err.Error(), "prod")
	})
}

// TestApplyGlobalDefaultsPrecedence 固化取值优先级：
// 显式旗标 > TORCHWOOD_CLI_* env > profile > 内建默认。
func TestApplyGlobalDefaultsPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		field        string // endpoint / api-key / timeout / output / tls
		explicitFlag bool
		flagValue    string
		envValue     string
		profileValue string
		want         string
	}{
		{name: "全缺省用内建", field: "endpoint", want: defaultEndpoint},
		{name: "env 覆盖内建", field: "endpoint", envValue: "env:1", want: "env:1"},
		{name: "profile 覆盖内建", field: "endpoint", profileValue: "prof:1", want: "prof:1"},
		{name: "env 优先于 profile", field: "endpoint", envValue: "env:1", profileValue: "prof:1", want: "env:1"},
		{name: "显式旗标最高", field: "endpoint", explicitFlag: true, flagValue: "flag:1", envValue: "env:1", profileValue: "prof:1", want: "flag:1"},
		{name: "timeout env 优先", field: "timeout", envValue: "1m", profileValue: "2m", want: "1m"},
		{name: "api-key env 优先", field: "api-key", envValue: "envkey", profileValue: "profkey", want: "envkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			g := &globalFlags{endpoint: defaultEndpoint, timeout: defaultTimeout, output: defaultOutput}
			switch tt.field {
			case "endpoint":
				if tt.explicitFlag {
					g.endpoint, g.explicit.endpoint = tt.flagValue, true
				}
				t.Setenv(envEndpoint, tt.envValue)
				require.NoError(t, g.applyGlobalDefaults(profile{Endpoint: tt.profileValue}))
				require.Equal(t, tt.want, g.endpoint)
			case "api-key":
				if tt.explicitFlag {
					g.apiKey, g.explicit.apiKey = tt.flagValue, true
				}
				t.Setenv(envAPIKey, tt.envValue)
				require.NoError(t, g.applyGlobalDefaults(profile{APIKey: tt.profileValue}))
				require.Equal(t, tt.want, g.apiKey)
			case "timeout":
				if tt.explicitFlag {
					g.timeout, g.explicit.timeout = tt.flagValue, true
				}
				t.Setenv(envTimeout, tt.envValue)
				require.NoError(t, g.applyGlobalDefaults(profile{Timeout: tt.profileValue}))
				require.Equal(t, tt.want, g.timeout)
			}
		})
	}

	t.Run("tls：profile 生效；env 非法布尔硬报错", func(t *testing.T) {
		isolateConfig(t)
		g := &globalFlags{}
		require.NoError(t, g.applyGlobalDefaults(profile{TLS: true}))
		require.True(t, g.tls)

		t.Setenv(envTLS, "true")
		g2 := &globalFlags{}
		require.NoError(t, g2.applyGlobalDefaults(profile{}))
		require.True(t, g2.tls, "env 覆盖 profile")

		t.Setenv(envTLS, "notabool")
		g3 := &globalFlags{}
		require.ErrorContains(t, g3.applyGlobalDefaults(profile{TLS: true}), "must be a boolean")
	})
}

// TestGlobalFlagsDispatchEndToEnd 走真实 App 分发：分组层旗标存活、
// --profile 注入、validate 校验链。
func TestGlobalFlagsDispatchEndToEnd(t *testing.T) {
	newRun := func(t *testing.T) (func(args ...string) int, *bytes.Buffer, *bytes.Buffer) {
		isolateConfig(t)
		app := NewApp("test")
		var out, errOut bytes.Buffer
		env := &commands.Environment{Stdout: &out, Stderr: &errOut}
		return func(args ...string) int {
			return app.Run(context.Background(), env, args)
		}, &out, &errOut
	}

	t.Run("profile 提供 api-key 与 endpoint", func(t *testing.T) {
		run, _, _ := newRun(t)
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "127.0.0.1:1", api-key: profkey}
`)
		// key 来自 profile → 越过缺 key 校验，拨号失败归 5xx 类（3）
		require.Equal(t, 3, run("users", "list"))
	})

	t.Run("无 profile 缺 key 报 1", func(t *testing.T) {
		run, _, errOut := newRun(t)
		require.Equal(t, 1, run("users", "list"))
		require.Contains(t, errOut.String(), "missing API key")
		require.Contains(t, errOut.String(), "config profile")
	})

	t.Run("--profile 选择生效", func(t *testing.T) {
		run, _, _ := newRun(t)
		writeConfig(t, `
default: dev
profiles:
  dev: {api-key: devkey, endpoint: "10.255.255.1:9060"}
  prod: {api-key: profkey, endpoint: "127.0.0.1:1"}
`)
		// dev 指向不可达地址；--profile prod 切到 loopback:1 → 快速失败 3
		require.Equal(t, 3, run("users", "list", "--profile", "prod"))
	})

	t.Run("env 优先于 profile", func(t *testing.T) {
		run, _, _ := newRun(t)
		writeConfig(t, `
default: prod
profiles:
  prod: {endpoint: "10.255.255.1:9060", api-key: profkey, timeout: 30s}
`)
		t.Setenv("TORCHWOOD_CLI_ENDPOINT", "127.0.0.1:1")
		// env endpoint 覆盖 profile → 快速拨号失败 3（若 profile 生效会慢速超时）
		require.Equal(t, 3, run("users", "list"))
	})

	t.Run("分组层显式旗标存活到叶子", func(t *testing.T) {
		run, _, _ := newRun(t)
		require.Equal(t, 3, run("users", "--api-key", "k", "--endpoint", "127.0.0.1:1", "list"))
	})

	t.Run("配置文件损坏如实报错", func(t *testing.T) {
		run, _, errOut := newRun(t)
		writeConfig(t, "profiles: [not, a, map]")
		require.Equal(t, 1, run("health", "get"))
		require.Contains(t, errOut.String(), "parse config")
	})
}

// TestConfigVerbsFlow 端到端走 config 子命令组：path → set（含 --stdin 不测，
// 单元层覆盖）→ list → show → use → remove。
func TestConfigVerbsFlow(t *testing.T) {
	isolateConfig(t)
	app := NewApp("test")
	var out, errOut bytes.Buffer
	env := &commands.Environment{Stdout: &out, Stderr: &errOut}
	run := func(args ...string) int {
		out.Reset()
		errOut.Reset()
		return app.Run(context.Background(), env, args)
	}

	// path：文件不存在也打印路径
	require.Equal(t, commands.ExitOK, run("config", "path"))
	path := strings.TrimSpace(out.String())
	require.Equal(t, os.Getenv("TORCHWOOD_CLI_CONFIG"), path)

	// init：创建模板；重复 init 报错
	require.Equal(t, commands.ExitOK, run("config", "init"))
	require.FileExists(t, path)
	require.Equal(t, 1, run("config", "init"))
	require.Contains(t, errOut.String(), "already exists")

	// init 出的模板必须能被严格解析（config list 即验证）
	require.Equal(t, commands.ExitOK, run("config", "list"))
	var listed struct {
		Default  string `json:"default"`
		Profiles []struct {
			Name     string `json:"name"`
			Endpoint string `json:"endpoint"`
			APIKey   string `json:"apiKey"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &listed))
	require.Equal(t, "local", listed.Default)
	require.Len(t, listed.Profiles, 1)
	require.Equal(t, "local", listed.Profiles[0].Name)

	// set：新增第二个 profile + 改字段
	require.Equal(t, commands.ExitOK, run("config", "set", "prod", "endpoint", "grpc.example.com:443"))
	require.Equal(t, commands.ExitOK, run("config", "set", "prod", "api-key", "secret-prod-value-1234"))
	require.Equal(t, commands.ExitOK, run("config", "set", "prod", "tls", "true"))

	// show：api-key 打码（末 4 位），不泄露本体
	require.Equal(t, commands.ExitOK, run("config", "show", "prod"))
	var shown struct {
		Name   string `json:"name"`
		APIKey string `json:"apiKey"`
		TLS    bool   `json:"tls"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &shown))
	require.Equal(t, "prod", shown.Name)
	require.Equal(t, "****1234", shown.APIKey)
	require.True(t, shown.TLS)
	require.NotContains(t, out.String(), "secret-prod-value")

	// set：非法 key / 非法值报错且不落盘
	require.Equal(t, commands.ExitError, run("config", "set", "prod", "api_key", "x"))
	require.Equal(t, commands.ExitError, run("config", "set", "prod", "tls", "maybe"))
	require.Equal(t, commands.ExitOK, run("config", "show", "prod"))
	require.NoError(t, json.Unmarshal(out.Bytes(), &shown))
	require.True(t, shown.TLS)

	// use：切换缺省 profile
	require.Equal(t, commands.ExitOK, run("config", "use", "prod"))
	cfg, err := loadConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, "prod", cfg.Default)

	// use：不存在的 profile 报错并列出可选项
	require.Equal(t, commands.ExitError, run("config", "use", "nope"))
	require.Contains(t, errOut.String(), "not found")

	// remove：删除缺省 profile 时一并清 default；删到空仍可用
	require.Equal(t, commands.ExitOK, run("config", "remove", "prod"))
	cfg, err = loadConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, "", cfg.Default)
	require.NotContains(t, cfg.Profiles, "prod")
	require.Equal(t, commands.ExitOK, run("config", "remove", "local"))
	require.Equal(t, commands.ExitOK, run("config", "list"))
}

// TestConfigSetStdin 覆盖 api-key 经 stdin 注入（值不进 argv/shell 历史）。
func TestConfigSetStdin(t *testing.T) {
	isolateConfig(t)
	app := NewApp("test")
	var out, errOut bytes.Buffer
	env := &commands.Environment{Stdout: &out, Stderr: &errOut}

	// 临时替换 os.Stdin（commands.Environment 不含 Stdin，set --stdin 读 os.Stdin）
	r, w, err := os.Pipe()
	require.NoError(t, err)
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	_, err = w.WriteString("  std-key-1234567890  \n")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	code := app.Run(context.Background(), env, []string{"config", "set", "--stdin", "prod", "api-key"})
	require.Equal(t, commands.ExitOK, code)
	cfg, err := loadConfigFile(os.Getenv("TORCHWOOD_CLI_CONFIG"))
	require.NoError(t, err)
	require.Equal(t, "std-key-1234567890", cfg.Profiles["prod"].APIKey)
}

func TestMaskAPIKey(t *testing.T) {
	require.Equal(t, "", maskAPIKey(""))
	require.Equal(t, "****", maskAPIKey("short"))
	require.Equal(t, "****", maskAPIKey("exactly11ch"))
	require.Equal(t, "****1chr", maskAPIKey("exactly11chr"))
	require.Equal(t, "****hars", maskAPIKey("twelve-chars"))
}

// TestProfileNameRoundTrip 固化 profile 名规则边界。
func TestProfileNameRoundTrip(t *testing.T) {
	isolateConfig(t)
	for _, name := range []string{"local", "prod-1", "team_x", "A", strings.Repeat("a", 64)} {
		_, err := mutateConfigFile(func(cfg *cliConfig) error {
			_, err := setProfileField(cfg, name, "endpoint", "127.0.0.1:9060")
			return err
		})
		require.NoError(t, err, "profile name %q should be accepted", name)
	}
	for _, name := range []string{"", "-lead", "_lead", "has space", "汉化", strings.Repeat("a", 65)} {
		_, err := mutateConfigFile(func(cfg *cliConfig) error {
			_, err := setProfileField(cfg, name, "endpoint", "127.0.0.1:9060")
			return err
		})
		require.Error(t, err, "profile name %q should be rejected", name)
	}
}
