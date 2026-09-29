package dispatcher

import (
	"fmt"
	"time"
)

// RunnerDrainTimeout 是 runner 排水定时器上限（TW_DRAIN_TIMEOUT_MS 注入值）：
// 收到 SIGTERM/平台 stop 后完成在途请求的预算。两种执行形态同值——runner
// 侧是单一实现，注入值分叉即排水语义分叉。
const RunnerDrainTimeout = 10 * time.Second

// runnerCredKeys 是请求侧 env 的凭证排除名单：TW_DATA 由请求体承载、
// TW_EXECUTION_TOKEN 经分发 header 传递——都不进常驻容器 env（常驻的是
// 容器不是凭证）。
var runnerCredKeys = map[string]struct{}{
	"TW_DATA":            {},
	"TW_EXECUTION_TOKEN": {},
}

// SanitizeRunnerEnv 把请求侧 env 组装为容器 env 形态（[]string k=v），剔除
// 凭证键。池 spawn 与部署验证 spawn 两条路径共用本实现——凭证不进容器 env
// 的不变量在此单点，新增 spawn 路径不得绕过。
func SanitizeRunnerEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if _, excluded := runnerCredKeys[k]; excluded {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// AppendRunnerControlEnv 注入 runner 自回收控制键：TW_MAX_REQUESTS（达阈
// 自退）与 TW_DRAIN_TIMEOUT_MS（排水上限，RunnerDrainTimeout）。两种驱动
// 形态的 SpawnInstance 共用——漏注入任一键 = runner 永不自退静默泄漏；
// 收进单一构造器后，驱动侧只有「调用/不调用」一种选择，而非逐键手写。
func AppendRunnerControlEnv(env []string, maxRequests int) []string {
	out := make([]string, 0, len(env)+2)
	out = append(out, env...)
	out = append(out,
		fmt.Sprintf("TW_MAX_REQUESTS=%d", maxRequests),
		fmt.Sprintf("TW_DRAIN_TIMEOUT_MS=%d", RunnerDrainTimeout.Milliseconds()),
	)
	return out
}
