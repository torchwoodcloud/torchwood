package main

import (
	"log"
	"time"

	// 嵌入 IANA 时区库：leaderboard 结算/retention 的期边界派生依赖
	// time.LoadLocation；不依赖宿主 /usr/share/zoneinfo（与 cmd/server 同因）。
	_ "time/tzdata"

	"github.com/joho/godotenv"
	"github.com/lynx-go/lynx"
	lynxzap "github.com/lynx-go/lynx/contrib/zap"
	"github.com/spf13/pflag"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 版本元数据：ldflags 由构建脚本注入 lynx 框架级 buildinfo 包（v1.20.0，
// 唯一注入目标），App 元数据经兜底链自动取值——应用不再显式 WithVersion，
// commit 修饰串由 buildinfo.Display 语义承接。

func main() {
	_ = godotenv.Load()

	runner := lynx.NewRunner(func(app lynx.App) error {
		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		// cleanup（关闭 DB/Redis 等底层资源）挂 OnPostStop（lynx v1.10.0）：
		// 所有服务停止后执行，自带 CleanupTimeout 预算——取代此前 main 里的
		// 手写超时兜底样板（不能进 OnPreStop：排水/关停期间在途任务还要用
		// 连接池）。
		app.OnPostStop(cleanup)
		bootstrap.Apply(app)
		return nil
	},
		lynx.WithName("Torchwood Worker"),
		// 版本不显式传入：lynx v1.20.0 兜底链 service.version 配置 →
		// 编译期 buildinfo.Version（构建脚本 ldflags 注入）自动生效。
		lynx.WithLoggerProvider(lynxzap.NewLogger),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			// 默认值为空（lynx v1.16.0 级别键契约，同 cmd/server）：非空默认
			// 会遮蔽配置文件的 logging.level；未传时回退配置文件，缺省 info。
			f.String("log-level", "", "log level, default info")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		// Worker 无需排水窗口（无 LB 摘流，仅消费队列与定时任务），仅需有界关停。
		lynx.WithShutdownTimeout(30*time.Second),
	)

	if err := runner.RunE(); err != nil {
		log.Fatalln(err)
	}
}
