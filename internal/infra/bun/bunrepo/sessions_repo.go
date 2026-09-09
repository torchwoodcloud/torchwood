package bunrepo

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const sha256HexLen = 64

var _ domainauth.SessionRepository = (*SessionRepository)(nil)

type SessionRepository struct {
	db *clients.Database
}

func NewSessionRepository(db *clients.Database) *SessionRepository {
	return &SessionRepository{db: db}
}

func (r *SessionRepository) Insert(ctx context.Context, projectID string, s *domainauth.Session) error {
	if s == nil || strings.TrimSpace(s.ID) == "" {
		return status.Error(codes.InvalidArgument, "session id is required")
	}
	if strings.TrimSpace(s.UserID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, sessionTable, "s")
	if err != nil {
		return err
	}
	m := mapSessionToModel(s)
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *SessionRepository) GetByID(ctx context.Context, projectID, id string) (*domainauth.Session, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, sessionTable, "s")
	if err != nil {
		return nil, err
	}
	m := new(model.Session)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("s.id = ?", id).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapSessionToDomain(m), nil
}

func (r *SessionRepository) ListByUser(ctx context.Context, projectID, userID string) ([]domainauth.Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, sessionTable, "s")
	if err != nil {
		return nil, err
	}
	var ms []model.Session
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("s.user_id = ?", userID).
		OrderExpr("s.expire_at DESC, s.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainauth.Session, len(ms))
	for i := range ms {
		out[i] = *mapSessionToDomain(&ms[i])
	}
	return out, nil
}

func (r *SessionRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return status.Error(codes.InvalidArgument, "session id is required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, sessionTable, "s")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Session)(nil)).ModelTableExpr(expr, sch).
		Where("s.id = ?", id).
		Exec(ctx)
	return err
}

func (r *SessionRepository) DeleteByUser(ctx context.Context, projectID, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, sessionTable, "s")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Session)(nil)).ModelTableExpr(expr, sch).
		Where("s.user_id = ?", userID).
		Exec(ctx)
	return err
}

func (r *SessionRepository) DeleteOldestByUser(ctx context.Context, projectID, userID string, keep int) error {
	if keep < 0 {
		return status.Error(codes.InvalidArgument, "keep must be non-negative")
	}
	if strings.TrimSpace(userID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	if _, _, _, err := Scoped(ctx, r.db, projectID, sessionTable, "s"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.%s
WHERE user_id = ?
  AND id NOT IN (
    SELECT id FROM (
      SELECT id FROM %s.%s
      WHERE user_id = ?
      ORDER BY expire_at DESC, id DESC
      LIMIT ?
    ) keep_rows
  )
`, quoted, sessionTable, quoted, sessionTable), userID, userID, keep)
	return err
}

func mapSessionToModel(s *domainauth.Session) *model.Session {
	now := time.Now()
	created := s.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := s.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	provider := s.Provider
	if provider == "" {
		provider = domainauth.ProviderEmail
	}
	factors := s.Factors
	if len(factors) == 0 {
		factors = append(json.RawMessage(nil), jsonEmptyObject...)
	}
	return &model.Session{
		ID:         s.ID,
		UserID:     s.UserID,
		SecretHash: s.SecretHash,
		Provider:   provider,
		UserAgent:  s.UserAgent,
		IP:         s.IP,
		Country:    s.Country,
		Factors:    factors,
		ExpireAt:   s.ExpireAt,
		CreatedAt:  created,
		UpdatedAt:  updated,
	}
}

func mapSessionToDomain(m *model.Session) *domainauth.Session {
	factors := append(json.RawMessage(nil), m.Factors...)
	if len(factors) == 0 {
		factors = append(json.RawMessage(nil), jsonEmptyObject...)
	}
	return &domainauth.Session{
		ID:         m.ID,
		UserID:     m.UserID,
		SecretHash: canonicalizeSessionSecretHash(m.SecretHash),
		Provider:   m.Provider,
		UserAgent:  m.UserAgent,
		IP:         m.IP,
		Country:    m.Country,
		Factors:    factors,
		ExpireAt:   m.ExpireAt,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

func sessionSecretLooksHashed(stored string) bool {
	if len(stored) != sha256HexLen {
		return false
	}
	_, err := hex.DecodeString(stored)
	return err == nil
}

func hashSessionSecret(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// canonicalizeSessionSecretHash 双读 secret_hash：64 字符 hex 视为已哈希，否则 SHA-256 hex。
func canonicalizeSessionSecretHash(stored string) string {
	if stored == "" || sessionSecretLooksHashed(stored) {
		return stored
	}
	return hashSessionSecret(stored)
}
