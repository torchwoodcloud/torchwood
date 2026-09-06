package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appshared "github.com/torchwooddev/torchwood/internal/app/shared"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/storage"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/crud"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Storage struct {
	cfg         *config.AppConfig
	projectRepo projects.Repository
	store       storage.ObjectStore
	uploads     storage.UploadSessionStore
	buckets     storage.BucketRepository
	files       storage.FileRepository
	clock       Clock
}

// Clock 提供当前时间；生产默认系统时钟，测试注入假时钟拨快以验证过期路径
// （J6-4：替代真实 sleep 1.1s 的 token 过期测试）。
type Clock interface {
	Now() time.Time
}

// WithClock 返回注入自定义时钟的 StorageOption（测试专用）。
func WithClock(c Clock) StorageOption {
	return func(s *Storage) { s.clock = c }
}

// StorageOption 定制 Storage 可选依赖。
type StorageOption func(*Storage)

// now 返回当前时间；clock 未注入（零值/旧构造路径）时回退系统时钟。
func (s *Storage) now() time.Time {
	if s.clock == nil {
		return time.Now()
	}
	return s.clock.Now()
}

func NewStorage(
	cfg *config.AppConfig,
	projectRepo projects.Repository,
	store storage.ObjectStore,
	uploads storage.UploadSessionStore,
	buckets storage.BucketRepository,
	files storage.FileRepository,
	opts ...StorageOption,
) *Storage {
	s := &Storage{cfg: cfg, projectRepo: projectRepo, store: store, uploads: uploads, buckets: buckets, files: files}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type CreateBucketCommand struct {
	ProjectID string
	Name      string
	// Permissions 保留落库但读路径不用于鉴权（A8）：bucket 行 permissions JSONB 仅作兼容，访问控制以 Public/owner+privileged 为准；Console 不应展示为 ACL。
	Permissions []string
	Public      bool
}

type CreateFileCommand struct {
	ProjectID   string
	OwnerUserID string
	BucketID    string
	Name        string
	MimeType    string
	Metadata    map[string]string
	// Permissions 已废弃（A8）：文件不再使用文档 _perms，服务端忽略该字段；保留以兼容旧调用方（proto 字段 6 仍存在）。
	Permissions []string
}

func (s *Storage) CreateBucket(ctx context.Context, cmd CreateBucketCommand) (*storage.Bucket, error) {
	// 纵深防御（G6-4/R06-P1）：CreateBucket 是 Server API 业务写操作，与
	// CreateUser 对齐使用 RequireServerPrincipal（console admin 会话或 API key
	// 主体；viewer 角色细粒度由拦截器 adminRoleMethodRules 把关）。RequirePlatformPrincipal
	// 过严：会拒绝拦截器已放行的 member/owner/admin 会话与 API key 写路径。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if cmd.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	project, err := s.resolveProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}

	bucketID := idgen.UUID().String()
	now := time.Now()
	bucket := &storage.Bucket{
		ID:          bucketID,
		ProjectID:   project.ID,
		Name:        cmd.Name,
		Permissions: cmd.Permissions,
		Public:      cmd.Public,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.buckets.Insert(ctx, project.ID, bucket); err != nil {
		return nil, fmt.Errorf("create bucket: %w", err)
	}
	return bucket, nil
}

// ListBuckets 返回 (buckets, total, nextPageToken, error)；nextPageToken 供
// 分页续拉（Round3 H6-1：此前被丢弃导致列表截断不可翻页）。
func (s *Storage) ListBuckets(ctx context.Context, projectID string, q databases.Query, principal databases.Principal) ([]storage.Bucket, int64, string, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, 0, "", err
	}

	list, err := s.buckets.List(ctx, project.ID)
	if err != nil {
		return nil, 0, "", err
	}
	docs := make([]storage.Bucket, 0, len(list))
	for _, b := range list {
		if b != nil {
			docs = append(docs, *b)
		}
	}
	return paginateBuckets(docs, q.PageSize, q.PageToken)
}

func (s *Storage) GetBucket(ctx context.Context, projectID, bucketID string, _ databases.Principal) (*storage.Bucket, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	got, err := s.buckets.GetByID(ctx, project.ID, bucketID)
	if err != nil {
		return nil, err
	}
	if got == nil {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	return got, nil
}

// UpdateBucketCommand 更新 bucket 元数据；空字段表示不修改。
type UpdateBucketCommand struct {
	ProjectID string
	ID        string
	Name      string
	Public    *bool
	Principal databases.Principal
}

func (s *Storage) UpdateBucket(ctx context.Context, cmd UpdateBucketCommand) (*storage.Bucket, error) {
	project, err := s.resolveProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	if cmd.Name != "" {
		data["name"] = cmd.Name
	}
	if cmd.Public != nil {
		data["public"] = *cmd.Public
	}
	if len(data) == 0 {
		return nil, status.Error(codes.InvalidArgument, "nothing to update")
	}
	if err := s.buckets.Update(ctx, project.ID, cmd.ID, data); err != nil {
		return nil, err
	}
	got, err := s.buckets.GetByID(ctx, project.ID, cmd.ID)
	if err != nil {
		return nil, err
	}
	if got == nil {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	return got, nil
}

func (s *Storage) DeleteBucket(ctx context.Context, projectID, bucketID string, principal databases.Principal) error {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return err
	}
	// 删除该 bucket 的文件文档与对象（按 bucket_id 过滤分页循环；清理由
	// SystemPrincipal 执行，避免调用方对个别文件无权限时留下孤儿文档）。
	var pageToken string
	for {
		q := databases.Query{PageSize: 1000, PageToken: pageToken}
		files, _, next, err := s.ListFiles(ctx, projectID, bucketID, q, databases.SystemPrincipal)
		if err != nil {
			return err
		}
		for _, f := range files {
			if derr := s.store.Delete(ctx, defaultBucketName(s.cfg), objectKey(project.ID, bucketID, f.ID)); derr != nil {
				slog.Warn("delete file object failed", "bucket", bucketID, "file", f.ID, "error", derr)
			}
			if derr := s.files.Delete(ctx, project.ID, f.ID); derr != nil {
				slog.Warn("delete file metadata failed", "bucket", bucketID, "file", f.ID, "error", derr)
			}
		}
		if next == "" || len(files) == 0 {
			break
		}
		pageToken = next
	}
	// 按前缀清尾：删除 ListFiles 不可见的残留对象（孤儿分片、complete 失败遗留等）。
	// 与上面的按文档删除有重叠，List+Delete 幂等。
	objects, err := s.store.List(ctx, defaultBucketName(s.cfg), objectKey(project.ID, bucketID, ""))
	if err != nil {
		slog.Warn("list objects for bucket cleanup failed", "bucket", bucketID, "error", err)
	} else {
		for _, obj := range objects {
			if derr := s.store.Delete(ctx, defaultBucketName(s.cfg), obj.Key); derr != nil {
				slog.Warn("delete object during bucket cleanup failed", "key", obj.Key, "error", derr)
			}
		}
	}
	return s.buckets.Delete(ctx, project.ID, bucketID)
}

func (s *Storage) CreateFile(ctx context.Context, cmd CreateFileCommand, content io.Reader, size int64, principal databases.Principal) (*storage.File, error) {
	cmd.MimeType = normalizeMimeType(cmd.MimeType)
	if cmd.BucketID == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket_id is required")
	}
	if cmd.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	project, err := s.resolveProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}

	bucket, err := s.buckets.GetByID(ctx, project.ID, cmd.BucketID)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}

	// A8：OwnerUserID 从 principal 派生（EndUser 填 user:<id>），丢弃未落地的 Permissions 切片。
	// 非 EndUser（keys/admin/system）保持空或调用方传入的 OwnerUserID（admin 可能携带 AdminID 但无鉴权意义）。
	ownerUserID := cmd.OwnerUserID
	if uid := storageEndUserID(principal); uid != "" {
		ownerUserID = uid
	} else if isStoragePrivileged(principal) { //nolint:staticcheck
		// API key / admin / system 创建的文件不归属到特定用户，保持传入值（通常空）。
		// 若调用方误传 EndUser 的 user id 但 principal 并非 EndUser，不覆盖以免伪造。
	} else { //nolint:staticcheck
		// 访客等无特权且非 EndUser 的主体不应通过 gRPC 直调进入文件创建（应由 HTTP 拦截器阻断），此处兜底不覆写。
	}

	fileID := idgen.UUID().String()
	now := time.Now()
	file := &storage.File{
		ID:          fileID,
		ProjectID:   project.ID,
		BucketID:    cmd.BucketID,
		Name:        cmd.Name,
		MimeType:    cmd.MimeType,
		Size:        size,
		Metadata:    cmd.Metadata,
		OwnerUserID: ownerUserID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.EnsureBucket(ctx, defaultBucketName(s.cfg)); err != nil {
		return nil, fmt.Errorf("ensure storage bucket: %w", err)
	}
	if err := s.files.Insert(ctx, project.ID, file); err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}
	if err := s.store.Put(ctx, defaultBucketName(s.cfg), objectKey(project.ID, cmd.BucketID, fileID), content, size, cmd.MimeType); err != nil {
		_ = s.files.Delete(ctx, project.ID, fileID)
		return nil, fmt.Errorf("upload file: %w", err)
	}
	return file, nil
}

