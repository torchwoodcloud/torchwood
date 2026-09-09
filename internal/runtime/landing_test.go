package runtime_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/runtime"
)

// / 站点首页：此前落到 grpc-gateway mux 无路由返回 404，现应返回 landing 页。
func TestLandingHandler_ServesRootPage(t *testing.T) {
	t.Parallel()
	h := runtime.NewLandingHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	require.Contains(t, body, "Torchwood")
	require.Contains(t, body, `href="/console/"`)
	// 自包含页面：品牌资源内联，不依赖外部请求。
	require.Contains(t, body, "<svg")
	require.NotContains(t, body, "<script")
}

func TestLandingHandler_SecurityHeaders(t *testing.T) {
	t.Parallel()
	h := runtime.NewLandingHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	require.Equal(t,
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:",
		rec.Header().Get("Content-Security-Policy"))
}

func TestLandingHandler_Methods(t *testing.T) {
	t.Parallel()
	h := runtime.NewLandingHandler()

	// HEAD：与 GET 同头，无 body（net/http 对 HEAD 也会丢弃 body，这里显式不写）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String())

	// 写方法：405 + Allow，不吞掉语义。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
	require.True(t, strings.HasPrefix(rec.Body.String(), "method not allowed"))
}
