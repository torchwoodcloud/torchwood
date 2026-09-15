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

var version, commit, date string

func main() {
	_ = godotenv.Load()

	// commit 注入时拼进版本串，便于日志定位构建来源。
	buildVersion := version
	if commit != "" {
		buildVersion = version + " (" + commit + ")"
	}

	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))

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
		lynx.WithVersion(buildVersion),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		// Worker 无需排水窗口（无 LB 摘流，仅消费队列与定时任务），仅需有界关停。
		lynx.WithShutdownTimeout(30*time.Second),
	)

	if err := runner.RunE(); err != nil {
		log.Fatalln(err)
	}
}
