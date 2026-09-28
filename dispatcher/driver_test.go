package dispatcher

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件是 IMPL-T2-5 双执行底座的驱动选择守卫：functions.driver 决定执行
// 底座，选择路径 fail-closed（未设/未知/docker 驱动未链接都拒绝启动），
// fleetly 形态选择行为零变化。

func driverTestConfig(driver string) *config.AppConfig {
	return &config.AppConfig{Functions: &config.Functions{
		Driver: driver,
		Dispatcher: &config.Functions_Dispatcher{
			BootTimeout: "1s",
			QueueDepth:  2,
		},
		Fleetly: &config.Functions_Fleetly{Endpoint: "127.0.0.1:1", Token: "tok"},
	}}
}

// preserveDockerDriverFactory 保存/恢复进程级驱动注册单值（注册发生在被测
// 调用前、恢复挂在 t.Cleanup；返回值可把测试期间置回未注册形态）。
func preserveDockerDriverFactory(t *testing.T) func() {
	t.Helper()
	prev := dockerDriverFactory
	t.Cleanup(func() { dockerDriverFactory = prev })
	return func() { dockerDriverFactory = nil }
}

// TestNewDaemonForConfig_FleetlyUnchanged driver=fleetly 走既有 fleetly
// 实现（行为零变化；boot 预算与池同源解析）。
func TestNewDaemonForConfig_FleetlyUnchanged(t *testing.T) {
	t.Parallel()
	cfg := driverTestConfig("fleetly")
	d, err := newDaemonForConfig(cfg, nil)
	require.NoError(t, err)
	fd, ok := d.(*fleetlyDaemon)
	require.True(t, ok, "driver=fleetly must select the fleetly daemon")
	require.Equal(t, PoolConfigFromConfig(cfg).BootTimeout, fd.bootTimeout)
}

// TestNewDaemonForConfig_DockerDriverNotLinked driver=docker 但本二进制未
// 链接 docker 驱动（无注册）→ 显式失败点名链接要求，不静默回落。
// 不并行：与注册类用例共享进程级驱动注册单值（写者互斥）。
func TestNewDaemonForConfig_DockerDriverNotLinked(t *testing.T) {
	preserveDockerDriverFactory(t)() // 测试期间保持未注册形态
	_, err := newDaemonForConfig(driverTestConfig("docker"), nil)
	require.ErrorContains(t, err, "docker execution driver is not linked")
	require.ErrorContains(t, err, "dispatcher/dockerdriver")
}

// TestNewDaemonForConfig_DriverSelectionFailClosed 未设/未知 driver 在选择
// 点兜底拒绝（组合根 bootkit 校验之外的防御线）。
func TestNewDaemonForConfig_DriverSelectionFailClosed(t *testing.T) {
	t.Parallel()
	_, err := newDaemonForConfig(driverTestConfig(""), nil)
	require.ErrorContains(t, err, "functions.driver is required")
	_, err = newDaemonForConfig(driverTestConfig("nomad"), nil)
	require.ErrorContains(t, err, `functions.driver "nomad" is unknown`)
}

// TestNewDaemonForConfig_DockerDriverRegistered driver=docker 且驱动已注册
// → 构造函数被调用并返回其产物（生产注册由 dockerdriver 包 init 承载，
// 此处注入 fake 验证注册协议；fakeDaemon 是 pool_test.go 的 Daemon fake）。
// 不并行：进程级驱动注册单值的写者互斥。
func TestNewDaemonForConfig_DockerDriverRegistered(t *testing.T) {
	preserveDockerDriverFactory(t)
	called := false
	RegisterDockerDriver(func(*config.AppConfig) (Daemon, error) {
		called = true
		return newFakeDaemon(), nil
	})
	d, err := newDaemonForConfig(driverTestConfig("docker"), nil)
	require.NoError(t, err)
	require.True(t, called, "registered docker driver factory must be invoked")
	require.IsType(t, newFakeDaemon(), d)
}

// TestNewService_DriverFailClosed NewService 在驱动选择失败时拒绝启动
// （组合根 wire 形态的启动期收口）。不并行：进程级驱动注册单值的写者互斥。
func TestNewService_DriverFailClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	preserveDockerDriverFactory(t)()
	_, err := NewService(driverTestConfig(""), rdb, nil)
	require.ErrorContains(t, err, "functions.driver is required")
}