func (s *Storage) GetFile(ctx context.Context, projectID, bucketID, fileID string, principal databases.Principal) (*storage.File, io.ReadCloser, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	bucket, err := s.buckets.GetByID(ctx, project.ID, bucketID)
	if err != nil {
		return nil, nil, err
	}
	if bucket == nil {
		return nil, nil, status.Error(codes.NotFound, "bucket not found")
	}
	file, err := s.files.GetByID(ctx, project.ID, fileID)
	if err != nil {
		return nil, nil, err
	}
	if file == nil || file.BucketID != bucketID {
		return nil, nil, status.Error(codes.NotFound, "file not found")
	}
	if !canAccessFile(bucket, file, principal) {
		return nil, nil, status.Error(codes.PermissionDenied, "permission denied")
	}
	reader, err := s.store.Get(ctx, defaultBucketName(s.cfg), objectKey(project.ID, bucketID, fileID))
	if err != nil {
		return file, nil, err
	}
	return file, reader, nil
}

func (s *Storage) DeleteFile(ctx context.Context, projectID, bucketID, fileID string, principal databases.Principal) error {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return err
	}
	bucket, err := s.buckets.GetByID(ctx, project.ID, bucketID)
	if err != nil {
		return err
	}
	if bucket == nil {
		return status.Error(codes.NotFound, "bucket not found")
	}
	file, err := s.files.GetByID(ctx, project.ID, fileID)
	if err != nil {
		return err
	}
	if file == nil || file.BucketID != bucketID {
		return status.Error(codes.NotFound, "file not found")
	}
	if !canAccessFile(bucket, file, principal) {
		return status.Error(codes.PermissionDenied, "permission denied")
	}
	if err := s.store.Delete(ctx, defaultBucketName(s.cfg), objectKey(project.ID, bucketID, fileID)); err != nil { //nolint:staticcheck
		// Continue to delete metadata even if object missing.
	}
	return s.files.Delete(ctx, project.ID, fileID)
}

