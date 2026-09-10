package main

import (
	"context"
	"log"
	"time"

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

	var cleanup func()
	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))
		app.Logger().Info("runtime environment",
			"env", string(config.CurrentRuntimeEnv()),
			"drain_timeout", drainTimeout.String())

		bootstrap, c, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		cleanup = c
		bootstrap.Bind(app)
		return nil
	},
		lynx.WithName("Torchwood"),
		lynx.WithVersion(version),
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		lynx.WithDrainTimeout(drainTimeout),
		lynx.WithShutdownTimeout(30*time.Second),
	)

	// cleanup（关闭 DB/Redis 等底层资源）不能注册进 OnStop：lynx 在服务
	// GracefulStop 之前执行 OnStop hooks，会先关掉连接池导致排水/关停期间
	// 的在途请求失败。这里等 runner.RunE() 返回（所有服务已停止）后再清理，
	// 并给 cleanup 单独的上限（10s）：任何 Close 挂起都不阻塞进程退出。
	err := runner.RunE()
	if cleanup != nil {
		log.Println("running resource cleanup")
		start := time.Now()
		done := make(chan struct{})
		go func() {
			cleanup()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		select {
		case <-done:
		case <-ctx.Done():
			log.Println("resource cleanup timed out after 10s")
		}
		log.Printf("resource cleanup finished in %s", time.Since(start).Round(time.Millisecond))
	}
	if err != nil {
		log.Fatalln(err)
	}
}
