package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"github.com/stretchr/testify/require"
)

// TestHealthz_Readiness 用真实监听验证 lynxhttp 的 /healthz/readiness：
// checkers 全 ok → 200；任一失败 → 503。gRPC gateway 与 lynx 的 HTTP
// readiness 走同一 WithHealthCheckers 机制。
func TestHealthz_Readiness(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())

	ok := func() error { return nil }
	fail := func() error { return errors.New("db down") }

	start := func(t *testing.T, checkers lynx.HealthCheckersFunc) string {
		t.Helper()
		srv := lynxhttp.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
			lynxhttp.WithAddr(addr),
			lynxhttp.WithHealthCheckers(checkers),
		)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = srv.Start(ctx) }()
		t.Cleanup(func() {
			cancel()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			_ = srv.Stop(stopCtx)
		})
		return addr
	}

	// probe 发起 GET 并排空 body；网络未就绪时返回错误而非断言失败
	// （require.Eventually 的条件函数中禁止 require.*——FailNow 会终止轮询
	// 协程，导致首连失败后不再重试）。
	probe := func(url string) (int, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	waitReady := func(t *testing.T, base string) {
		t.Helper()
		require.Eventually(t, func() bool {
			code, err := probe(base + "/healthz/liveness")
			return err == nil && code == http.StatusOK
		}, 5*time.Second, 20*time.Millisecond, "server not ready")
	}

	getStatus := func(t *testing.T, url string) int {
		t.Helper()
		code, err := probe(url)
		require.NoError(t, err)
		return code
	}

	t.Run("all healthy -> 200", func(t *testing.T) {
		start(t, func() []lynx.Checker { return []lynx.Checker{okChecker{ok}} })
		waitReady(t, "http://"+addr)

		require.Equal(t, http.StatusOK, getStatus(t, "http://"+addr+"/healthz/readiness"))
		require.Equal(t, http.StatusOK, getStatus(t, "http://"+addr+"/healthz/liveness"))
	})

	t.Run("one failed -> 503", func(t *testing.T) {
		start(t, func() []lynx.Checker { return []lynx.Checker{okChecker{fail}} })
		waitReady(t, "http://"+addr)

		require.Equal(t, http.StatusServiceUnavailable, getStatus(t, "http://"+addr+"/healthz/readiness"))
		require.Equal(t, http.StatusOK, getStatus(t, "http://"+addr+"/healthz/liveness"))
	})
}

type okChecker struct {
	fn func() error
}

func (c okChecker) CheckHealth() error { return c.fn() }

var _ lynx.Checker = okChecker{}