func (s *Storage) ListFiles(ctx context.Context, projectID, bucketID string, q databases.Query, principal databases.Principal) ([]storage.File, int64, string, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, 0, "", err
	}
	bucket, err := s.buckets.GetByID(ctx, project.ID, bucketID)
	if err != nil {
		return nil, 0, "", err
	}
	if bucket == nil {
		return nil, 0, "", status.Error(codes.NotFound, "bucket not found")
	}

	list, err := s.files.ListByBucket(ctx, project.ID, bucketID)
	if err != nil {
		return nil, 0, "", err
	}
	out := make([]storage.File, 0, len(list))
	for _, f := range list {
		if f != nil {
			out = append(out, *f)
		}
	}
	// A8：EndUser 仅见自己文件，public bucket 或特权主体可见全部。
	if !bucket.Public && !isStoragePrivileged(principal) {
		uid := storageEndUserID(principal)
		if uid == "" {
			return nil, 0, "", status.Error(codes.PermissionDenied, "permission denied")
		}
		filtered := make([]storage.File, 0, len(out))
		for _, f := range out {
			if f.OwnerUserID == uid {
				filtered = append(filtered, f)
			}
		}
		out = filtered
	}
	return paginateFiles(out, q.PageSize, q.PageToken)
}

// UpdateFileCommand 携带可更新的文件元数据字段；空值表示不修改。
type UpdateFileCommand struct {
	ProjectID string
	BucketID  string
	FileID    string
	Name      string
	MimeType  string
	Metadata  map[string]string // nil 表示不修改；非 nil（含空 map）整体替换
	Principal databases.Principal
}

