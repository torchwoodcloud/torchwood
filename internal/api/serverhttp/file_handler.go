package serverhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	appstorage "github.com/torchwooddev/torchwood/internal/app/storage"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inlineSafeMimeTypes 是可安全同源内联展示的 MIME 白名单，不含任何可执行脚本类型。
// image/svg+xml 可内嵌脚本，虽列入名单，但实际判断时强制降级为附件下载（见 inlineSafeMime）。
var inlineSafeMimeTypes = map[string]struct{}{
	"image/png":       {},
	"image/jpeg":      {},
	"image/gif":       {},
	"image/webp":      {},
	"image/avif":      {},
	"image/svg+xml":   {},
	"text/plain":      {},
	"application/pdf": {},
}

// FileHandler provides HTTP multipart upload/download for storage.
type FileHandler struct {
	policies  *domainauth.PolicySet
	cfg       *config.AppConfig
	auth      *httpAuth
	storage   *appstorage.Storage
	trusted   *interceptor.TrustedProxies
	auditRepo audit.Repository
	logger    *slog.Logger
}

// NewFileHandler creates a new file HTTP handler.
func NewFileHandler(
	cfg *config.AppConfig,
	validator AuthValidator,
	storage *appstorage.Storage,
	auditRepo audit.Repository,
	logger *slog.Logger,
	policies *domainauth.PolicySet,
) (*FileHandler, error) {
	if logger == nil {
		logger = slog.Default()
	}
	trusted, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	return &FileHandler{cfg: cfg, auth: newHTTPAuth(validator, policies), storage: storage, auditRepo: auditRepo,
		trusted: trusted, logger: logger, policies: policies}, nil
}

// clientIP 与 gRPC ClientInfoInterceptor 走同一 trusted-proxy 规则。
func (h *FileHandler) clientIP(r *http.Request) string {
	return h.trusted.ResolveClientIP(
		interceptor.PeerIPFromAddr(r.RemoteAddr),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-Ip"),
	)
}

// logOp 输出文件上传/下载的结构化访问日志（成功/失败各一条），不记录凭证。
func (h *FileHandler) logOp(r *http.Request, op, bucketID, fileID string, principal *shared.Principal, err error) {
	attrs := []any{
		slog.String("op", op),
		slog.String("bucket_id", bucketID),
		slog.String("file_id", fileID),
		slog.String("ip", h.clientIP(r)),
	}
	if principal != nil {
		attrs = append(attrs,
			slog.String("actor_id", string(principal.ActorID)),
			slog.String("actor_kind", string(principal.ActorKind)),
			slog.String("credential_type", string(principal.CredentialType)),
			slog.String("project_id", principal.ProjectID),
		)
	}
	// slog 之外落持久审计（P2-6；best-effort，见 auditFromHTTP）。
	meta := map[string]any{"op": op, "bucket_id": bucketID}
	if principal != nil {
		meta["project_id"] = principal.ProjectID
	}
	resource := fileID
	if resource == "" {
		resource = bucketID
	}
	auditFromHTTP(r, h.clientIP(r), h.auditRepo, h.logger, "http.storage."+op, meta, resource, principal, err)
	if err != nil {
		st, _ := status.FromError(err)
		attrs = append(attrs, slog.String("error", st.Code().String()))
		h.logger.Warn("file operation failed", attrs...)
		return
	}
	h.logger.Info("file operation", attrs...)
}

// Register attaches the upload/download routes to the gateway mux.
func (h *FileHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/files", h.upload)
	_ = mux.HandlePath("GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/download", h.download)
	_ = mux.HandlePath("GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/view", h.download)
	_ = mux.HandlePath("GET", "/v1/storage/buckets/{bucketId}/files/{fileId}/preview", h.preview)
	// 分片上传（upload session）：create/get/uploadChunk/complete/abort。
	_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads", h.createUpload)
	_ = mux.HandlePath("GET", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}", h.getUpload)
	_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/chunks/{partNumber}", h.uploadChunk)
	_ = mux.HandlePath("POST", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}/complete", h.completeUpload)
	_ = mux.HandlePath("DELETE", "/v1/storage/buckets/{bucketId}/uploads/{uploadId}", h.abortUpload)
}

