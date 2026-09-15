package main

import (
	"log"
	"time"

	// 嵌入 IANA 时区库：leaderboard period_tz（如 Asia/Shanghai）的期边界
	// 派生依赖 time.LoadLocation；不依赖宿主 /usr/share/zoneinfo，镜像精简
	// 不致静默回落 UTC（domain Location() 的回落是 fail-open）。
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

	// DrainTimeout 必须在 NewRunner 时确定（lynx 在 newLynx 注册 drainChecker，
	// 早于 YAML 绑定）。development 默认 0（本地 Stop 立刻关）；production
	// 默认 30s（LB 摘流）。显式 TORCHWOOD_SERVER_DRAIN_TIMEOUT 可覆盖。
	drainTimeout := config.CurrentDrainTimeout()

	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))
		app.Logger().Info("runtime environment",
			"env", string(config.CurrentRuntimeEnv()),
			"drain_timeout", drainTimeout.String())

		bootstrap, cleanup, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		// cleanup（关闭 DB/Redis 等底层资源）挂 OnPostStop（lynx v1.10.0）：
		// 所有服务 Stop、总线关停之后、Run 返回前逆序执行，自带
		// CleanupTimeout 预算（默认 10s），覆盖 Run 全部退出路径。此前它
		// 不能进 OnPreStop（先于服务 Stop，会掐断排水/关停期间在途请求的
		// 连接池），只能等 RunE 返回后由 main 手写超时兜底样板。
		app.OnPostStop(cleanup)
		bootstrap.Apply(app)
		return nil
	},
		lynx.WithName("Torchwood"),
		lynx.WithVersion(version),
		lynx.WithBindFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		lynx.WithDrainTimeout(drainTimeout),
		lynx.WithShutdownTimeout(30*time.Second),
	)

	if err := runner.RunE(); err != nil {
		log.Fatalln(err)
	}
}
