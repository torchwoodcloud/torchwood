package runtime_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/cmd/server/internal/runtime"
	"github.com/torchwoodcloud/torchwood/console"
)

func TestConsoleHandler_SecurityHeaders(t *testing.T) {
	t.Parallel()
	h, err := runtime.NewConsoleHandler()
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	require.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"))
	require.Equal(t,
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:",
		rec.Header().Get("Content-Security-Policy"))
}

func TestConsoleHandler_SPAFallbackAlsoHasHeaders(t *testing.T) {
	t.Parallel()
	h, err := runtime.NewConsoleHandler()
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/console/some/unknown/route", nil)
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.NotEmpty(t, rec.Header().Get("Content-Security-Policy"))
}

// 前端路由恰好落在 Vite chunk 目录的路径前缀下（2026-09-22 事故：这些路由
// 曾被 assets/ 前缀特判误判为静态资源而 404），必须回退 index.html。
func TestConsoleHandler_SPARoutesUnderAssetsPrefix(t *testing.T) {
	t.Parallel()
	h, err := runtime.NewConsoleHandler()
	require.NoError(t, err)

	for _, route := range []string{
		"/console/assets/users",
		"/console/assets/defs/01JZZDEFHERO00000000000000",
		"/console/assets/defs/new",
		"/console/assets",
		"/console/assets/",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		require.Equal(t, http.StatusOK, rec.Code, route)
		require.Contains(t, rec.Header().Get("Content-Type"), "text/html", route)
		require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), route)
	}
}

// P3-10 语义保留：真实 chunk / 文件资源缺失仍 404，不回退 index.html
// （避免 JS 请求拿到 HTML 触发 MIME 错误）。依赖 console/dist 已构建。
func TestConsoleHandler_MissingStaticAssetStill404(t *testing.T) {
	t.Parallel()
	if _, err := console.Dist.ReadFile("dist/index.html"); err != nil {
		t.Skip("console/dist not built")
	}
	h, err := runtime.NewConsoleHandler()
	require.NoError(t, err)

	for _, target := range []string{
		"/console/assets/no-such-chunk-xyz.js",
		"/console/no-such-file.json",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, target)
	}
}

// chunk 与根级静态文件正常 serve，且命中各自缓存策略。
func TestConsoleHandler_ServesEmbeddedFiles(t *testing.T) {
	t.Parallel()
	if _, err := console.Dist.ReadFile("dist/index.html"); err != nil {
		t.Skip("console/dist not built")
	}
	h, err := runtime.NewConsoleHandler()
	require.NoError(t, err)

	entries, err := fs.ReadDir(console.Dist, "dist/assets")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	chunk := "assets/" + entries[0].Name()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/"+chunk, nil))
	require.Equal(t, http.StatusOK, rec.Code, chunk)
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"), chunk)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/console/", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
}
