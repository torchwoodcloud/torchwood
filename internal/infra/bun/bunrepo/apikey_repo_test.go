package bunrepo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

func TestAPIKeyRepository_SecretHashIndexAndScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	p1, _, c1 := testutil.CreateTestProject(ctx, db)
	defer c1()
	p2, _, c2 := testutil.CreateTestProject(ctx, db)
	defer c2()

	repo := bunrepo.NewAPIKeyRepository(db)
	now := time.Now()
	key := &projects.APIKey{
		ID:         "key-owner",
		ProjectID:  p1,
		Name:       "owner",
		SecretHash: "hash-owner-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Scopes:     []string{"*"},
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, repo.CreateAPIKey(ctx, key))

	dup := *key
	dup.ID = "key-dup"
	err := repo.CreateAPIKey(ctx, &dup)
	require.Error(t, err, "UNIQUE(secret_hash) 必须拒绝重复 hash")
	require.Contains(t, strings.ToLower(err.Error()), "api_keys_secret_hash_key")

	got, err := repo.GetAPIKey(ctx, p1, key.ID)
	require.NoError(t, err)
	require.NotNil(t, got)

	cross, err := repo.GetAPIKey(ctx, p2, key.ID)
	require.NoError(t, err)
	require.Nil(t, cross, "跨项目 GetAPIKey 必须 miss")

	byHash, err := repo.GetAPIKeyBySecretHash(ctx, key.SecretHash)
	require.NoError(t, err)
	require.NotNil(t, byHash)
	require.Equal(t, p1, byHash.ProjectID)

	_, err = db.ExecContext(ctx, `SET enable_seqscan = off`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, `SET enable_seqscan = on`) })

	rows, err := db.QueryContext(ctx, `EXPLAIN SELECT id FROM api_keys WHERE secret_hash = ?`, key.SecretHash)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	require.Contains(t, plan.String(), "api_keys_secret_hash_key")
}
