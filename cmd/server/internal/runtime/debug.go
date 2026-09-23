package runtime

import (
	lynxdebug "github.com/lynx-go/lynx/debug"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/buildinfo"
)

// NewDebugServer 装配 lynx/debug 运维诊断服务（pprof 全端点 + /healthz +
// /version + /loglevel）。仅 server 进程装配；/loglevel 对定制 zap logger
// 返回 409（lynx SetLogLevel 不代理用户 logger），可用面是 pprof 与
// /version。
//
// 地址沿用 metrics 的回环缺省（127.0.0.1:6060）：pprof 暴露内存快照与
// 源码路径，生产只在容器内网命名空间可达（docker exec / SSH 转发诊断），
// 不进 compose ports 映射。
//
// /version 输出 lynx/debug 包级变量（ldflags 注入口），与经
// /v1/server/health/version 暴露的 buildinfo 是两份存储——在此桥接一次
// （ldflags 只需注入 main 包既有变量，构建脚本不必为 debug 包加 -X）。
// 测试构建下 ldflags 变量为空，保持包缺省值（"dev"/"unknown"）。
func NewDebugServer(cfg *config.AppConfig, info buildinfo.BuildInfo) *lynxdebug.Service {
	if info.Version != "" {
		lynxdebug.BuildVersion = info.Version
	}
	if info.Commit != "" {
		lynxdebug.BuildCommit = info.Commit
	}
	if info.Date != "" {
		lynxdebug.BuildDate = info.Date
	}
	addr := cfg.GetServer().GetDebug().GetAddr()
	if addr == "" {
		addr = lynxdebug.DefaultAddr
	}
	return lynxdebug.NewService(lynxdebug.WithAddr(addr))
}
