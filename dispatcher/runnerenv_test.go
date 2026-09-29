package dispatcher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ——runner env 组装单点测试：两种驱动形态共用的 env 不变量在此钉死——
// （间接断言另见 daemon_verify_test TestSpawnVerifyInstance_EnvAssembly 与
// fleetly_daemon_test TestSpawnInstanceCreatesTaskWithScopeEnvAndResources）

// TestSanitizeRunnerEnv_CredentialExclusion 凭证键不进容器 env（常驻的是
// 容器不是凭证），其余用户变量原样透传。
func TestSanitizeRunnerEnv_CredentialExclusion(t *testing.T) {
	env := SanitizeRunnerEnv(map[string]string{ // #nosec G101 -- 测试夹具伪凭证（断言不进容器 env）
		"GREETING":           "hi",
		"EMPTY":              "",
		"TW_DATA":            `{"secret":"must-not-leak"}`,
		"TW_EXECUTION_TOKEN": "twx_must-not-leak",
	})
	require.ElementsMatch(t, []string{"GREETING=hi", "EMPTY="}, env)
}

// TestAppendRunnerControlEnv_ControlKeys 驱动侧注入自回收控制键：达阈自退
// + 排水上限；值取自共享常量（两形态分叉即 runner 排水语义分叉）。
func TestAppendRunnerControlEnv_ControlKeys(t *testing.T) {
	require.Equal(t, 10*time.Second, RunnerDrainTimeout,
		"排水上限与 runner 侧定时器同一常量口径")

	env := AppendRunnerControlEnv([]string{"GREETING=hi"}, 42)
	require.Equal(t, []string{
		"GREETING=hi",
		"TW_MAX_REQUESTS=42",
		"TW_DRAIN_TIMEOUT_MS=10000",
	}, env)

	// 空 env（零函数变量）也必须注入控制键。
	require.Equal(t, []string{
		"TW_MAX_REQUESTS=1",
		"TW_DRAIN_TIMEOUT_MS=10000",
	}, AppendRunnerControlEnv(nil, 1))
}
