package config

import (
	"os"
	"strings"
	"time"
)

// 运行时环境（TORCHWOOD_ENV）与排水窗口。Lynx 在 NewRunner 时就要 DrainTimeout
// （早于 YAML 绑定），因此这两项只从进程环境读取，不进 config.proto。
const (
	EnvVarRuntime      = "TORCHWOOD_ENV"
	EnvVarDrainTimeout = "TORCHWOOD_SERVER_DRAIN_TIMEOUT"

	productionDrainTimeout = 30 * time.Second
)

// RuntimeEnv 是进程运行时环境。未知/空值按生产处理（保留排水窗口）。
type RuntimeEnv string

const (
	EnvDevelopment RuntimeEnv = "development"
	EnvProduction  RuntimeEnv = "production"
)

// ParseRuntimeEnv 把 TORCHWOOD_ENV 的原始值归一化。
// development/dev/local/test → development；其余（含空）→ production。
func ParseRuntimeEnv(raw string) RuntimeEnv {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "development", "dev", "local", "test":
		return EnvDevelopment
	default:
		return EnvProduction
	}
}

// CurrentRuntimeEnv 读取 TORCHWOOD_ENV。
func CurrentRuntimeEnv() RuntimeEnv {
	return ParseRuntimeEnv(os.Getenv(EnvVarRuntime))
}

// DrainTimeoutFor 按环境给出排水窗口；override 为合法非负 duration 时覆盖默认。
func DrainTimeoutFor(env RuntimeEnv, override string) time.Duration {
	def := productionDrainTimeout
	if env == EnvDevelopment {
		def = 0
	}
	raw := strings.TrimSpace(override)
	if raw == "" {
		return def
	}
	// "0" 无单位时 time.ParseDuration 在部分版本会失败；裸 0 就是关掉排水。
	if raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// CurrentDrainTimeout 读取 TORCHWOOD_ENV 与可选的 TORCHWOOD_SERVER_DRAIN_TIMEOUT。
func CurrentDrainTimeout() time.Duration {
	return DrainTimeoutFor(CurrentRuntimeEnv(), os.Getenv(EnvVarDrainTimeout))
}
