package testutil

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// CreateTestAPIKey inserts an API key and returns its raw secret plus a cleanup func.
func CreateTestAPIKey(ctx context.Context, db *clients.Database, projectID string, scopes []string) (string, func()) {
	if len(scopes) == 0 {
		scopes = []string{"users", "storage", "databases", "groups"}
	}
	secret := idgen.APIKeySecret()
	hash := sha256.Sum256([]byte(secret))
	key := &model.APIKey{
		ID:         idgen.UUID().String(),
		ProjectID:  projectID,
		Name:       fmt.Sprintf("test-key-%d", time.Now().UnixNano()),
		SecretHash: hex.EncodeToString(hash[:]),
		Scopes:     scopes,
		Enabled:    true,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := db.NewInsert().Model(key).Exec(ctx); err != nil {
		panic(err)
	}
	cleanup := func() {
		_, _ = db.NewDelete().Model((*model.APIKey)(nil)).Where("id = ?", key.ID).Exec(ctx)
	}
	return secret, cleanup
}
