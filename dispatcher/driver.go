package dispatcher

// 本文件是双执行底座（IMPL-T2-5）的驱动集成面，也是 docker 驱动与
// fleetly 驱动路径之间唯一的桥：
//   - 驱动注册：docker 底座实现落隔离子包 dockerdriver（本仓唯一的
//     docker client 持有面），经 RegisterDockerDriver 注入构造函数，由
//     组合根（cmd/dispatcher）blank-import 完成链接；
//   - 驱动选择：newDaemonForConfig 按 functions.driver 构造执行底座
//     （fail-closed：值未知或 docker 驱动未链接都拒绝启动，不静默回落）。
//
// 驱动共享面（构建上下文 / tar 流 / 验证 spawn / 镜像源 host 准入 / 清理
// 超时 / runner env 组装）分别落在 buildcontext.go / verify.go / imageref.go
// / runnerenv.go，dockerdriver 只消费这些导出面与 Daemon 接口——「fleetly
// 驱动路径零 docker client」的机制断言以该包边界成立（口径见
// import_guard_test.go 与 fleetly 仓实施记录）。

import (
	"fmt"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// DockerDriverFactory 是 docker 执行底座的构造函数形态（dockerdriver 包
// 注册；cfg 携带 functions.driver 与 functions.docker.* 配置）。
type DockerDriverFactory func(cfg *config.AppConfig) (Daemon, error)

// dockerDriverFactory 是已注册的 docker 底座构造函数（nil = 本二进制未
// 链接 docker 驱动）。进程级单值：注册发生在组合根的包 init，先于任何
// 消费；测试可重注册（后注册者生效）。
var dockerDriverFactory DockerDriverFactory

// RegisterDockerDriver 注册 docker 执行底座构造函数（dockerdriver 包 init
// 调用；组合根 blank-import 该包即完成注册——database/sql 驱动注册同款
// 形态）。
func RegisterDockerDriver(factory DockerDriverFactory) {
	dockerDriverFactory = factory
}

// newDaemonForConfig 按 functions.driver 构造执行底座（NewService 的选择
// 点）。driver 值在组合根已经 bootkit.ValidateFunctionsDriverConfig
// fail-closed；此处对「未链接 docker 驱动的二进制被要求 docker 底座」与
// 跳过组合根校验的调用方兜底防御，一律显式失败。
func newDaemonForConfig(cfg *config.AppConfig, registry Registry) (Daemon, error) {
	switch driver := cfg.GetFunctions().GetDriver(); driver {
	case config.FunctionsDriverFleetly:
		return NewFleetlyDaemon(cfg, registry), nil
	case config.FunctionsDriverDocker:
		if dockerDriverFactory == nil {
			return nil, fmt.Errorf("functions.driver is %q but the docker execution driver is not linked into this binary (the composition root must import dispatcher/dockerdriver)", config.FunctionsDriverDocker)
		}
		return dockerDriverFactory(cfg)
	case "":
		return nil, fmt.Errorf("functions.driver is required (set it to %q or %q; env TORCHWOOD_FUNCTIONS_DRIVER)",
			config.FunctionsDriverFleetly, config.FunctionsDriverDocker)
	default:
		return nil, fmt.Errorf("functions.driver %q is unknown (supported values: %q, %q; env TORCHWOOD_FUNCTIONS_DRIVER)",
			driver, config.FunctionsDriverFleetly, config.FunctionsDriverDocker)
	}
}
