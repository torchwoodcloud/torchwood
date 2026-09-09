package projectschema

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// EnsureTimeout 是启动自愈的软超时：超时项目记失败指标，不卡死进程。
const EnsureTimeout = 30 * time.Second

var ensureFailures = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "torchwood_project_schema_ensure_failures_total",
	Help: "Project schema EnsureAll failures (apply error, dirty, or timeout).",
})

func init() {
	prometheus.MustRegister(ensureFailures)
}

// KickoffEnsureAll 在后台对列出的项目 Apply（并发上限 4，见 EnsureAll）。
// 调用方立即返回；失败只打日志与指标，不阻塞 listen / health。
func KickoffEnsureAll(db *clients.Database, projectIDs []string, logger *slog.Logger) {
	if db == nil || len(projectIDs) == 0 {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	ids := append([]string(nil), projectIDs...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), EnsureTimeout)
		defer cancel()
		if err := EnsureAll(ctx, db, ids); err != nil {
			ensureFailures.Inc()
			logger.Error("project schema ensure", "error", err, "projects", len(ids))
		}
	}()
}
