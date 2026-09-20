package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// TestCORSMiddleware_ExposeHeaders 钉住 S11 契约修复：config expose_headers
// 此前只声明无实现，现要求实际（非预检）响应输出 Access-Control-Expose-Headers
// （逗号 join），空配置不输出；预检响应不输出。
func TestCORSMiddleware_ExposeHeaders(t *testing.T) {
	noop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("configured: joined on actual response only", func(t *testing.T) {
		h := CORSMiddleware(&config.Http_Cors{
			AllowOrigins:  []string{"https://app.example.com"},
			ExposeHeaders: []string{"X-Request-Id", "X-Total-Count"},
		}, nil)(noop)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		require.Equal(t, "X-Request-Id, X-Total-Count",
			rec.Header().Get("Access-Control-Expose-Headers"))

		// 预检短路（204）：不输出 Expose-Headers。
		preflight := httptest.NewRequest(http.MethodOptions, "/x", nil)
		preflight.Header.Set("Origin", "https://app.example.com")
		recPre := httptest.NewRecorder()
		h.ServeHTTP(recPre, preflight)
		require.Equal(t, http.StatusNoContent, recPre.Code)
		require.Empty(t, recPre.Header().Get("Access-Control-Expose-Headers"))
	})

	t.Run("empty config: header omitted", func(t *testing.T) {
		h := CORSMiddleware(&config.Http_Cors{AllowOrigins: []string{"https://app.example.com"}}, nil)(noop)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		require.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"))
	})
}
