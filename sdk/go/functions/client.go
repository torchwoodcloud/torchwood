package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是平台 API 客户端雏形（函数内回访平台用）：鉴权自动——NewClient
// 经 ctx 执行身份取 executionToken（Authorization: Bearer twx_…）与
// apiBaseUrl（TW_API_BASE_URL），零配置。
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewClient 从 ctx 执行身份构建平台 API 客户端。ctx 无身份（如脱离 Start
// 系列的裸调用）时 base/token 为空，调用将以本地错误收场。
func NewClient(ctx context.Context) *Client {
	id := FromContext(ctx)
	return &Client{
		baseURL: strings.TrimRight(id.APIBaseURL, "/"),
		token:   id.ExecutionToken,
		hc:      &http.Client{},
	}
}

// APIError 是平台 API 非 2xx 响应。
type APIError struct {
	// StatusCode 是 HTTP 状态码；Status 是完整状态行（如 "403 Forbidden"）；
	// Body 是响应体原文（≤1MB 截取）。
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("functions: platform api %s: %s", e.Status, e.Body)
}

// Do 调用平台 API：method 为 HTTP 方法；path 形如 /v1/server/assets/defs；
// body 非 nil 时 marshal 为 JSON 请求体；out 非 nil 时把 2xx 响应解码进去。
// 非 2xx 返回 *APIError。
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("functions: encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("functions: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("functions: platform api call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("functions: read platform api response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("functions: decode platform api response: %w", err)
		}
	}
	return nil
}

// DocumentRecord 是平台 API 的文档投影（REST Document；protojson 形状——
// version 为 int64 的字符串化 JSON，解码兼容字符串/数字双形态）。
type DocumentRecord struct {
	ID          string         `json:"id"`
	Data        map[string]any `json:"data,omitempty"`
	Permissions []string       `json:"permissions,omitempty"`
	Version     int64          `json:"version,omitempty"`
	CreatedAt   time.Time      `json:"createdAt,omitempty"`
	UpdatedAt   time.Time      `json:"updatedAt,omitempty"`
}

// UnmarshalJSON 兼容 protojson（version 字符串化）与 encoding/json（数字）
// 两种形态。
func (d *DocumentRecord) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID          string         `json:"id"`
		Data        map[string]any `json:"data"`
		Permissions []string       `json:"permissions"`
		Version     json.Number    `json:"version"`
		CreatedAt   time.Time      `json:"createdAt"`
		UpdatedAt   time.Time      `json:"updatedAt"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	d.ID = raw.ID
	d.Data = raw.Data
	d.Permissions = raw.Permissions
	if raw.Version != "" {
		v, err := raw.Version.Int64()
		if err != nil {
			return fmt.Errorf("functions: invalid document version %q: %w", raw.Version, err)
		}
		d.Version = v
	}
	d.CreatedAt = raw.CreatedAt
	d.UpdatedAt = raw.UpdatedAt
	return nil
}

// CreateDocument 创建文档（REST：POST /v1/server/databases/{db}/collections/
// {coll}/documents），返回新文档 ID。种子方法示例——更多平台面按 Client.Do
// 自行扩展。
func (c *Client) CreateDocument(ctx context.Context, databaseID, collectionID string, data map[string]any) (string, error) {
	var rec DocumentRecord
	body := struct {
		Data map[string]any `json:"data"`
	}{Data: data}
	path := fmt.Sprintf("/v1/server/databases/%s/collections/%s/documents",
		url.PathEscape(databaseID), url.PathEscape(collectionID))
	if err := c.Do(ctx, http.MethodPost, path, body, &rec); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// GetDocument 读取文档（REST：GET /v1/server/databases/{db}/collections/
// {coll}/documents/{id}）；不存在时返回 *APIError（StatusCode 404）。事件
// 触发的函数按 DocumentChange 回读全量的推荐路径。
func (c *Client) GetDocument(ctx context.Context, databaseID, collectionID, documentID string) (*DocumentRecord, error) {
	var rec DocumentRecord
	path := fmt.Sprintf("/v1/server/databases/%s/collections/%s/documents/%s",
		url.PathEscape(databaseID), url.PathEscape(collectionID), url.PathEscape(documentID))
	if err := c.Do(ctx, http.MethodGet, path, nil, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
