package main

import (
	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
	"github.com/torchwoodcloud/torchwood/internal/pkg/bootkit"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/packer"
)

//go:generate wire

// ProviderSet 只装配 packer 所需组件：零 Redis、零 Postgres、零 docker.sock
// ——packer 无状态可牺牲（重启即恢复），OOM 只影响 git 部署自身
// （设计 §2 资源画像与隔离声明）。
var ProviderSet = wire.NewSet(
	boot.New,
	bootkit.NewLogger,
	bootkit.NewComponentBuilders,
	bootkit.NewDrains,
	bootkit.NewPreStops,
	bootkit.NewPostStops,
	NewAppConfig,
	NewPreStarts,
	packer.NewService,
	NewComponents,
)

// NewAppConfig 解析并校验 AppConfig（与 server/worker/dispatcher 同一
// 安全校验口径；functions.dispatcher.url 等业务必填项不经
// ValidateFunctionsDispatchConfig——那是 server/worker 组合根专属，
// packer 是通路旁路、不消费该键）。
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

// NewPreStarts 返回空启动前钩子集：packer 不碰项目 schema（worker 同款边界）。
func NewPreStarts() boot.PreStartHooks { return boot.PreStartHooks{} }

// NewComponents 注册 packer 服务（单服务进程）。
func NewComponents(svc *packer.Service) []lynx.Service {
	return []lynx.Service{svc}
}
