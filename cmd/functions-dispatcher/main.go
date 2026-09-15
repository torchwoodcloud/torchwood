package main

import (
	"log"
	"time"

	"github.com/joho/godotenv"
	"github.com/lynx-go/lynx"
	lynxzap "github.com/lynx-go/lynx/contrib/zap"
	"github.com/spf13/pflag"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

var version, commit, date string

// functions-dispatcher 是执行器 v2 的独立分发进程（P0.5 方案③，owner 拍板）：
// 专职持有 docker.sock、join 各项目函数网络、承接 Build/Execute/RemoveImage
// 全部 daemon 操作；server/worker 零 daemon 依赖（host-root 等价凭证收敛到
// 非 API 面进程，Q13 联动收口）。
func main() {
	_ = godotenv.Load()

	// commit/date 注入时拼进版本串，便于日志定位构建来源。
	buildVersion := version
	if commit != "" {
		buildVersion = version + " (" + commit + ")"
	}
	if date != "" {
		buildVersion += " built " + date
	}

	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))

		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		// cleanup（关闭 Redis 等底层资源）挂 OnPostStop（lynx v1.10.0，
		// 与 server/worker 同约定）：所有服务停止后执行，自带预算——取代
		// 此前 main 里的手写超时兜底样板。
		app.OnPostStop(cleanup)
		bootstrap.Bind(app)
		return nil
	},
		lynx.WithName("Torchwood Functions Dispatcher"),
		lynx.WithVersion(buildVersion),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		// 内网 API 面（无 LB 摘流需求）：有界关停即可，池内实例由 reaper
		// 幽灵对账兜底清理。
		lynx.WithShutdownTimeout(30*time.Second),
	)

	if err := runner.RunE(); err != nil {
		log.Fatalln(err)
	}
}
