package testutil

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

type systemDocumentSpec struct {
	name        string
	attrs       []databases.Attribute
	indexes     []databases.Index
	permissions []databases.Permission
}

// SeedSystemDocumentCollections 在 sentinel 上重建七张系统集合文档表，
// 仅供测试（生产系统资源是静态表，不走文档面；见 06-databases §0）。
func SeedSystemDocumentCollections(ctx context.Context, db *clients.Database, docDB databases.DocumentDB, projectID string) error {
	now := time.Now()
	m := &model.DocumentDatabase{ProjectID: projectID, DatabaseID: ident.ProjectDataPlaneID, Name: "(project)", CreatedAt: now, UpdatedAt: now}
	if _, err := db.NewInsert().Model(m).
		On("CONFLICT (project_id, database_id) DO NOTHING").Exec(ctx); err != nil {
		return err
	}
	specs := systemDocumentSpecs()
	for _, id := range databases.SystemCollectionIDs {
		spec := specs[id]
		if err := docDB.CreateCollection(ctx, projectID, databases.SystemDatabaseID, id, spec.name, spec.attrs, spec.indexes, spec.permissions, true); err != nil {
			return err
		}
	}
	return nil
}

func systemDocumentSpecs() map[string]systemDocumentSpec {
	return map[string]systemDocumentSpec{
		"users": {
			name: "users",
			attrs: []databases.Attribute{
				{ID: "users_email", Key: "email", Type: "email", Size: 320},
				{ID: "users_password_hash", Key: "password_hash", Type: "string", Size: 512},
				{ID: "users_name", Key: "name", Type: "string", Size: 256},
				{ID: "users_status", Key: "status", Type: "string", Size: 64, Default: "active"},
				{ID: "users_email_verified", Key: "email_verified", Type: "boolean", Default: false},
				{ID: "users_pending_email", Key: "pending_email", Type: "email", Size: 320},
				{ID: "users_phone", Key: "phone", Type: "string", Size: 64},
				{ID: "users_phone_verified", Key: "phone_verified", Type: "boolean", Default: false},
				{ID: "users_labels", Key: "labels", Type: "json"},
				{ID: "users_prefs", Key: "prefs", Type: "json"},
				{ID: "users_factors", Key: "factors", Type: "json"},
			},
			indexes: []databases.Index{
				{ID: "users_email_unique", Type: "unique", Attributes: []string{"email"}},
				{ID: "users_phone", Type: "key", Attributes: []string{"phone"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "keys"},
				{Type: "read", Role: "user:{id}"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "user:{id}"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "user:{id}"},
				{Type: "delete", Role: "admin"},
			},
		},
		"sessions": {
			name: "sessions",
			attrs: []databases.Attribute{
				{ID: "sessions_user_id", Key: "user_id", Type: "string", Size: 64},
				{ID: "sessions_secret_hash", Key: "secret_hash", Type: "string", Size: 512},
				{ID: "sessions_provider", Key: "provider", Type: "string", Size: 64, Default: "email"},
				{ID: "sessions_user_agent", Key: "user_agent", Type: "string", Size: 1024},
				{ID: "sessions_ip", Key: "ip", Type: "string", Size: 64},
				{ID: "sessions_country", Key: "country", Type: "string", Size: 8},
				{ID: "sessions_factors", Key: "factors", Type: "json"},
				{ID: "sessions_expire_at", Key: "expire_at", Type: "datetime"},
			},
			indexes: []databases.Index{
				{ID: "sessions_user_id", Type: "key", Attributes: []string{"user_id"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "user:{id}"},
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "user:{id}"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "user:{id}"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "user:{id}"},
				{Type: "delete", Role: "admin"},
			},
		},
		"identities": {
			name: "identities",
			attrs: []databases.Attribute{
				{ID: "identities_user_id", Key: "user_id", Type: "string", Size: 64, Required: true},
				{ID: "identities_provider", Key: "provider", Type: "string", Size: 64, Required: true},
				{ID: "identities_provider_uid", Key: "provider_uid", Type: "string", Size: 256, Required: true},
				{ID: "identities_provider_email", Key: "provider_email", Type: "email", Size: 320},
				{ID: "identities_provider_data", Key: "provider_data", Type: "json"},
				{ID: "identities_expire_at", Key: "expire_at", Type: "datetime"},
			},
			indexes: []databases.Index{
				{ID: "identities_user_id", Type: "key", Attributes: []string{"user_id"}},
				{ID: "identities_provider_uid_unique", Type: "unique", Attributes: []string{"provider", "provider_uid"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "user:{id}"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "user:{id}"},
				{Type: "delete", Role: "admin"},
			},
		},
		"buckets": {
			name: "buckets",
			attrs: []databases.Attribute{
				{ID: "buckets_name", Key: "name", Type: "string", Size: 256},
				{ID: "buckets_permissions", Key: "permissions", Type: "json"},
				{ID: "buckets_public", Key: "public", Type: "boolean"},
			},
			indexes: []databases.Index{
				{ID: "buckets_name", Type: "key", Attributes: []string{"name"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "any"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "keys"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "keys"},
				{Type: "delete", Role: "admin"},
			},
		},
		"files": {
			name: "files",
			attrs: []databases.Attribute{
				{ID: "files_bucket_id", Key: "bucket_id", Type: "string", Size: 64},
				{ID: "files_name", Key: "name", Type: "string", Size: 256},
				{ID: "files_mime_type", Key: "mime_type", Type: "string", Size: 128},
				{ID: "files_size", Key: "size", Type: "integer"},
				{ID: "files_metadata", Key: "metadata", Type: "json"},
			},
			indexes: []databases.Index{
				{ID: "files_bucket_id", Type: "key", Attributes: []string{"bucket_id"}},
				{ID: "files_name_fulltext", Type: "fulltext", Attributes: []string{"name"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "users"},
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "any"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "user:{id}"},
				{Type: "update", Role: "keys"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "user:{id}"},
				{Type: "delete", Role: "keys"},
				{Type: "delete", Role: "admin"},
			},
		},
		"groups": {
			name: "groups",
			attrs: []databases.Attribute{
				{ID: "groups_name", Key: "name", Type: "string", Size: 256},
				{ID: "groups_permissions", Key: "permissions", Type: "json"},
				{ID: "groups_total", Key: "total", Type: "integer", Default: 0},
				{ID: "groups_prefs", Key: "prefs", Type: "json"},
			},
			indexes: []databases.Index{
				{ID: "groups_name", Type: "key", Attributes: []string{"name"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "users"},
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "any"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "group:{id}"},
				{Type: "update", Role: "keys"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "group:{id}"},
				{Type: "delete", Role: "keys"},
				{Type: "delete", Role: "admin"},
			},
		},
		"memberships": {
			name: "memberships",
			attrs: []databases.Attribute{
				{ID: "memberships_group_id", Key: "group_id", Type: "string", Size: 64, Required: true},
				{ID: "memberships_user_id", Key: "user_id", Type: "string", Size: 64},
				{ID: "memberships_email", Key: "email", Type: "email", Size: 320},
				{ID: "memberships_name", Key: "name", Type: "string", Size: 256},
				{ID: "memberships_roles", Key: "roles", Type: "json"},
				{ID: "memberships_status", Key: "status", Type: "string", Size: 32, Default: "pending"},
				{ID: "memberships_invited_at", Key: "invited_at", Type: "datetime"},
				{ID: "memberships_joined_at", Key: "joined_at", Type: "datetime"},
			},
			indexes: []databases.Index{
				{ID: "memberships_group_id", Type: "key", Attributes: []string{"group_id"}},
				{ID: "memberships_user_id", Type: "key", Attributes: []string{"user_id"}},
				{ID: "memberships_email", Type: "key", Attributes: []string{"email"}},
			},
			permissions: []databases.Permission{
				{Type: "create", Role: "users"},
				{Type: "create", Role: "keys"},
				{Type: "create", Role: "admin"},
				{Type: "read", Role: "user:{id}"},
				{Type: "read", Role: "group:{id}"},
				{Type: "read", Role: "keys"},
				{Type: "read", Role: "admin"},
				{Type: "update", Role: "user:{id}"},
				{Type: "update", Role: "group:{id}"},
				{Type: "update", Role: "keys"},
				{Type: "update", Role: "admin"},
				{Type: "delete", Role: "user:{id}"},
				{Type: "delete", Role: "group:{id}"},
				{Type: "delete", Role: "keys"},
				{Type: "delete", Role: "admin"},
			},
		},
	}
}
