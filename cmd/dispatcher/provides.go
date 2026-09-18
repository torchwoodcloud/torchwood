package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwoodcloud/torchwood/dispatcher"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/bootkit"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

//go:generate wire

// ProviderSet 只装配 dispatcher 所需端口（Redis + 池管理服务）：
// 零 Postgres、零 docker.sock 之外特权（docker.sock 由 daemon 层持有）。
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
// 路由模式校验（四期 4b）与 server/worker 对齐——dispatcher 是
// routing_mode/registry_push 的直接消费方，字符串笔误在此 fail-fast，
// 不得静默降级 local。url 校验（ValidateFunctionsDispatchConfig）不适用
// ——dispatcher 自身是通路终点，不消费该键。
func NewAppConfig(app lynx.App) (*config.AppConfig, error) {
	var c config.AppConfig
	if err := config.UnmarshalConfig(app.Config(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateAppConfig(app.Logger(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateFunctionsRoutingConfig(&c); err != nil {
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
