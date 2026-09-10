package main

import (
	"context"
	"log"
	"time"

	"github.com/joho/godotenv"
	"github.com/lynx-go/lynx"
	lynxzap "github.com/lynx-go/lynx/contrib/zap"
	"github.com/spf13/pflag"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

var version, commit, date string

func main() {
	_ = godotenv.Load()

	// commit 注入时拼进版本串，便于日志定位构建来源。
	buildVersion := version
	if commit != "" {
		buildVersion = version + " (" + commit + ")"
	}

	var cleanup func()
	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(lynxzap.MustNewLogger(app))

		bootstrap, c, err := wireBootstrap(app)
		if err != nil {
			return err
		}
		cleanup = c
		bootstrap.Bind(app)
		return nil
	},
		lynx.WithName("Torchwood Worker"),
		lynx.WithVersion(buildVersion),
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config-dir", "./configs", "config file path")
			f.String("log-level", "info", "log level")
		}),
		lynx.WithBindConfigFunc(config.NewBindConfigFunc()),
		// Worker 无需排水窗口（无 LB 摘流，仅消费队列与定时任务），仅需有界关停。
		lynx.WithShutdownTimeout(30*time.Second),
	)

	// cleanup（关闭 DB/Redis 等底层资源）不能注册进 OnStop：lynx 在服务
	// GracefulStop 之前执行 OnStop hooks，会先关掉连接池导致排水/关停期间
	// 的在途任务写库失败。这里等 runner.RunE() 返回（所有服务已停止）后再清理，
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