// maxUploadBytes caps the total size of a multipart upload request body。
// +1MiB 缓冲 multipart 边界/头部开销，避免整 100MiB 文件被拒。
const maxUploadBytes = (100 << 20) + (1 << 20)

// maxChunkUploadBytes caps a chunk upload request body（16MiB 整片 + 缓冲；
// use-case 仍按 size 严格拒绝）。
const maxChunkUploadBytes = domainstorage.MaxChunkSize + (1 << 20)

func (h *FileHandler) upload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "upload", bucketID, "", nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "upload", bucketID, "", principal, err)
		httpError(w, err)
		return
	}
	if bucketID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing bucket id"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpError(w, status.Error(codes.InvalidArgument, "invalid multipart form or file too large"))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	fileHeader := r.MultipartForm.File["file"]
	if len(fileHeader) == 0 {
		file, fh, err := r.FormFile("file")
		if err != nil {
			httpError(w, status.Error(codes.InvalidArgument, "missing file"))
			return
		}
		defer func() { _ = file.Close() }()
		h.createFile(ctx, w, r, projectID, bucketID, file, fh.Size, fh.Filename, fh.Header.Get("Content-Type"), principal)
		return
	}

	fh := fileHeader[0]
	f, err := fh.Open()
	if err != nil {
		httpError(w, status.Error(codes.Internal, "cannot open uploaded file"))
		return
	}
	defer func() { _ = f.Close() }()

	h.createFile(ctx, w, r, projectID, bucketID, f, fh.Size, fh.Filename, fh.Header.Get("Content-Type"), principal)
}

// createUploadRequest 是创建分片上传会话的 JSON body。A8 后 Permissions 已废弃：文件权限仅由 owner_user_id + bucket.Public 决定，请求中的 permissions 忽略。
type createUploadRequest struct {
	Name     string            `json:"name"`
	MimeType string            `json:"mime_type"`
	Size     int64             `json:"size"`
	Metadata map[string]string `json:"metadata"`
}

// createUpload 创建分片上传会话：POST /v1/storage/buckets/{bucketId}/uploads。
func (h *FileHandler) createUpload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "create-upload", bucketID, "", nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "create-upload", bucketID, "", principal, err)
		httpError(w, err)
		return
	}
	if bucketID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing bucket id"))
		return
	}

	// JSON body 1MiB 上限（防超大 metadata）。
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req createUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, status.Error(codes.InvalidArgument, "invalid JSON body"))
		return
	}
	session, err := h.storage.CreateUploadSession(ctx, appstorage.CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      req.Name,
		MimeType:  req.MimeType,
		Size:      req.Size,
		Metadata:  req.Metadata,
	}, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "create-upload", bucketID, "", principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "create-upload", bucketID, session.ID, principal, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"upload_id":  session.ID,
		"file_id":    session.FileID,
		"chunk_size": session.ChunkSize,
		"part_count": session.PartCount,
		"expires_at": session.ExpiresAt,
	})
}

