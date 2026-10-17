package runtime

import (
	lynxdebug "github.com/lynx-go/lynx/debug"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// NewDebugServer 装配 lynx/debug 运维诊断服务（pprof 全端点 + /healthz +
// /version + /loglevel）。仅 server 进程装配；logger 走 WithLoggerProvider
// 定制后（lynx v1.16.0）/loglevel 返回 501（框架不再代理用户 logger 的
// 级别），可用面是 pprof 与 /version。
//
// 地址沿用 metrics 的回环缺省（127.0.0.1:6060）：pprof 暴露内存快照与
// 源码路径，生产只在容器内网命名空间可达（docker exec / SSH 转发诊断），
// 不进 compose ports 映射。
//
// /version 直读 lynx 框架级 buildinfo 包（v1.20.0，ldflags 唯一注入目标，
// 与 /v1/server/health/version 同源同值）——构建脚本注入即生效，无需桥接。
func NewDebugServer(cfg *config.AppConfig) *lynxdebug.Service {
	addr := cfg.GetServer().GetDebug().GetAddr()
	if addr == "" {
		addr = lynxdebug.DefaultAddr
	}
	return lynxdebug.NewService(lynxdebug.WithAddr(addr))
}
