package runtime

import (
	"net/http"
	"time"

	lynxhttp "github.com/lynx-go/lynx/server/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

type MetricsServer struct {
	*lynxhttp.Server
}

func NewMetricsServer(cfg *config.AppConfig) (*MetricsServer, error) {
	addr := cfg.GetServer().GetMetrics().GetAddr()
	if addr == "" {
		// 默认仅监听回环地址：metrics 端点无鉴权，暴露到所有接口会被内网
		// 任意主机抓取；生产可配置反向代理 + 网络策略后改为非回环地址。
		addr = "127.0.0.1:9040"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := lynxhttp.NewServer(mux, lynxhttp.WithAddr(addr), lynxhttp.WithTimeout(30*time.Second))
	return &MetricsServer{srv}, nil
}