func (s *Storage) UpdateFile(ctx context.Context, cmd UpdateFileCommand) (*storage.File, error) {
	project, err := s.resolveProject(ctx, cmd.ProjectID)
	if err != nil {
		return nil, err
	}
	bucket, err := s.buckets.GetByID(ctx, project.ID, cmd.BucketID)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	file, err := s.files.GetByID(ctx, project.ID, cmd.FileID)
	if err != nil {
		return nil, err
	}
	if file == nil || file.BucketID != cmd.BucketID {
		return nil, status.Error(codes.NotFound, "file not found")
	}
	if !canAccessFile(bucket, file, cmd.Principal) {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}

	data := map[string]any{}
	if cmd.Name != "" {
		data["name"] = cmd.Name
	}
	if cmd.MimeType != "" {
		data["mime_type"] = normalizeMimeType(cmd.MimeType)
	}
	if cmd.Metadata != nil {
		data["metadata"] = cmd.Metadata
	}
	if len(data) == 0 {
		return nil, status.Error(codes.InvalidArgument, "nothing to update")
	}
	if err := s.files.Update(ctx, project.ID, cmd.FileID, data); err != nil {
		return nil, err
	}
	got, err := s.files.GetByID(ctx, project.ID, cmd.FileID)
	if err != nil {
		return nil, err
	}
	return got, nil
}

