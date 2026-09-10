package functions

import (
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

// executor 值域（functions.executor；config.yaml.template 注释引导）。
const (
	// ExecutorV1Docker 是 v1 回退模式：每请求一容器、进程内执行（docker.sock
	// 需挂载进 server/worker）。默认值——二选一，不同时启用。
	ExecutorV1Docker = "docker"
	// ExecutorV2Dispatcher 是 v2 默认执行模型（常驻 runner）：经
	// functions-dispatcher 分发（docker.sock 收敛到 dispatcher 进程，
	// server/worker 零 daemon 依赖）。
	ExecutorV2Dispatcher = "dispatcher"
)

// ProvideExecutor 按配置绑定 Executor 实现（server/worker 组合根共用）：
//   - "docker"（默认）→ v1 DockerExecutor（回退 = v1 进程内执行）；
//   - "dispatcher" → v2 DispatcherExecutor（常驻 runner 为默认执行模型）；
//   - 未知值启动失败（fail-closed，不静默回落）。
//
// 两个实现都构造（构造均为惰性——v1 的 docker client 与 v2 的 HTTP 客户端
// 都把配置错误延迟到首次调用），但只有一个接入 Executor 端口。
func ProvideExecutor(cfg *config.AppConfig, dockerExec *DockerExecutor, dispatcherExec *DispatcherExecutor) (domainfunctions.Executor, error) {
	switch cfg.GetFunctions().GetExecutor() {
	case "", ExecutorV1Docker:
		return dockerExec, nil
	case ExecutorV2Dispatcher:
		return dispatcherExec, nil
	default:
		return nil, &executorConfigError{value: cfg.GetFunctions().GetExecutor()}
	}
}

type executorConfigError struct{ value string }

func (e *executorConfigError) Error() string {
	return "invalid functions.executor value " + e.value + " (expected \"docker\" or \"dispatcher\")"
}
