package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestHTTPErrorHandler_RetryAfterHeader（T-01）：429 且 status 携带
// RetryInfo detail 时，响应必须带 Retry-After 头（整秒向上取整）。
func TestHTTPErrorHandler_RetryAfterHeader(t *testing.T) {
	t.Parallel()

	st := status.New(codes.ResourceExhausted, "too many failed sign-in attempts, try again later")
	enriched, err := st.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(1500 * time.Millisecond),
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/account/sign-in", nil)
	HTTPErrorHandler(t.Context(), nil, NewCustomMarshaler(), rec, req, enriched.Err())

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "2", rec.Header().Get("Retry-After"), "1500ms 必须向上取整为 2s")

	body := rec.Body.String()
	require.Contains(t, body, "rate_limit_error")
	require.Contains(t, body, "too many failed sign-in attempts")
}

// TestHTTPErrorHandler_RetryAfterOmittedWithoutDetail：无 RetryInfo 的 429
// 不设 Retry-After 头（通用限流的兜底 detail 由拦截器层负责补齐）。
func TestHTTPErrorHandler_RetryAfterOmittedWithoutDetail(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/account/sign-in", nil)
	HTTPErrorHandler(t.Context(), nil, NewCustomMarshaler(), rec, req, status.Error(codes.ResourceExhausted, "no detail"))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"))
}