// GetStorageUsage 统计项目级 bucket/文件数量与总容量（按调用方读权限过滤）。
func (s *Storage) GetStorageUsage(ctx context.Context, projectID string, principal databases.Principal) (*storage.Usage, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	buckets, err := s.buckets.Count(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	files, err := s.files.Count(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	totalSize, err := s.files.SumSize(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	return &storage.Usage{Buckets: buckets, Files: files, TotalSize: totalSize}, nil
}

// maxFileTokenLifetime caps token validity at 1 hour (short-lived, W-L).
// Previously 7 days – excessive window for anonymous download URL leaked in logs.
const maxFileTokenLifetime = 3600

// defaultFileTokenLifetime is the validity when expires_in is not provided (15 minutes).
const defaultFileTokenLifetime = 15 * 60

// FileToken 是短期匿名下载凭证。
type FileToken struct {
	Token     string
	ExpiresAt time.Time
}

// CreateFileToken 为文件签发 HMAC 签名的短期匿名下载 token（绑定 bucket/file/过期时间）。
func (s *Storage) CreateFileToken(ctx context.Context, projectID, bucketID, fileID string, expiresIn int64, principal databases.Principal) (*FileToken, error) {
	project, err := s.resolveProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	bucket, err := s.buckets.GetByID(ctx, project.ID, bucketID)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	file, err := s.files.GetByID(ctx, project.ID, fileID)
	if err != nil {
		return nil, err
	}
	if file == nil || file.BucketID != bucketID {
		return nil, status.Error(codes.NotFound, "file not found")
	}
	if !canAccessFile(bucket, file, principal) {
		return nil, status.Error(codes.PermissionDenied, "permission denied")
	}

	if expiresIn <= 0 {
		expiresIn = defaultFileTokenLifetime
	}
	if expiresIn > maxFileTokenLifetime {
		expiresIn = maxFileTokenLifetime
	}
	master := s.cfg.GetSecurity().GetJwt().GetSecret()
	if master == "" {
		return nil, status.Error(codes.Internal, "file token secret is not configured")
	}
	expiresAt := s.now().Add(time.Duration(expiresIn) * time.Second)
	token := signFileToken(fileTokenKey(master), project.ID, bucketID, fileID, expiresAt.Unix())
	return &FileToken{Token: token, ExpiresAt: expiresAt}, nil
}

// ParseFileToken 校验匿名下载 token：签名正确、未过期，返回绑定的
// project/bucket/file。任何一项不符即返回错误（调用方仍需比对路径参数）。
// token 格式："{expiresAt}.{projectID}.{bucketID}.{fileID}.{hex(hmac)}"。
func (s *Storage) ParseFileToken(token string) (projectID, bucketID, fileID string, err error) {
	master := s.cfg.GetSecurity().GetJwt().GetSecret()
	if master == "" {
		return "", "", "", status.Error(codes.Internal, "file token secret is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return "", "", "", status.Error(codes.Unauthenticated, "invalid file token")
	}
	expiresAt, parseErr := strconv.ParseInt(parts[0], 10, 64)
	if parseErr != nil {
		return "", "", "", status.Error(codes.Unauthenticated, "invalid file token")
	}
	if s.now().Unix() >= expiresAt {
		return "", "", "", status.Error(codes.Unauthenticated, "file token expired")
	}
	pid, bid, fid := parts[1], parts[2], parts[3]
	expected := signFileToken(fileTokenKey(master), pid, bid, fid, expiresAt)
	if !hmac.Equal([]byte(expected), []byte(token)) {
		return "", "", "", status.Error(codes.Unauthenticated, "invalid file token")
	}
	return pid, bid, fid, nil
}

// fileTokenKey 从主密钥派生 file token 专用密钥（域分离：file token 不与其他
// JWT/会话凭证共用密钥材料；参考 pkg/jwtparser.DeriveKey 模式）。
func fileTokenKey(master string) []byte {
	return jwtparser.DeriveKey(master, jwtparser.PurposeFileToken)
}

// signFileToken 计算 token = "{expiresAt}.{projectID}.{bucketID}.{fileID}.{hex(hmac)}"。
func signFileToken(secret []byte, projectID, bucketID, fileID string, expiresAt int64) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(mac, "%s.%s.%s.%d", projectID, bucketID, fileID, expiresAt)
	return fmt.Sprintf("%d.%s.%s.%s.%s", expiresAt, projectID, bucketID, fileID, hex.EncodeToString(mac.Sum(nil)))
}

func (s *Storage) resolveProject(ctx context.Context, projectID string) (*projects.Project, error) {
	p, err := s.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return p, nil
}

// normalizeMimeType 归一化客户端声明的 Content-Type：
// 空值与可执行/危险类型（含带参数的，取分号前 base 判断）一律改判为 application/octet-stream，
// 防止存储型 XSS 经 /view 端点内联执行。
func normalizeMimeType(mime string) string {
	base := strings.Split(mime, ";")[0]
	switch base {
	case "text/html", "application/xhtml+xml", "application/javascript",
		"text/javascript", "application/xml", "text/xml":
		return "application/octet-stream"
	}
	if base == "" {
		return "application/octet-stream"
	}
	return mime
}

func defaultBucketName(cfg *config.AppConfig) string {
	if b := cfg.GetStorage().GetS3().GetBucket(); b != "" {
		return b
	}
	return storage.DefaultBucketName
}

func objectKey(projectID, bucketID, fileID string) string {
	return fmt.Sprintf("%s/%s/%s", projectID, bucketID, fileID)
}

// A8 权限辅助：storage 的 file 级鉴权以 owner_user_id + bucket.Public 为最小模型，不再假装文档 _perms。
func isStoragePrivileged(p databases.Principal) bool {
	if p.BypassesDocumentACL() {
		return true
	}
	if p.HasRole(databases.RoleKeys) {
		return true
	}
	for _, r := range p.Roles {
		switch r {
		case "owner", "admin", "member", "viewer":
			return true
		}
	}
	return false
}

func storageEndUserID(p databases.Principal) string {
	hasUsers := false
	for _, r := range p.Roles {
		if r == databases.RoleUsers {
			hasUsers = true
			break
		}
	}
	if !hasUsers {
		return ""
	}
	for _, r := range p.Roles {
		if id, ok := strings.CutPrefix(r, databases.RolePrefixUser); ok {
			return id
		}
	}
	return ""
}

func canAccessFile(bucket *storage.Bucket, file *storage.File, principal databases.Principal) bool {
	if bucket != nil && bucket.Public {
		return true
	}
	if isStoragePrivileged(principal) {
		return true
	}
	uid := storageEndUserID(principal)
	if uid != "" && file != nil && file.OwnerUserID == uid {
		return true
	}
	return false
}

func paginateBuckets(items []storage.Bucket, pageSize int32, pageToken string) ([]storage.Bucket, int64, string, error) {
	total := int64(len(items))
	offset := 0
	if pageToken != "" {
		var err error
		offset, err = crud.DecodePageToken(pageToken)
		if err != nil {
			return nil, 0, "", status.Error(codes.InvalidArgument, "invalid page_token")
		}
	}
	limit := int(pageSize)
	if limit <= 0 {
		limit = 25
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		token, err := crud.EncodePageToken(end)
		if err != nil {
			return nil, 0, "", status.Error(codes.Internal, err.Error())
		}
		next = token
	}
	return items[offset:end], total, next, nil
}

func paginateFiles(items []storage.File, pageSize int32, pageToken string) ([]storage.File, int64, string, error) {
	total := int64(len(items))
	offset := 0
	if pageToken != "" {
		var err error
		offset, err = crud.DecodePageToken(pageToken)
		if err != nil {
			return nil, 0, "", status.Error(codes.InvalidArgument, "invalid page_token")
		}
	}
	limit := int(pageSize)
	if limit <= 0 {
		limit = 25
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		token, err := crud.EncodePageToken(end)
		if err != nil {
			return nil, 0, "", status.Error(codes.Internal, err.Error())
		}
		next = token
	}
	return items[offset:end], total, next, nil
}
