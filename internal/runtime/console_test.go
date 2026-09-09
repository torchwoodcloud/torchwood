package runtime_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/runtime"
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
