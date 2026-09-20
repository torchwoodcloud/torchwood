package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"
)

// unsetForTest 让环境变量在本用例内真实缺席（结束后还原）：不能只置空串——
// godotenv.Load 不覆盖已存在变量（含空值），空串会掩盖污染检测。
func unsetForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := os.LookupEnv(key); ok {
			t.Setenv(key, "") // 登记 cleanup：用例结束后还原原值
		}
		_ = os.Unsetenv(key)
	}
}

// TestMainNoDotEnvAutoLoad 防回归：CLI 入口不得自动加载 cwd 的 .env。
// CLI 常在不可信仓库目录运行（runbook / Agent 工作流）：若有人恢复
// godotenv.Load()，恶意仓库的 .env 即可设 TORCHWOOD_CLI_ENDPOINT /
// TORCHWOOD_CLI_API_KEY，把配置 profile 内的 API Key 以 x-api-key 重定向到
// 攻击者端点（TORCHWOOD_DATA_DATABASE_SOURCE 等 DSN 同理可被劫持）。
// 断言：在埋了恶意 .env 的 cwd 里走完整装配分发链，(1) 缺 key 校验如实
// 失败（.env 的 key/endpoint 未参与取值）；(2) 进程环境未被 .env 污染；
// (3) 无凭据命令（version）在不可信目录照常工作。
func TestMainNoDotEnvAutoLoad(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".env"),
		[]byte("TORCHWOOD_CLI_ENDPOINT=attacker.example:9060\n"+
			"TORCHWOOD_CLI_API_KEY=stolen-key\n"+
			"TORCHWOOD_DATA_DATABASE_SOURCE=postgres://attacker/x\n"),
		0o600,
	))
	t.Chdir(dir)

	// 隔离外部环境：TORCHWOOD_CLI_* 与 DSN 变量真实缺席（mise/开发机 shell
	// 常带 TORCHWOOD_DATA_DATABASE_SOURCE）；配置文件指向缺失路径，
	// 不受开发机 ~/.torchwood/config.yaml 影响。
	unsetForTest(t,
		"TORCHWOOD_CLI_ENDPOINT",
		"TORCHWOOD_CLI_API_KEY",
		"TORCHWOOD_DATA_DATABASE_SOURCE",
	)
	t.Setenv("TORCHWOOD_CLI_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))

	newEnv := func() (*commands.Environment, *bytes.Buffer, *bytes.Buffer) {
		var out, errOut bytes.Buffer
		return &commands.Environment{Stdout: &out, Stderr: &errOut}, &out, &errOut
	}

	// (1)+(2) 需要凭据的动词：validate 链在干净环境下报缺 key（退出码 1）。
	env, _, errOut := newEnv()
	code := run(env, []string{"users", "list"})
	require.Equal(t, 1, code, "stderr: %s", errOut.String())
	require.Contains(t, errOut.String(), "missing API key")
	for _, key := range []string{
		"TORCHWOOD_CLI_ENDPOINT",
		"TORCHWOOD_CLI_API_KEY",
		"TORCHWOOD_DATA_DATABASE_SOURCE",
	} {
		require.Empty(t, os.Getenv(key), "cwd .env leaked into process env: %s", key)
	}

	// (3) version 不依赖凭据，不可信目录下装配路径照常工作。
	env, out, errOut := newEnv()
	require.Equal(t, commands.ExitOK, run(env, []string{"version"}))
	require.Contains(t, out.String(), "torchwood")
	require.Empty(t, errOut.String())
}
