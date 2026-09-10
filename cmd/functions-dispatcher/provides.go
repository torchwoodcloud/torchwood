package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwoodcloud/torchwood/pkg/bootkit"
	"github.com/torchwoodcloud/torchwood/cmd/functions-dispatcher/internal/functionsdispatcher"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	config "github.com/torchwoodcloud/torchwood/pkg/config"
)

//go:generate wire

// ProviderSet 只装配 dispatcher 所需端口（Redis + 池管理服务）：
// 零 Postgres、零 docker.sock 之外特权（docker.sock 由 daemon 层持有）。
var ProviderSet = wire.NewSet(
	boot.New,
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewOnStops,
	NewAppConfig,
	NewOnStarts,
	clients.NewRedisClient,
	functionsdispatcher.NewService,
	NewComponents,
)

// NewAppConfig 解析并校验 AppConfig（与 server/worker 同一安全校验口径）。
func NewAppConfig(app lynx.App) (*config.AppConfig, error) {
	var c config.AppConfig
	if err := config.UnmarshalConfig(app.Config(), &c); err != nil {
		return nil, err
	}
	if err := bootkit.ValidateAppConfig(app.Logger(), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// NewOnStarts 返回空钩子集：dispatcher 不碰项目 schema（worker 同款边界）。
func NewOnStarts() boot.OnStartHooks { return boot.OnStartHooks{} }

// NewComponents 注册 dispatcher 服务（单服务进程）。
func NewComponents(svc *functionsdispatcher.Service) []lynx.Service {
	return []lynx.Service{svc}
}
