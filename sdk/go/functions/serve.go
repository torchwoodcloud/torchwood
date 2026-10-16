package functions

// 常驻服务运行语义（v4 统一入口的承载面）：Listen 阻塞服务 + SIGTERM/SIGINT
// 优雅排水。旧 runner 契约循环（POST / 的 TW_DATA 明文分发、TW_MAX_REQUESTS
// 自退出、x-tw-timeout-seconds per-request 超时）已随执行面外包退役——分发
// 只走 NewServer 的双轴协议面；TW_RUNNER_PORT 语义改为「常驻服务监听端口」。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// 运行语义常量。TW_RUNNER_PORT 沿用 v3 名字（部署面平滑），语义 = 常驻服务
// 监听端口；TW_API_BASE_URL 由部署侧注入（fleetly app env / compose，D9 一致）。
const (
	defaultPort         = 18080            // TW_RUNNER_PORT 缺省值
	defaultDrainTimeout = 10 * time.Second // TW_DRAIN_TIMEOUT_MS 缺省值
	healthPath          = "/_tw/health"    // 可选探针（部署平台用，非平台强制）
	envAPIBaseURL       = "TW_API_BASE_URL"
	envPort             = "TW_RUNNER_PORT"
	envDrainTimeoutMS   = "TW_DRAIN_TIMEOUT_MS"
)

// healthEnvelope 是探针响应（NewServer 编译期注册，进程活着即就绪；不走验
// 签——部署平台探活不持签名密钥）。
type healthEnvelope struct {
	OK    bool `json:"ok"`
	Ready bool `json:"ready"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{"ok":false}`)
		status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// listenPort 解析 TW_RUNNER_PORT（缺省 18080；非法/负值回落缺省，0 = 随机
// 端口合法）。
func listenPort() int {
	if v := os.Getenv(envPort); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p >= 0 {
			return p
		}
	}
	return defaultPort
}

// drainTimeout 解析 TW_DRAIN_TIMEOUT_MS（缺省 10000）。
func drainTimeout() time.Duration {
	if v := os.Getenv(envDrainTimeoutMS); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return defaultDrainTimeout
}

// serveOpts 是 serve 的测试注入口（生产 Listen 全部零值）。
type serveOpts struct {
	exit   func(int)        // 缺省 os.Exit
	addrCh chan<- string    // 监听地址就绪通知（测试）
	sig    <-chan os.Signal // 信号源注入（nil = 安装真实 SIGTERM/SIGINT）
}

// Listen 以 v4 统一入口启动常驻服务并阻塞（语义同 http.ListenAndServe）：
// 构建 NewServer(mux, secrets...)（验签必经），绑定 TW_RUNNER_PORT（缺省
// 18080，bind 0.0.0.0），SIGTERM/SIGINT → 优雅排水（停止接新 + 等在途完成
// + TW_DRAIN_TIMEOUT_MS 兜底强退，进程退出，正常不返回）。绑定失败返回
// error。多触发源组合经 Listen(mux, secrets...)；单源便捷封装见 Start*。
func Listen(mux *Mux, secrets ...string) error {
	return serve(NewServer(mux, secrets...), serveOpts{})
}

func serve(h http.Handler, opts serveOpts) error {
	if opts.exit == nil {
		opts.exit = os.Exit
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("0.0.0.0:%d", listenPort()))
	if err != nil {
		return err
	}
	if opts.addrCh != nil {
		opts.addrCh <- ln.Addr().String()
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	sig := opts.sig
	if sig == nil {
		s := make(chan os.Signal, 1)
		signal.Notify(s, syscall.SIGTERM, os.Interrupt)
		defer signal.Stop(s)
		sig = s
	}
	// drained：Serve 结束后放行信号 goroutine（测试注入形态下进程不退出，
	// 需让 goroutine 可收尾，避免泄漏阻塞测试二进制退出）。
	drained := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-drained:
			return
		}
		// 优雅排水：Shutdown 关 listener（新连接拒绝，LB 摘实例信号）+ 等
		// 在途完成；到点兜底强退。
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout())
		defer cancel()
		_ = srv.Shutdown(ctx)
		opts.exit(0)
	}()

	err = srv.Serve(ln)
	close(drained)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
