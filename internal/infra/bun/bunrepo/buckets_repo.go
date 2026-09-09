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

var _ domainstorage.BucketRepository = (*BucketRepository)(nil)

type BucketRepository struct {
	db *clients.Database
}

func NewBucketRepository(db *clients.Database) *BucketRepository {
	return &BucketRepository{db: db}
}

func (r *BucketRepository) Insert(ctx context.Context, projectID string, bucket *domainstorage.Bucket) error {
	if bucket == nil || strings.TrimSpace(bucket.ID) == "" {
		return domainstorage.ErrBucketIDRequired
	}
	if strings.TrimSpace(bucket.Name) == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return err
	}
	m, err := mapBucketToModel(bucket)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *BucketRepository) GetByID(ctx context.Context, projectID, id string) (*domainstorage.Bucket, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return nil, err
	}
	m := new(model.Bucket)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("b.id = ?", id).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapBucketToDomain(projectID, m), nil
}

func (r *BucketRepository) List(ctx context.Context, projectID string) ([]*domainstorage.Bucket, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return nil, err
	}
	var ms []model.Bucket
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		OrderExpr("b.created_at DESC, b.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domainstorage.Bucket, len(ms))
	for i := range ms {
		out[i] = mapBucketToDomain(projectID, &ms[i])
	}
	return out, nil
}

func (r *BucketRepository) Count(ctx context.Context, projectID string) (int64, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return 0, err
	}
	n, err := conn.NewSelect().Model((*model.Bucket)(nil)).ModelTableExpr(expr, sch).Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func (r *BucketRepository) Update(ctx context.Context, projectID, id string, cols map[string]any) error {
	if strings.TrimSpace(id) == "" {
		return domainstorage.ErrBucketIDRequired
	}
	cols, err := domainstorage.NormalizeBucketUpdateColumns(cols)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return err
	}
	q := conn.NewUpdate().Model((*model.Bucket)(nil)).ModelTableExpr(expr, sch).
		Where("b.id = ?", id)
	for col, val := range cols {
		encoded, err := encodeBucketUpdateValue(col, val)
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
		return status.Error(codes.NotFound, "bucket not found")
	}
	return nil
}

func (r *BucketRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domainstorage.ErrBucketIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, bucketTable, "b")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Bucket)(nil)).ModelTableExpr(expr, sch).
		Where("b.id = ?", id).
		Exec(ctx)
	return err
}

func encodeBucketUpdateValue(col string, v any) (any, error) {
	switch col {
	case "permissions":
		return marshalJSONCol(v, jsonEmptyArray)
	default:
		return v, nil
	}
}

func mapBucketToModel(b *domainstorage.Bucket) (*model.Bucket, error) {
	now := time.Now()
	created := b.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := b.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	perms, err := marshalJSONCol(b.Permissions, jsonEmptyArray)
	if err != nil {
		return nil, err
	}
	return &model.Bucket{
		ID:          b.ID,
		Name:        strings.TrimSpace(b.Name),
		Permissions: perms,
		Public:      b.Public,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}, nil
}

func mapBucketToDomain(projectID string, m *model.Bucket) *domainstorage.Bucket {
	perms := unmarshalStringSlice(m.Permissions)
	if perms == nil {
		perms = []string{}
	}
	return &domainstorage.Bucket{
		ID:          m.ID,
		ProjectID:   projectID,
		Name:        m.Name,
		Permissions: perms,
		Public:      m.Public,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}
