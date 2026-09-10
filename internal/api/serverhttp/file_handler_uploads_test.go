package serverhttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// doJSON 发送 JSON 请求并解析响应（仅 2xx 时解析 payload）。
func (f *storageHTTPFixture) doJSON(method, path string, body any, headers map[string]string) (int, map[string]any) {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(f.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.server.URL+path, &buf)
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	payload := map[string]any{}
	if resp.StatusCode < 300 {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&payload))
	}
	return resp.StatusCode, payload
}

// uploadChunkViaHTTP 以 multipart 字段 `chunk` 上传一个分片；
// 返回 (HTTP status, received_count)（非 2xx 时 received_count 为 0）。
func (f *storageHTTPFixture) uploadChunkViaHTTP(bucketID, uploadID string, partNumber int, content []byte, headers map[string]string) (int, int) {
	f.t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("chunk", "chunk")
	require.NoError(f.t, err)
	_, err = part.Write(content)
	require.NoError(f.t, err)
	require.NoError(f.t, writer.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/v1/storage/buckets/%s/uploads/%s/chunks/%d", f.server.URL, bucketID, uploadID, partNumber), body)
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		ReceivedCount int `json:"received_count"`
	}
	if resp.StatusCode < 300 {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&payload))
	}
	return resp.StatusCode, payload.ReceivedCount
}

// TestFileHandler_ChunkedUploadFullFlow 分片全流程：create → 2 片（16MiB+8MiB）
// → get（received 正确）→ complete → download 内容一致 + 文档 mime 归一化。
func TestFileHandler_ChunkedUploadFullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)
	headers := map[string]string{"X-Api-Key": f.apiSecret}

	content := make([]byte, 24<<20)
	_, err := rand.Read(content)
	require.NoError(t, err)

	status, created := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name":      "chunked.bin",
		"mime_type": "text/html; charset=utf-8", // 危险 mime → complete 后归一化
		"size":      len(content),
	}, headers)
	require.Equal(t, http.StatusCreated, status)
	uploadID, _ := created["upload_id"].(string)
	fileID, _ := created["file_id"].(string)
	require.NotEmpty(t, uploadID)
	require.NotEmpty(t, fileID)
	require.EqualValues(t, 16<<20, created["chunk_size"])
	require.EqualValues(t, 2, created["part_count"])

	// 第 1 片 16MiB + 第 2 片 8MiB；received_count 为准确值（原子 SCARD）。
	status, received := f.uploadChunkViaHTTP(f.bucketID, uploadID, 1, content[:16<<20], headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, received)
	status, received = f.uploadChunkViaHTTP(f.bucketID, uploadID, 2, content[16<<20:], headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 2, received)

	// getUpload：received 正确。
	status, got := f.doJSON(http.MethodGet, "/v1/storage/buckets/"+f.bucketID+"/uploads/"+uploadID, nil, headers)
	require.Equal(t, http.StatusOK, status)
	require.EqualValues(t, 2, got["part_count"])
	receivedList, _ := got["received"].([]any)
	require.Equal(t, []any{float64(1), float64(2)}, receivedList)

	// complete → 文件 JSON。
	status, file := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads/"+uploadID+"/complete", nil, headers)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, fileID, file["id"])
	require.Equal(t, "application/octet-stream", file["mime_type"])
	require.EqualValues(t, len(content), file["size"])

	// download 内容一致。
	code, gotContent, _ := f.download("/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/download", headers)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, content, gotContent)
}

// TestFileHandler_ChunkedUploadAuthScopes：
// 无 storage scope key → create 403；只读（storage.read）key → getUpload 200、
// uploadChunk 403（GET 分支要求 read，写路径要求 write）。
func TestFileHandler_ChunkedUploadAuthScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)

	// 无 storage scope 的 key：create → 403。
	noScopeSecret, noScopeCleanup := testutil.CreateTestAPIKey(context.Background(), f.db, f.projectID, []string{"users"})
	defer noScopeCleanup()
	status, _ := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name": "x.bin", "size": 1024,
	}, map[string]string{"X-Api-Key": noScopeSecret})
	require.Equal(t, http.StatusForbidden, status)

	// 只读 scope（storage.read）的 key。
	readSecret, readCleanup := testutil.CreateTestAPIKey(context.Background(), f.db, f.projectID, []string{"storage.read"})
	defer readCleanup()
	readHeaders := map[string]string{"X-Api-Key": readSecret}

	// 用全权限 key 建会话并传 1 片。
	status, created := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name": "r.bin", "size": 1024,
	}, map[string]string{"X-Api-Key": f.apiSecret})
	require.Equal(t, http.StatusCreated, status)
	uploadID, _ := created["upload_id"].(string)

	// 只读 key：getUpload 200（GET 分支 storage.read），uploadChunk 403。
	status, _ = f.doJSON(http.MethodGet, "/v1/storage/buckets/"+f.bucketID+"/uploads/"+uploadID, nil, readHeaders)
	require.Equal(t, http.StatusOK, status)
	status, _ = f.uploadChunkViaHTTP(f.bucketID, uploadID, 1, make([]byte, 1024), readHeaders)
	require.Equal(t, http.StatusForbidden, status)
}

// TestFileHandler_ChunkedUploadValidation：
// 超大 chunk 拒绝；缺失分片 complete → 400；无凭证 → 401。
func TestFileHandler_ChunkedUploadValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)
	headers := map[string]string{"X-Api-Key": f.apiSecret}

	status, created := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name": "v.bin",
		"size": 24 << 20,
	}, headers)
	require.Equal(t, http.StatusCreated, status)
	uploadID, _ := created["upload_id"].(string)

	// 超大 chunk（20MiB > 16MiB 整片 + 缓冲）→ 400。
	status, _ = f.uploadChunkViaHTTP(f.bucketID, uploadID, 1, make([]byte, 20<<20), headers)
	require.Equal(t, http.StatusBadRequest, status)

	// 传第 1 片后 complete：缺第 2 片 → 400（FailedPrecondition 经 gateway 映射为 400）。
	status, _ = f.uploadChunkViaHTTP(f.bucketID, uploadID, 1, make([]byte, 16<<20), headers)
	require.Equal(t, http.StatusOK, status)
	status, _ = f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads/"+uploadID+"/complete", nil, headers)
	require.Equal(t, http.StatusBadRequest, status)

	// 无凭证 → 401。
	status, _ = f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name": "anon.bin", "size": 1024,
	}, nil)
	require.Equal(t, http.StatusUnauthorized, status)
}
