package servergrpc

import (
	"bytes"
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appstorage "github.com/torchwoodcloud/torchwood/internal/app/storage"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type StorageService struct {
	serverv1.UnimplementedStorageServiceServer
	storage *appstorage.Storage
}

func NewStorageService(storage *appstorage.Storage) *StorageService {
	return &StorageService{storage: storage}
}

func (s *StorageService) projectID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}

func (s *StorageService) CreateBucket(ctx context.Context, req *serverv1.CreateBucketRequest) (*serverv1.Bucket, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	bucket, err := s.storage.CreateBucket(ctx, appstorage.CreateBucketCommand{
		ProjectID:   projectID,
		Name:        req.GetName(),
		Permissions: req.GetPermissions(),
		Public:      req.GetPublic(),
	})
	if err != nil {
		return nil, err
	}
	return mapBucket(bucket), nil
}

func (s *StorageService) ListBuckets(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListBucketsResponse, error) {
	// storage 面不消费查询过滤：显式拒绝（预决策 7），不再静默忽略。
	if err := rejectListQueries(req.GetQueries()); err != nil {
		return nil, err
	}
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	buckets, total, next, err := s.storage.ListBuckets(ctx, projectID, databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.Bucket, len(buckets))
	for i := range buckets {
		out[i] = mapBucket(&buckets[i])
	}
	return &serverv1.ListBucketsResponse{
		Buckets: out,
		Meta:    &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *StorageService) GetBucket(ctx context.Context, req *serverv1.GetBucketRequest) (*serverv1.Bucket, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	got, err := s.storage.GetBucket(ctx, projectID, req.GetId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapBucket(got), nil
}

func (s *StorageService) DeleteBucket(ctx context.Context, req *serverv1.GetBucketRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.storage.DeleteBucket(ctx, projectID, req.GetId(), dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *StorageService) UpdateBucket(ctx context.Context, req *serverv1.UpdateBucketRequest) (*serverv1.Bucket, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "storage.buckets/"+req.GetId())
	bucket, err := s.storage.UpdateBucket(ctx, appstorage.UpdateBucketCommand{
		ProjectID: projectID,
		ID:        req.GetId(),
		Name:      req.GetName(),
		Public:    req.Public,
		Principal: dbPrincipal(ctx),
	})
	if err != nil {
		return nil, err
	}
	return mapBucket(bucket), nil
}

func (s *StorageService) CreateFile(ctx context.Context, req *serverv1.CreateFileRequest) (*serverv1.File, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	data := req.GetData()
	p, ok := contexts.Principal(ctx)
	ownerUserID := ""
	if ok {
		ownerUserID = p.StorageOwnerID()
	}
	file, err := s.storage.CreateFile(ctx, appstorage.CreateFileCommand{
		ProjectID:   projectID,
		OwnerUserID: ownerUserID,
		BucketID:    req.GetBucketId(),
		Name:        req.GetName(),
		MimeType:    req.GetMimeType(),
		Metadata:    req.GetMetadata(),
		Permissions: nil, // P3-7：proto 字段 6 已 reserved，不再透传
	}, bytes.NewReader(data), int64(len(data)), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapFile(file), nil
}

func (s *StorageService) ListFiles(ctx context.Context, req *serverv1.ListFilesRequest) (*serverv1.ListFilesResponse, error) {
	// storage 面不消费查询过滤：显式拒绝（预决策 7）；bucket_id 定位走路径
	// 参数，不属查询过滤。
	if err := rejectListQueries(req.GetQueries()); err != nil {
		return nil, err
	}
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	q := databases.Query{
		Queries:   req.GetQueries(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}
	q.Queries = append([]string{query.BuildEqual("bucket_id", req.GetBucketId())}, q.Queries...)
	files, total, next, err := s.storage.ListFiles(ctx, projectID, req.GetBucketId(), q, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.File, len(files))
	for i := range files {
		out[i] = mapFile(&files[i])
	}
	return &serverv1.ListFilesResponse{
		Files: out,
		Meta:  &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *StorageService) GetFile(ctx context.Context, req *serverv1.GetFileRequest) (*serverv1.File, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	file, _, err := s.storage.GetFile(ctx, projectID, req.GetBucketId(), req.GetFileId(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return mapFile(file), nil
}

func (s *StorageService) DeleteFile(ctx context.Context, req *serverv1.GetFileRequest) (*sharedv1.Empty, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.storage.DeleteFile(ctx, projectID, req.GetBucketId(), req.GetFileId(), dbPrincipal(ctx)); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

func (s *StorageService) UpdateFile(ctx context.Context, req *serverv1.UpdateFileRequest) (*serverv1.File, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "storage.buckets/"+req.GetBucketId()+"/files/"+req.GetFileId())
	file, err := s.storage.UpdateFile(ctx, appstorage.UpdateFileCommand{
		ProjectID: projectID,
		BucketID:  req.GetBucketId(),
		FileID:    req.GetFileId(),
		Name:      req.GetName(),
		MimeType:  req.GetMimeType(),
		Metadata:  req.GetMetadata(),
		Principal: dbPrincipal(ctx),
	})
	if err != nil {
		return nil, err
	}
	return mapFile(file), nil
}

func (s *StorageService) CreateFileToken(ctx context.Context, req *serverv1.CreateFileTokenRequest) (*serverv1.FileToken, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "storage.buckets/"+req.GetBucketId()+"/files/"+req.GetFileId()+"/tokens")
	token, err := s.storage.CreateFileToken(ctx, projectID, req.GetBucketId(), req.GetFileId(), req.GetExpiresIn(), dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return &serverv1.FileToken{
		Token:     token.Token,
		ExpiresAt: timestamppb.New(token.ExpiresAt),
	}, nil
}

func (s *StorageService) GetStorageUsage(ctx context.Context, _ *serverv1.GetStorageUsageRequest) (*serverv1.StorageUsage, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	ctx = contexts.WithAuditResource(ctx, "storage.usage")
	usage, err := s.storage.GetStorageUsage(ctx, projectID, dbPrincipal(ctx))
	if err != nil {
		return nil, err
	}
	return &serverv1.StorageUsage{
		Buckets:   usage.Buckets,
		Files:     usage.Files,
		TotalSize: usage.TotalSize,
	}, nil
}

func mapBucket(b *domainstorage.Bucket) *serverv1.Bucket {
	if b == nil {
		return nil
	}
	return &serverv1.Bucket{
		Id:          b.ID,
		Name:        b.Name,
		Permissions: b.Permissions,
		Public:      b.Public,
		CreatedAt:   timestamppb.New(b.CreatedAt),
		UpdatedAt:   timestamppb.New(b.UpdatedAt),
	}
}

func mapFile(f *domainstorage.File) *serverv1.File {
	if f == nil {
		return nil
	}
	return &serverv1.File{
		Id:        f.ID,
		BucketId:  f.BucketID,
		Name:      f.Name,
		MimeType:  f.MimeType,
		Size:      f.Size,
		Metadata:  f.Metadata,
		CreatedAt: timestamppb.New(f.CreatedAt),
		UpdatedAt: timestamppb.New(f.UpdatedAt),
	}
}

func dbPrincipal(ctx context.Context) databases.Principal {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return databases.Principal{}
	}
	return p.DocPrincipal()
}
