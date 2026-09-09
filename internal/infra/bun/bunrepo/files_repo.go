package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ domainstorage.FileRepository = (*FileRepository)(nil)

type FileRepository struct {
	db *clients.Database
}

func NewFileRepository(db *clients.Database) *FileRepository {
	return &FileRepository{db: db}
}

func (r *FileRepository) Insert(ctx context.Context, projectID string, file *domainstorage.File) error {
	if file == nil || strings.TrimSpace(file.ID) == "" {
		return domainstorage.ErrFileIDRequired
	}
	if strings.TrimSpace(file.BucketID) == "" {
		return status.Error(codes.InvalidArgument, "bucket_id is required")
	}
	if strings.TrimSpace(file.Name) == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	if file.Size < 0 {
		return status.Error(codes.InvalidArgument, "size must be non-negative")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return err
	}
	m, err := mapFileToModel(file)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *FileRepository) GetByID(ctx context.Context, projectID, id string) (*domainstorage.File, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return nil, err
	}
	m := new(model.File)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("f.id = ?", id).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapFileToDomain(projectID, m), nil
}

func (r *FileRepository) ListByBucket(ctx context.Context, projectID, bucketID string) ([]*domainstorage.File, error) {
	if strings.TrimSpace(bucketID) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return nil, err
	}
	var ms []model.File
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("f.bucket_id = ?", bucketID).
		OrderExpr("f.created_at DESC, f.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domainstorage.File, len(ms))
	for i := range ms {
		out[i] = mapFileToDomain(projectID, &ms[i])
	}
	return out, nil
}

func (r *FileRepository) Count(ctx context.Context, projectID string) (int64, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return 0, err
	}
	n, err := conn.NewSelect().Model((*model.File)(nil)).ModelTableExpr(expr, sch).Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func (r *FileRepository) Update(ctx context.Context, projectID, id string, cols map[string]any) error {
	if strings.TrimSpace(id) == "" {
		return domainstorage.ErrFileIDRequired
	}
	cols, err := domainstorage.NormalizeFileUpdateColumns(cols)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return err
	}
	q := conn.NewUpdate().Model((*model.File)(nil)).ModelTableExpr(expr, sch).
		Where("f.id = ?", id)
	for col, val := range cols {
		encoded, err := encodeFileUpdateValue(col, val)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		q = q.Set(col+" = ?", encoded)
	}
	if _, ok := cols["updated_at"]; !ok {
		q = q.Set("updated_at = ?", time.Now())
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Error(codes.NotFound, "file not found")
	}
	return nil
}

func (r *FileRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domainstorage.ErrFileIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.File)(nil)).ModelTableExpr(expr, sch).
		Where("f.id = ?", id).
		Exec(ctx)
	return err
}

func (r *FileRepository) SumSize(ctx context.Context, projectID string) (int64, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, fileTable, "f")
	if err != nil {
		return 0, err
	}
	var sum int64
	err = conn.NewSelect().
		Model((*model.File)(nil)).
		ModelTableExpr(expr, sch).
		ColumnExpr("COALESCE(SUM(f.size), 0)").
		Scan(ctx, &sum)
	if err != nil {
		return 0, err
	}
	return sum, nil
}

func encodeFileUpdateValue(col string, v any) (any, error) {
	switch col {
	case "metadata":
		return marshalJSONCol(v, jsonEmptyObject)
	default:
		return v, nil
	}
}

func mapFileToModel(f *domainstorage.File) (*model.File, error) {
	now := time.Now()
	created := f.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := f.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	meta, err := marshalJSONCol(f.Metadata, jsonEmptyObject)
	if err != nil {
		return nil, err
	}
	return &model.File{
		ID:          f.ID,
		BucketID:    f.BucketID,
		Name:        strings.TrimSpace(f.Name),
		MimeType:    strings.TrimSpace(f.MimeType),
		Size:        f.Size,
		Metadata:    meta,
		OwnerUserID: nullIfEmpty(strings.TrimSpace(f.OwnerUserID)),
		CreatedAt:   created,
		UpdatedAt:   updated,
	}, nil
}

func mapFileToDomain(projectID string, m *model.File) *domainstorage.File {
	meta := unmarshalStringMap(m.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	return &domainstorage.File{
		ID:          m.ID,
		ProjectID:   projectID,
		BucketID:    m.BucketID,
		Name:        m.Name,
		MimeType:    m.MimeType,
		Size:        m.Size,
		Metadata:    meta,
		OwnerUserID: derefString(m.OwnerUserID),
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}
