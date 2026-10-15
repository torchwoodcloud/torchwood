package serverhttp

// 本文件是从 functions_handler_test.go 抢救出的共享测试基建（v4：multipart
// 部署上传 handler 退役，validator/docdb/签名等桩仍被 auth_test、file/auth
// 测试共用——functions 前缀是历史命名，保持不动以免牵连其它引用）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

const functionsTestJWTSecret = "functions-handler-test-secret"

func functionsTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{Jwt: &config.Security_Jwt{Secret: functionsTestJWTSecret}},
	}
}

type functionsAPIKeyRepo struct {
	keys map[string]*projects.APIKey
}

func (r *functionsAPIKeyRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *functionsAPIKeyRepo) GetAPIKey(context.Context, string, string) (*projects.APIKey, error) {
	return nil, nil
}
func (r *functionsAPIKeyRepo) GetAPIKeyBySecretHash(_ context.Context, hash string) (*projects.APIKey, error) {
	return r.keys[hash], nil
}
func (r *functionsAPIKeyRepo) ListAPIKeys(context.Context, string, int, int, projects.APIKeyListFilter) ([]projects.APIKey, int, error) {
	return nil, 0, nil
}
func (r *functionsAPIKeyRepo) UpdateAPIKey(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *functionsAPIKeyRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

type functionsAdminRepo struct {
	admins map[string]*projects.Admin
}

func (r *functionsAdminRepo) GetAdmin(_ context.Context, id string) (*projects.Admin, error) {
	return r.admins[id], nil
}
func (r *functionsAdminRepo) RevokeCredentials(context.Context, string, time.Time) error {
	return nil
}
func (r *functionsAdminRepo) GetAdminByEmail(context.Context, string) (*projects.Admin, error) {
	return nil, nil
}
func (r *functionsAdminRepo) ListAdmins(context.Context) ([]projects.Admin, error) { return nil, nil }
func (r *functionsAdminRepo) CreateAdmin(context.Context, *projects.Admin) error   { return nil }
func (r *functionsAdminRepo) UpdateAdmin(context.Context, *projects.Admin) error   { return nil }
func (r *functionsAdminRepo) DeleteAdmin(context.Context, string) error            { return nil }
func (r *functionsAdminRepo) CountAdminsByRole(context.Context, string) (int64, error) {
	return 1, nil
}
func (r *functionsAdminRepo) UpdateAdminMetadata(context.Context, string, map[string]string, []string, time.Time) error {
	return nil
}
func (r *functionsAdminRepo) WithBootstrapLock(_ context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

type functionsAdminProjectRepo struct{}

func (functionsAdminProjectRepo) HasProjectAccess(context.Context, string, string) (bool, error) {
	return true, nil
}
func (functionsAdminProjectRepo) GrantProjectAccess(context.Context, string, string) error {
	return nil
}
func (functionsAdminProjectRepo) ListProjectIDs(context.Context, string) ([]string, error) {
	return nil, nil
}

type functionsDocDB struct {
	users map[string]map[string]map[string]any
}

func (d *functionsDocDB) GetDocument(_ context.Context, projectID, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	if collectionID == "users" {
		if data, ok := d.users[projectID][docID]; ok {
			return &databases.Document{ID: docID, Data: data}, nil
		}
	}
	return nil, nil
}
func (d *functionsDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (d *functionsDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (d *functionsDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (d *functionsDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (d *functionsDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (d *functionsDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (d *functionsDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (d *functionsDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (d *functionsDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (d *functionsDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (d *functionsDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (d *functionsDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (d *functionsDocDB) DeleteIndex(context.Context, string, string, string, string) error {
	return nil
}
func (d *functionsDocDB) CreateDocument(context.Context, string, string, string, databases.Document, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) UpdateDocument(context.Context, string, string, string, databases.DocumentUpdate, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *functionsDocDB) DeleteDocument(context.Context, string, string, string, string, databases.DeleteOptions, databases.Principal) error {
	return nil
}
func (d *functionsDocDB) ListDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (*databases.DocumentList, error) {
	return nil, nil
}
func (d *functionsDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (d *functionsDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) BulkDeleteDocuments(context.Context, string, string, string, []string, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *functionsDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (*functionsDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*functionsDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*functionsDocDB)(nil)

func functionsHashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newFunctionsValidator(docDB *functionsDocDB) *auth.Validator {
	users := &functionsUserRepo{users: map[string]*domainusers.User{}}
	if docDB != nil {
		for _, byUser := range docDB.users {
			for id := range byUser {
				users.users[id] = &domainusers.User{ID: id, Status: domainusers.StatusActive}
			}
		}
	}
	return auth.NewValidator(functionsTestConfig(), &functionsAPIKeyRepo{}, nil, &functionsAdminRepo{}, &functionsAdminProjectRepo{}, nil, nil, users, nil)
}

type functionsUserRepo struct {
	users map[string]*domainusers.User
}

func (r *functionsUserRepo) GetByEmail(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *functionsUserRepo) GetByID(_ context.Context, _, id string) (*domainusers.User, error) {
	if r.users == nil {
		return nil, nil
	}
	u := r.users[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (r *functionsUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *functionsUserRepo) Insert(context.Context, string, *domainusers.User) error { return nil }
func (r *functionsUserRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *functionsUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *functionsUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *functionsUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}
