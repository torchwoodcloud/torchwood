package dispatcher

// 本文件是双执行底座（IMPL-T2-5）的驱动集成面，也是 docker 驱动与
// fleetly 驱动路径之间唯一的桥：
//   - 驱动注册：docker 底座实现落隔离子包 dockerdriver（本仓唯一的
//     docker client 持有面），经 RegisterDockerDriver 注入构造函数，由
//     组合根（cmd/dispatcher）blank-import 完成链接；
//   - 驱动选择：newDaemonForConfig 按 functions.driver 构造执行底座
//     （fail-closed：值未知或 docker 驱动未链接都拒绝启动，不静默回落）；
//   - 驱动共享面：构建上下文编排 / build context tar 流 / 验证 spawn /
//     镜像源 host 准入的导出形态——dockerdriver 只消费本文件导出的共享面
//     与 Daemon 接口，「fleetly 驱动路径零 docker client」的机制断言以该
//     包边界成立（口径见 import_guard_test.go 与 fleetly 仓实施记录）。

import (
	"context"
	"fmt"
	"io"
	"time"

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

// ——驱动共享面（dockerdriver 只消费本段导出形态；底层实现在 daemon.go /
// imageref.go，fleetly 驱动同用底层实现，行为零变化）——

// PrepareBuildContext 是 prepareBuildContext 的导出形态：在 buildDir 准备
// 镜像构建上下文（zip 解压校验 → runtime 对账 → 模板渲染；构建期不执行
// 用户代码的不变量在此保持）。
func PrepareBuildContext(buildDir string, opts BuildImageOptions) error {
	return prepareBuildContext(buildDir, opts)
}

// TarDir 是 tarDir 的导出形态：将目录流式打包为 build context tar（失败
// 路径调用方须 Close 读端，唤醒阻塞在 pipe 写侧的打包 goroutine）。
func TarDir(dir string) io.ReadCloser { return tarDir(dir) }

// NewHTTPProber 返回验证 spawn 的生产探针（/_tw/health HTTP 客户端，与池
// 的启动握手同款节拍）。
func NewHTTPProber() HealthProber { return newHTTPRunner() }

// SpawnVerifyInstance 执行一次部署后验证 spawn（池外实例：ensure 网络 →
// spawn → /_tw/health 轮询（预算 = bootTimeout）→ 就绪或失败现场回收；
// image 须为执行底座可直接解析的引用）。两形态驱动的 BuildImage /
// ImportImage 共用同一编排；probe 是验证探针（生产 = NewHTTPProber，测试
// 可注入 fake）；maxRequests 是验证实例 TW_MAX_REQUESTS 注入值。
func SpawnVerifyInstance(ctx context.Context, d Daemon, probe HealthProber, opts BuildImageOptions, image string, bootTimeout time.Duration, maxRequests int) error {
	return spawnVerifyInstance(ctx, d, probe, opts, verifySpawnConfig{
		Image:       image,
		BootTimeout: bootTimeout,
		MaxRequests: maxRequests,
	})
}

// ValidateImageRegistryHost 是 validateImageRegistryHost 的导出形态：
// functions.image 准入规则（白名单优先放行 + allow_insecure 显式放行，
// 拒绝 IP 字面量与 localhost/*.localhost）。
func ValidateImageRegistryHost(host string, allowedRegistries []string, allowInsecure bool) error {
	return validateImageRegistryHost(host, allowedRegistries, allowInsecure)
}

// ParseImageReferenceHost 是 parseImageReferenceHost 的导出形态：解析镜像
// 引用的 registry host 段（无显式 host 归属 docker.io；统一小写含端口）。
func ParseImageReferenceHost(reference string) string {
	return parseImageReferenceHost(reference)
}