// getUpload 查询会话（续传）：GET /v1/storage/buckets/{bucketId}/uploads/{uploadId}。
// GET 分支自动要求 storage.read scope。
func (h *FileHandler) getUpload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	uploadID := pathParams["uploadId"]
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "get-upload", bucketID, uploadID, nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "get-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	session, err := h.storage.GetUploadSession(ctx, projectID, uploadID, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "get-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	if session.BucketID != bucketID {
		err := status.Error(codes.NotFound, "upload session not found in bucket")
		h.logOp(r, "get-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "get-upload", bucketID, uploadID, principal, nil)
	received := make([]int, 0, len(session.Received))
	for p := range session.Received {
		received = append(received, p)
	}
	sort.Ints(received)
	writeJSON(w, http.StatusOK, map[string]any{
		"upload_id":  session.ID,
		"part_count": session.PartCount,
		"received":   received,
		"chunk_size": session.ChunkSize,
	})
}

// uploadChunk 上传分片：POST /v1/storage/buckets/{bucketId}/uploads/{uploadId}/chunks/{partNumber}。
// multipart 字段名 `chunk`；成功不记 logOp（防 64 片 64 条噪音），失败仍记。
func (h *FileHandler) uploadChunk(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	uploadID := pathParams["uploadId"]
	partNumber, err := strconv.Atoi(pathParams["partNumber"])
	if err != nil {
		httpError(w, status.Error(codes.InvalidArgument, "invalid part number"))
		return
	}
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "upload-chunk", bucketID, uploadID, nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "upload-chunk", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	if bucketID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing bucket id"))
		return
	}
	session, err := h.storage.GetUploadSession(ctx, projectID, uploadID, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "upload-chunk", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	if session.BucketID != bucketID {
		err := status.Error(codes.NotFound, "upload session not found in bucket")
		h.logOp(r, "upload-chunk", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxChunkUploadBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		httpError(w, status.Error(codes.InvalidArgument, "invalid multipart form or chunk too large"))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	fh := r.MultipartForm.File["chunk"]
	if len(fh) == 0 {
		httpError(w, status.Error(codes.InvalidArgument, "missing chunk"))
		return
	}
	f, err := fh[0].Open()
	if err != nil {
		httpError(w, status.Error(codes.Internal, "cannot open uploaded chunk"))
		return
	}
	defer func() { _ = f.Close() }()

	received, err := h.storage.UploadChunk(ctx, projectID, uploadID, partNumber, f, fh[0].Size, principal.StorageOwnerID(), principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "upload-chunk", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"part_number":    partNumber,
		"received_count": received,
	})
}

// completeUpload 合并分片并创建文件文档：POST /v1/storage/buckets/{bucketId}/uploads/{uploadId}/complete。
func (h *FileHandler) completeUpload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	uploadID := pathParams["uploadId"]
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "complete-upload", bucketID, uploadID, nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "complete-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	session, err := h.storage.GetUploadSession(ctx, projectID, uploadID, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "complete-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	if session.BucketID != bucketID {
		err := status.Error(codes.NotFound, "upload session not found in bucket")
		h.logOp(r, "complete-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}

	file, err := h.storage.CompleteUpload(ctx, projectID, uploadID, principal.StorageOwnerID(), principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "complete-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "complete-upload", bucketID, file.ID, principal, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         file.ID,
		"bucket_id":  file.BucketID,
		"name":       file.Name,
		"mime_type":  file.MimeType,
		"size":       file.Size,
		"created_at": file.CreatedAt,
		"updated_at": file.UpdatedAt,
	})
}

// abortUpload 取消上传并清理分片：DELETE /v1/storage/buckets/{bucketId}/uploads/{uploadId}。
func (h *FileHandler) abortUpload(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	uploadID := pathParams["uploadId"]
	principal, err := h.authorize(r, bucketID)
	if err != nil {
		h.logOp(r, "abort-upload", bucketID, uploadID, nil, err)
		httpError(w, err)
		return
	}
	projectID := h.auth.projectID(r, principal)
	if projectID == "" {
		err := status.Error(codes.Unauthenticated, "missing project context")
		h.logOp(r, "abort-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	session, err := h.storage.GetUploadSession(ctx, projectID, uploadID, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "abort-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	if session.BucketID != bucketID {
		err := status.Error(codes.NotFound, "upload session not found in bucket")
		h.logOp(r, "abort-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}

	if err := h.storage.AbortUpload(ctx, projectID, uploadID, principal.StorageOwnerID(), principal.DocPrincipal()); err != nil {
		h.logOp(r, "abort-upload", bucketID, uploadID, principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "abort-upload", bucketID, uploadID, principal, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *FileHandler) createFile(ctx context.Context, w http.ResponseWriter, r *http.Request, projectID, bucketID string, rd io.Reader, size int64, name, contentType string, principal *shared.Principal) {
	file, err := h.storage.CreateFile(ctx, appstorage.CreateFileCommand{
		ProjectID:   projectID,
		OwnerUserID: principal.StorageOwnerID(),
		BucketID:    bucketID,
		Name:        name,
		MimeType:    contentType,
	}, rd, size, principal.DocPrincipal())
	if err != nil {
		h.logOp(r, "upload", bucketID, "", principal, err)
		httpError(w, err)
		return
	}
	h.logOp(r, "upload", bucketID, file.ID, principal, nil)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         file.ID,
		"bucket_id":  file.BucketID,
		"name":       file.Name,
		"mime_type":  file.MimeType,
		"size":       file.Size,
		"created_at": file.CreatedAt,
		"updated_at": file.UpdatedAt,
	})
}

func (h *FileHandler) download(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	fileID := pathParams["fileId"]
	if bucketID == "" || fileID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing bucket or file id"))
		return
	}

	projectID, principal, actor, public, err := h.resolveReadContext(ctx, r, bucketID, fileID)
	if err != nil {
		h.logOp(r, "download", bucketID, fileID, nil, err)
		httpError(w, err)
		return
	}

	file, reader, err := h.storage.GetFile(ctx, projectID, bucketID, fileID, principal)
	if err != nil {
		h.logOp(r, "download", bucketID, fileID, actor, err)
		httpError(w, err)
		return
	}
	defer func() { _ = reader.Close() }()
	h.logOp(r, "download", bucketID, fileID, actor, nil)

	w.Header().Set("Content-Type", file.MimeType)
	disposition := "attachment"
	if !strings.HasSuffix(r.URL.Path, "/download") && inlineSafeMime(file.MimeType) {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", contentDispositionHeader(disposition, file.Name))
	// 响应加固：禁止浏览器 MIME 嗅探，并把同源输出限制在沙箱内，杜绝存储型 XSS。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// 私有文件（凭证/token 路径）禁止缓存；仅公开 bucket 匿名路径允许公共缓存。
	if public {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

// resolveReadContext 解析文件读请求的项目与主体：
//  1. 常规凭证（API key / admin / end-user JWT / session cookie）；
//  2. 无凭证但带有效 file token（短期匿名下载凭证，优先级最高）；
//  3. 无凭证且 bucket 为 public（URL 需携带 project 参数定位项目），
//     以匿名主体读取（文件文档级 read:any 权限兜底）。
//
// 返回项目 ID、文档层 principal、用于日志的 actor（匿名路径为 nil）以及
// isPublicBucket（仅公开 bucket 匿名路径为 true，用于 Cache-Control 决策）。
func (h *FileHandler) resolveReadContext(ctx context.Context, r *http.Request, bucketID, fileID string) (string, databases.Principal, *shared.Principal, bool, error) {
	principal, err := h.authorize(r, bucketID)
	if err == nil {
		projectID := h.auth.projectID(r, principal)
		if projectID == "" {
			return "", databases.Principal{}, nil, false, status.Error(codes.Unauthenticated, "missing project context")
		}
		return projectID, principal.DocPrincipal(), principal, false, nil
	}

	// File token 匿名下载：token 绑定 project/bucket/file 与过期时间。
	// 优先从 header 读取（避免 query 泄露到日志/代理），兼容 query 参数。
	if token := fileTokenFromRequest(r); token != "" {
		projectID, tokBucket, tokFile, verr := h.storage.ParseFileToken(token)
		if verr == nil && tokBucket == bucketID && tokFile == fileID {
			return projectID, databases.SystemPrincipal, nil, false, nil
		}
	}

	// 公开 bucket 匿名读：需要 project 参数定位项目。
	projectID := strings.TrimSpace(r.URL.Query().Get("project"))
	if projectID != "" {
		// bucketID 直接拼入 query DSL（BuildEqual），resolve 前必须先校验格式：
		// 非法 ID 直接 400，不进入查询（防 DSL 注入与恶意长串探测）。
		if !isValidBucketID(bucketID) {
			return "", databases.Principal{}, nil, false, status.Error(codes.InvalidArgument, "invalid bucket id")
		}
		b, berr := h.storage.GetBucket(ctx, projectID, bucketID, databases.GuestPrincipal)
		if berr == nil && b != nil && b.Public {
			return projectID, databases.GuestPrincipal, nil, true, nil
		}
	}

	return "", databases.Principal{}, nil, false, err
}

// fileTokenFromRequest 从请求提取 file token：优先 header（X-File-Token /
// Authorization: Bearer），回退 query ?token=（兼容旧客户端，query 仍可用但建议迁移至 header）。
func fileTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("X-File-Token")); token != "" {
		return token
	}
	if token := strings.TrimSpace(r.Header.Get("X-Torchwood-File-Token")); token != "" {
		return token
	}
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			if token := strings.TrimSpace(auth[7:]); token != "" && strings.Count(token, ".") == 4 {
				return token
			}
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// inlineSafeMime 判断文件 MIME 是否可安全以 inline 方式展示：
// 视频/音频（无脚本执行面）与白名单类型可以内联；SVG 可内嵌脚本，一律按附件下载。
func inlineSafeMime(mime string) bool {
	if mime == "image/svg+xml" {
		return false
	}
	if _, ok := inlineSafeMimeTypes[mime]; ok {
		return true
	}
	return strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")
}

// previewImageMimeTypes 是可生成缩略图的图片类型；webp 由 golang.org/x/image 解码。
var previewImageMimeTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/webp": {},
}

// maxPreviewSourceBytes 限制预览源文件大小（防解压炸弹与内存耗尽）。
const maxPreviewSourceBytes = 50 << 20 // 50 MiB

// maxPreviewHeaderBytes 限制预览头部解码阶段的读取上限（覆盖 PNG IHDR / JPEG
// SOF / GIF 逻辑屏幕描述 / WebP 头等解码器所需的最小字节数）。
const maxPreviewHeaderBytes = 512 << 10 // 512 KiB

// maxPreviewSourceDimension 限制预览源图片边长（防整图解压出超大位图导致 OOM）。
const maxPreviewSourceDimension = 8192

// maxPreviewDimension 限制缩放后的输出尺寸。
const maxPreviewDimension = 4096

// previewSourceConfig 只读有限 header 解析并校验预览源图像：
// 先读最多 maxPreviewHeaderBytes 字节解析宽高，任一维度超限直接拒绝（不读全量）；
// 非图片/损坏图片返回 InvalidArgument（400），读源失败返回 Internal。
// 已读取的 header 字节会一并返回，调用方必须将其拼回后续的全文件读取
// （小文件可能整个被 header 阶段读完，直接续读只剩空流）。
// 注意：header 先整体读入自有缓冲再交给 image.DecodeConfig，其内部 bufio
// 预读只作用于该缓冲，不会污染后续的全文件读取。
func previewSourceConfig(src io.Reader) (image.Config, []byte, error) {
	header, err := io.ReadAll(io.LimitReader(src, maxPreviewHeaderBytes))
	if err != nil {
		return image.Config{}, nil, status.Error(codes.Internal, "cannot read image")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(header))
	if err != nil {
		return image.Config{}, nil, status.Error(codes.InvalidArgument, "cannot decode image")
	}
	if cfg.Width > maxPreviewSourceDimension || cfg.Height > maxPreviewSourceDimension {
		return image.Config{}, nil, status.Error(codes.InvalidArgument, "image dimensions too large to preview")
	}
	return cfg, header, nil
}

// preview 生成图片缩略图：GET /v1/storage/buckets/{bucketId}/files/{fileId}/preview?width=&height=
// 鉴权与 download 一致（凭证 / file token / public bucket），仅支持图片类型，
// 无缩放参数时直接回源。
func (h *FileHandler) preview(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	ctx := r.Context()
	bucketID := pathParams["bucketId"]
	fileID := pathParams["fileId"]
	if bucketID == "" || fileID == "" {
		httpError(w, status.Error(codes.InvalidArgument, "missing bucket or file id"))
		return
	}

	projectID, principal, actor, public, err := h.resolveReadContext(ctx, r, bucketID, fileID)
	if err != nil {
		h.logOp(r, "preview", bucketID, fileID, nil, err)
		httpError(w, err)
		return
	}

	file, reader, err := h.storage.GetFile(ctx, projectID, bucketID, fileID, principal)
	if err != nil {
		h.logOp(r, "preview", bucketID, fileID, actor, err)
		httpError(w, err)
		return
	}
	defer func() { _ = reader.Close() }()

	if _, ok := previewImageMimeTypes[file.MimeType]; !ok {
		httpError(w, status.Error(codes.InvalidArgument, "file type is not previewable"))
		return
	}
	if file.Size > maxPreviewSourceBytes {
		httpError(w, status.Error(codes.InvalidArgument, "file too large to preview"))
		return
	}

	width, height, err := parsePreviewDimensions(r)
	if err != nil {
		httpError(w, err)
		return
	}

	cacheControl := "private, no-store"
	if public {
		cacheControl = "public, max-age=86400"
	}

	// 无缩放参数时回源（仍带安全响应头）。
	if width <= 0 && height <= 0 {
		serveImage(w, file, reader, cacheControl)
		return
	}

	// 解码前先读有限 header（512KB，不读全量）解析图像宽高：任一维度超限直接
	// 拒绝，防 50MiB 压缩图解码出 ~600MB 位图的 OOM DoS；通过后才受限读取全文件。
	// header 阶段已消费的字节必须拼回（小文件可能整个在 header 阶段读完）。
	_, header, err := previewSourceConfig(reader)
	if err != nil {
		httpError(w, err)
		return
	}
	srcBytes, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(header), reader), maxPreviewSourceBytes+1))
	if err != nil {
		httpError(w, status.Error(codes.Internal, "cannot read image"))
		return
	}
	if len(srcBytes) > maxPreviewSourceBytes {
		httpError(w, status.Error(codes.InvalidArgument, "file too large to preview"))
		return
	}

	src, err := imaging.Decode(bytes.NewReader(srcBytes), imaging.AutoOrientation(true))
	if err != nil {
		// 头部解析通过但整图解码失败 = 损坏图片，属客户端输入问题，返回 400。
		httpError(w, status.Error(codes.InvalidArgument, "cannot decode image"))
		return
	}
	if width > maxPreviewDimension {
		width = maxPreviewDimension
	}
	if height > maxPreviewDimension {
		height = maxPreviewDimension
	}
	dst := imaging.Fit(src, width, height, imaging.Lanczos)

	// 流式编码：w 直接作为 Encoder 目标，避免整图 bytes.Buffer 峰值内存翻倍。
	w.Header().Set("Content-Type", file.MimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	if err := imaging.Encode(w, dst, imagingFormat(file.MimeType)); err != nil {
		h.logger.Warn("preview encode failed", "bucket_id", bucketID, "file_id", fileID, "error", err)
		return
	}
	h.logOp(r, "preview", bucketID, fileID, actor, nil)
}

// serveImage 原样输出图片（带安全响应头）。
func serveImage(w http.ResponseWriter, file *domainstorage.File, reader io.Reader, cacheControl string) {
	w.Header().Set("Content-Type", file.MimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

// validBucketIDPattern 校验路径参数 bucketID 的字符集与长度（公开匿名路径会
// 将 bucketID 拼入 query DSL，必须防注入；bucketID 由 idgen.UUID 生成，兼容
// 历史短 ID）。
var validBucketIDPattern = regexp.MustCompile(`^[0-9a-zA-Z_-]{1,64}$`)

// isValidBucketID 判断 bucketID 是否合法：非空 + 字符集/长度约束。
func isValidBucketID(id string) bool {
	return idgen.ID(id).IsValid() && validBucketIDPattern.MatchString(id)
}

// parsePreviewDimensions 解析 width/height query 参数（正整数）。
func parsePreviewDimensions(r *http.Request) (int, int, error) {
	parse := func(name string) (int, error) {
		raw := strings.TrimSpace(r.URL.Query().Get(name))
		if raw == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > maxPreviewDimension {
			return 0, status.Error(codes.InvalidArgument, "invalid "+name)
		}
		return n, nil
	}
	w, err := parse("width")
	if err != nil {
		return 0, 0, err
	}
	hgt, err := parse("height")
	if err != nil {
		return 0, 0, err
	}
	return w, hgt, nil
}

// imagingFormat 按 MIME 选择输出编码格式；webp 输出 JPEG 兜底（x/image webp 仅解码）。
func imagingFormat(mime string) imaging.Format {
	switch mime {
	case "image/png":
		return imaging.PNG
	case "image/gif":
		return imaging.GIF
	case "image/webp":
		return imaging.JPEG
	default:
		return imaging.JPEG
	}
}

// authorize 认证并做方法级授权（A9/C5 对齐），携带请求寻址的 bucket 目标
// （T-02 资源级 scope：storage:<bucket> 仅放行该桶的请求）：
//   - EndUser：允许 upload/download/view（含 preview/分片上传），受 A8 文件级 owner 校验约束（此处仅放行，文件层再判）
//   - API key：按方法区分 CreateFile（POST/写）/ GetFile（GET/读）scope，与 gRPC StorageService 相同；实例限定 scope 以 bucketID 强制
//   - Admin：与 gRPC 相同（scope 已由 h.auth.authorize 完成项目绑定；此处额外校验写方法的 admin 角色——viewer 仅读，member/owner/admin 可写）
//
// 写操作的审计与 gRPC 一致：至少 slog 带 actor 的结构化日志（logOp）；若 handler 持有 audit.Repository 则额外 Insert（当前仅 slog）。
// 认证/项目解析等公共逻辑见 httpAuth（auth.go）。
func (h *FileHandler) authorize(r *http.Request, bucketID string) (*shared.Principal, error) {
	isRead := r.Method == http.MethodGet
	method := domainauth.StorageServiceGetFile
	if !isRead {
		method = domainauth.StorageServiceCreateFile
	}
	p, err := h.auth.authorize(r, func(*http.Request) string { return method }, domainauth.ScopeTargets{BucketID: bucketID})
	if err != nil {
		return nil, err
	}
	if p.ActorKind == shared.ActorKindEndUser {
		return p, nil
	}
	if p.ActorKind == shared.ActorKindAdmin && !isRead {
		// 角色门从 PolicySet 派生（镜像收编 M3：与拦截器 admin_roles 同源）。
		if !p.HasAnyRole(domainauth.RoleStrings(h.policies.AllowedAdminRoles(domainauth.StorageServiceCreateFile))) {
			return nil, status.Error(codes.PermissionDenied, "admin role not permitted for storage write")
		}
	}
	return p, nil
}

func safeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"', r == '\\':
			b.WriteByte('_')
		case r < 32, r == 127:
			// drop control characters to prevent header injection
			continue
		case r == '\n', r == '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "download"
	}
	return out
}

func contentDispositionHeader(disposition, name string) string {
	safe := safeFilename(name)
	ascii := asciiFilenameFallback(safe)
	encoded := url.PathEscape(safe)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disposition, ascii, encoded)
}

func asciiFilenameFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 32 && r <= 126 && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._ ")
	if out == "" {
		return "download"
	}
	return out
}

func httpError(w http.ResponseWriter, err error) {
	st, ok := status.FromError(err)
	if !ok {
		st = status.New(codes.Internal, err.Error())
	}
	httpStatus := runtime.HTTPStatusFromCode(st.Code())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"type":    st.Code().String(),
			"message": st.Message(),
		},
	})
	_, _ = w.Write(payload)
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
