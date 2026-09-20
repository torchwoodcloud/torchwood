package serverhttp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rejectValidator 恒定拒绝认证：闸放行后的请求应到达认证层并以 401 收场，
// 以此区分「闸拦截（429）」与「闸放行」两种结果。
type rejectValidator struct{}

func (rejectValidator) Authenticate(context.Context, shared.AuthnRequest) (*shared.Principal, error) {
	return nil, status.Error(codes.Unauthenticated, "no credentials")
}

func (rejectValidator) ValidateAdminProjectAccess(context.Context, *shared.Principal) error {
	return nil
}

func newGateTestHandler(t *testing.T, max int) *FileHandler {
	t.Helper()
	trusted, err := interceptor.ParseTrustedProxies(nil)
	require.NoError(t, err)
	return &FileHandler{
		auth:        newHTTPAuth(rejectValidator{}, testPolicies()),
		trusted:     trusted,
		logger:      slog.Default(),
		previewGate: semaphore.NewInMemory(max),
	}
}

func callPreview(h *FileHandler) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/storage/buckets/b1/files/f1/preview?width=10", nil)
	rec := httptest.NewRecorder()
	h.preview(rec, r, map[string]string{"bucketId": "b1", "fileId": "f1"})
	return rec
}

// P2 修复第 4 项（preview 无并发闸）：解码槽位满载时 preview 必须快速 429
// （ResourceExhausted），不做任何鉴权/读流工作。
func TestPreview_ConcurrencyGate_RejectsWhenSaturated(t *testing.T) {
	h := newGateTestHandler(t, 1)

	// 占满唯一解码槽位。
	acquired, release, err := h.previewGate.TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	defer release()

	rec := callPreview(h)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, codes.ResourceExhausted.String(), payload.Error.Type)
}

// 闸未满时请求正常通过闸进入后续处理（此处为认证层 401），且 defer 释放后
// 槽位立即可复用（不泄漏）。
func TestPreview_ConcurrencyGate_PassesWhenFree(t *testing.T) {
	h := newGateTestHandler(t, 1)

	rec := callPreview(h)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "闸放行后应到达认证层而非 429")

	// defer release 已归还槽位：可立即重新获取。
	acquired, release, err := h.previewGate.TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired, "请求结束后闸槽位必须已释放")
	release()
}
