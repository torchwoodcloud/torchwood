package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestObservability_MetricsEndpoint covers manual checklist §10.1.
func TestObservability_MetricsEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "# HELP")
}

// TestObservability_CORS covers manual checklist §10.2.
func TestObservability_CORS(t *testing.T) {
	corsCfg := &config.Http_Cors{
		AllowOrigins:     []string{"http://localhost:5173"},
		AllowMethods:     []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "X-Api-Key", "X-Torchwood-Project"},
		AllowCredentials: true,
		MaxAge:           86400,
	}
	handler := CORSMiddleware(corsCfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/v1/account/me", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, "http://localhost:5173", rec.Header().Get("Access-Control-Allow-Origin"))
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "GET")
	})

	t.Run("actual request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "http://localhost:5173", rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

// TestObservability_StructuredHTTPError covers manual checklist §10.3.
func TestObservability_StructuredHTTPError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/account/sign-up", bytes.NewReader([]byte("{")))
	HTTPErrorHandler(context.Background(), nil, NewCustomMarshaler(), rec, req, status.Error(codes.InvalidArgument, "malformed request body"))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp sharedv1.ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.GetError())
	require.Equal(t, "InvalidArgument", resp.GetError().GetCode())
	require.Equal(t, "malformed request body", resp.GetError().GetMessage())
	require.NotEmpty(t, resp.GetError().GetErrorId())
	require.Equal(t, "invalid_request_error", resp.GetError().GetType())
}
