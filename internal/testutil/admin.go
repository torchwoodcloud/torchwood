package testutil

import (
	"context"
	"fmt"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"github.com/torchwoodcloud/torchwood/pkg/password"
)

// CreateTestAdmin inserts a console admin and returns the model plus cleanup.
func CreateTestAdmin(ctx context.Context, db *clients.Database, role string) (*model.Admin, func()) {
	hash, err := password.Hash("Admin@123")
	if err != nil {
		panic(err)
	}
	admin := &model.Admin{
		ID:           idgen.UUID().String(),
		Email:        fmt.Sprintf("admin-%d@torchwood.local", time.Now().UnixNano()),
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if _, err := db.NewInsert().Model(admin).Exec(ctx); err != nil {
		panic(err)
	}
	cleanup := func() {
		_, _ = db.NewDelete().Model((*model.Admin)(nil)).Where("id = ?", admin.ID).Exec(ctx)
		_, _ = db.NewDelete().Model((*model.AdminProject)(nil)).Where("admin_id = ?", admin.ID).Exec(ctx)
	}
	return admin, cleanup
}

// SignAdminToken issues a console admin JWT compatible with auth.Validator.
func SignAdminToken(cfg *config.AppConfig, admin *model.Admin) (string, error) {
	now := time.Now()
	return jwtparser.Generate(jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		TokenID:   idgen.UUID().String(),
		UserID:    admin.ID,
		Username:  admin.Email,
		ActorKind: "admin",
		Roles:     []string{admin.Role},
		ExpiresAt: now.Add(time.Hour).Unix(),
		IssuedAt:  now.Unix(),
	})
}

// GrantAdminProject links a non-platform admin to a project.
func GrantAdminProject(ctx context.Context, db *clients.Database, adminID, projectID string) error {
	return bunrepo.NewAdminProjectRepository(db).GrantProjectAccess(ctx, adminID, projectID)
}
