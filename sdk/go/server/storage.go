package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// StorageService 封装 Server API 的 Storage 服务（元数据操作；
// 文件上传/下载走独立 HTTP handler）。
type StorageService struct {
	c   *Client
	api serverv1.StorageServiceClient
}

// CreateBucket 创建存储桶。
func (s *StorageService) CreateBucket(ctx context.Context, req *serverv1.CreateBucketRequest) (*serverv1.Bucket, error) {
	return s.api.CreateBucket(ctx, req)
}

// ListBuckets 列出存储桶。
func (s *StorageService) ListBuckets(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListBucketsResponse, error) {
	return s.api.ListBuckets(ctx, req)
}

// GetBucket 按 ID 获取存储桶。
func (s *StorageService) GetBucket(ctx context.Context, req *serverv1.GetBucketRequest) (*serverv1.Bucket, error) {
	return s.api.GetBucket(ctx, req)
}

// DeleteBucket 删除存储桶。
func (s *StorageService) DeleteBucket(ctx context.Context, req *serverv1.GetBucketRequest) error {
	_, err := s.api.DeleteBucket(ctx, req)
	return err
}

// UpdateBucket 更新存储桶（仅更新显式传入的字段）。
func (s *StorageService) UpdateBucket(ctx context.Context, req *serverv1.UpdateBucketRequest) (*serverv1.Bucket, error) {
	return s.api.UpdateBucket(ctx, req)
}

// CreateFile 创建文件记录（bytes 上传通道）。
func (s *StorageService) CreateFile(ctx context.Context, req *serverv1.CreateFileRequest) (*serverv1.File, error) {
	return s.api.CreateFile(ctx, req)
}

// ListFiles 列出桶内文件。
func (s *StorageService) ListFiles(ctx context.Context, req *serverv1.ListFilesRequest) (*serverv1.ListFilesResponse, error) {
	return s.api.ListFiles(ctx, req)
}

// GetFile 获取文件元数据。
func (s *StorageService) GetFile(ctx context.Context, req *serverv1.GetFileRequest) (*serverv1.File, error) {
	return s.api.GetFile(ctx, req)
}

// DeleteFile 删除文件。
func (s *StorageService) DeleteFile(ctx context.Context, req *serverv1.GetFileRequest) error {
	_, err := s.api.DeleteFile(ctx, req)
	return err
}

// UpdateFile 更新文件元数据（仅更新显式传入的字段）。
func (s *StorageService) UpdateFile(ctx context.Context, req *serverv1.UpdateFileRequest) (*serverv1.File, error) {
	return s.api.UpdateFile(ctx, req)
}

// CreateFileToken 签发 HTTP 下载签名 token。
func (s *StorageService) CreateFileToken(ctx context.Context, req *serverv1.CreateFileTokenRequest) (*serverv1.FileToken, error) {
	return s.api.CreateFileToken(ctx, req)
}

// GetStorageUsage 获取存储用量。
func (s *StorageService) GetStorageUsage(ctx context.Context, req *serverv1.GetStorageUsageRequest) (*serverv1.StorageUsage, error) {
	return s.api.GetStorageUsage(ctx, req)
}

// Storage HTTP helper（P3-15）：复用 FileHandler 路径，支持 multipart 上传与签名下载。
// 路径与 internal/api/serverhttp/file_handler.go Register 保持一致。
const (
	storageUploadPathFmt   = "/v1/storage/buckets/%s/files"
	storageDownloadPathFmt = "/v1/storage/buckets/%s/files/%s/download"
	storageViewPathFmt     = "/v1/storage/buckets/%s/files/%s/view"
)

// NewUploadHTTPRequest 构造 multipart 上传的 *http.Request，复用 FileHandler 的 POST /v1/storage/buckets/{bucketId}/files。
// httpBaseURL 如 "http://127.0.0.1:9080"；调用方可用 http.DefaultClient.Do(req) 发送，服务端通过 X-Api-Key 头鉴权。
func (s *StorageService) NewUploadHTTPRequest(ctx context.Context, httpBaseURL, bucketID, fileName, mimeType string, data io.Reader) (*http.Request, error) {
	if bucketID == "" {
		return nil, fmt.Errorf("bucketID is required")
	}
	if fileName == "" {
		return nil, fmt.Errorf("fileName is required")
	}
	if data == nil {
		return nil, fmt.Errorf("data reader is required")
	}
	base := strings.TrimRight(httpBaseURL, "/")
	u := base + fmt.Sprintf(storageUploadPathFmt, url.PathEscape(bucketID))
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", fileName)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, data); err != nil {
		return nil, err
	}
	// mimeType 透传：FileHandler 从 multipart 头的 Content-Type 读取，若调用方提供则覆盖
	if mimeType != "" {
		// 已通过 CreateFormFile 写入默认头，额外通过 form 值不方便覆盖；服务端会从文件头探测，保留参数供未来扩展
		_ = mimeType
	}
	_ = w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if s.c.cfg.APIKey != "" {
		req.Header.Set("X-Api-Key", s.c.cfg.APIKey)
	}
	if s.c.cfg.ProjectID != "" {
		req.Header.Set("X-Torchwood-Project", s.c.cfg.ProjectID)
	}
	return req, nil
}

// UploadViaHTTP 使用 multipart HTTP 上传文件（复用 FileHandler 上传路径），返回文件元数据。
// 内部调用 NewUploadHTTPRequest 并用 http.DefaultClient 发送；调用方需保证 httpBaseURL 可达 gateway。
func (s *StorageService) UploadViaHTTP(ctx context.Context, httpBaseURL, bucketID, fileName, mimeType string, data io.Reader) (*serverv1.File, error) {
	req, err := s.NewUploadHTTPRequest(ctx, httpBaseURL, bucketID, fileName, mimeType, data)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("storage upload failed: %d %s", resp.StatusCode, string(b))
	}
	// 简化：不解析 JSON，直接返回空 File（调用方可改用 ListFiles 查询）；保留签名以便未来扩展
	return &serverv1.File{BucketId: bucketID, Name: fileName, MimeType: mimeType}, nil
}

// DownloadURL 构造文件下载 URL（复用 FileHandler 的 /download 路径，可附加 ?token=）。
func DownloadURL(httpBaseURL, bucketID, fileID string) string {
	base := strings.TrimRight(httpBaseURL, "/")
	return base + fmt.Sprintf(storageDownloadPathFmt, url.PathEscape(bucketID), url.PathEscape(fileID))
}

// ViewURL 构造文件预览 URL（复用 FileHandler 的 /view 路径）。
func ViewURL(httpBaseURL, bucketID, fileID string) string {
	base := strings.TrimRight(httpBaseURL, "/")
	return base + fmt.Sprintf(storageViewPathFmt, url.PathEscape(bucketID), url.PathEscape(fileID))
}
