package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	// docker 执行底座（IMPL-T2-5 双执行底座）：blank-import 完成驱动注册
	//（dispatcher.RegisterDockerDriver）——本二进制按 functions.driver 在
	// fleetly / docker 两执行底座间选择；docker client 只经该子包持有。
	_ "github.com/torchwoodcloud/torchwood/dispatcher/dockerdriver"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/bootkit"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

//go:generate wire

// ProviderSet 只装配 dispatcher 所需端口（Redis + 池管理服务）：
// 零 Postgres、零执行底座特权（函数实例/构建按 functions.driver 经 fleetly
// Tasks/build API 或本机 docker.sock 承接）。
var ProviderSet = wire.NewSet(
	boot.New,
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewDrains,
	bootkit.NewPreStops,
	bootkit.NewPostStops,
	NewAppConfig,
	NewPreStarts,
	clients.NewRedisClient,
	dispatcher.NewService,
	NewComponents,
)

// NewAppConfig 解析并校验 AppConfig（与 server/worker 同一安全校验口径）。
// 执行底座驱动选择（functions.driver）在此 fail-fast（IMPL-T2-5：按驱动
// 分发——fleetly 要求 endpoint/机具令牌，docker 无必填键）；url 校验
// （ValidateFunctionsDispatchConfig）不适用——dispatcher 自身是通路终点。
func NewAppConfig(app lynx.App) (*config.AppConfig, error) {
	var c config.AppConfig
	if err := config.UnmarshalConfig(app.Config(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateAppConfig(app.Logger(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateFunctionsDriverConfig(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// NewPreStarts 返回空启动前钩子集：dispatcher 不碰项目 schema（worker 同款边界）。
func NewPreStarts() boot.PreStartHooks { return boot.PreStartHooks{} }

// NewComponents 注册 dispatcher 服务（单服务进程）。
func NewComponents(svc *dispatcher.Service) []lynx.Service {
	return []lynx.Service{svc}
}
