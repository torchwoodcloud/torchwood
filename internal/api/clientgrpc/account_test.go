package clientgrpc

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/messaging"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	inframessaging "github.com/torchwoodcloud/torchwood/internal/infra/messaging"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// fakeDocDB 是内存版 DocumentDB（仅实现 account 测试路径所需语义：
// users/sessions 集合的 List/Get/Create/Delete/BulkDelete）。
type fakeDocDB struct {
	users       map[string]map[string]map[string]any // projectID -> userID -> data
	sessions    map[string]map[string]map[string]any // projectID -> sessionID -> data
	usersRepo   *grpcMemUserRepo
	sessionRepo *grpcMemSessionRepo
}

func (d *fakeDocDB) listByQuery(col string, projectID string, q databases.Query) []databases.Document {
	parsed := q.AST
	if parsed == nil {
		var err error
		parsed, err = query.ParseMany(q.Queries)
		if err != nil {
			return nil
		}
	}
	src := d.users
	if col == "sessions" {
		src = d.sessions
	}
	var docs []databases.Document
	for id, data := range src[projectID] {
		match := true
		parsed.WalkLeaves(func(f query.Filter) {
			if f.Op == query.OpEqual && len(f.Values) > 0 && data[f.Attribute] != f.Values[0] {
				match = false
			}
		})
		if match {
			docs = append(docs, databases.Document{ID: id, Data: data})
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	if parsed.Limit > 0 && len(docs) > parsed.Limit {
		docs = docs[:parsed.Limit]
	}
	return docs
}

func (d *fakeDocDB) CreateDocument(_ context.Context, projectID, _, collectionID string, doc databases.Document, _ []databases.Permission, _ databases.Principal) (databases.Document, error) {
	if d.users == nil {
		d.users = map[string]map[string]map[string]any{}
	}
	if d.sessions == nil {
		d.sessions = map[string]map[string]map[string]any{}
	}
	switch collectionID {
	case "users":
		if d.users[projectID] == nil {
			d.users[projectID] = map[string]map[string]any{}
		}
		d.users[projectID][doc.ID] = doc.Data
	case "sessions":
		if d.sessions[projectID] == nil {
			d.sessions[projectID] = map[string]map[string]any{}
		}
		d.sessions[projectID][doc.ID] = doc.Data
	}
	return doc, nil
}

func (d *fakeDocDB) GetDocument(_ context.Context, projectID, _, collectionID, docID string, _ databases.Principal) (*databases.Document, error) {
	src := d.users
	if collectionID == "sessions" {
		src = d.sessions
	}
	if src == nil || src[projectID] == nil {
		return nil, nil
	}
	if data, ok := src[projectID][docID]; ok {
		return &databases.Document{ID: docID, Data: data}, nil
	}
	return nil, nil
}

func (d *fakeDocDB) DeleteDocument(_ context.Context, projectID, _, collectionID, docID string, _ databases.DeleteOptions, _ databases.Principal) error {
	src := d.users
	if collectionID == "sessions" {
		src = d.sessions
	}
	if src != nil && src[projectID] != nil {
		delete(src[projectID], docID)
	}
	return nil
}

func (d *fakeDocDB) ListDocuments(_ context.Context, projectID, _, collectionID string, q databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	return &databases.DocumentList{Documents: d.listByQuery(collectionID, projectID, q)}, nil
}

func (d *fakeDocDB) BulkDeleteDocuments(_ context.Context, projectID, _, collectionID string, ids []string, _ databases.Principal) (int64, error) {
	src := d.users
	if collectionID == "sessions" {
		src = d.sessions
	}
	var n int64
	if src != nil && src[projectID] != nil {
		for _, id := range ids {
			if _, ok := src[projectID][id]; ok {
				delete(src[projectID], id)
				n++
			}
		}
	}
	return n, nil
}

func (d *fakeDocDB) UpdateDocument(_ context.Context, projectID, _, collectionID string, update databases.DocumentUpdate, _ databases.Principal) (databases.Document, error) {
	src := d.users
	if collectionID == "sessions" {
		src = d.sessions
	}
	if src == nil || src[projectID] == nil || src[projectID][update.Document.ID] == nil {
		return databases.Document{}, nil
	}
	for k, v := range update.Document.Data {
		src[projectID][update.Document.ID][k] = v
	}
	return databases.Document{ID: update.Document.ID, Data: src[projectID][update.Document.ID]}, nil
}

func (d *fakeDocDB) CreateDatabase(context.Context, string, string, string) error { return nil }
func (d *fakeDocDB) GetDatabase(context.Context, string, string) (*databases.Database, error) {
	return nil, nil
}
func (d *fakeDocDB) ListDatabases(context.Context, string) ([]databases.Database, error) {
	return nil, nil
}
func (d *fakeDocDB) DeleteDatabase(context.Context, string, string) error { return nil }
func (d *fakeDocDB) CreateCollection(context.Context, string, string, string, string, []databases.Attribute, []databases.Index, []databases.Permission, bool) error {
	return nil
}
func (d *fakeDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return nil, nil
}
func (d *fakeDocDB) ListCollections(context.Context, string, string, databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (d *fakeDocDB) DeleteCollection(context.Context, string, string, string) error { return nil }
func (d *fakeDocDB) UpdateCollection(context.Context, string, string, string, databases.CollectionPatch) error {
	return nil
}
func (d *fakeDocDB) CreateAttribute(context.Context, string, string, string, databases.Attribute) error {
	return nil
}
func (d *fakeDocDB) DeleteAttribute(context.Context, string, string, string, string) error {
	return nil
}
func (d *fakeDocDB) CreateIndex(context.Context, string, string, string, databases.Index) error {
	return nil
}
func (d *fakeDocDB) DeleteIndex(context.Context, string, string, string, string) error { return nil }
func (d *fakeDocDB) UpsertDocument(context.Context, string, string, string, databases.Document, []string, []databases.Permission, databases.Principal) (databases.Document, error) {
	return databases.Document{}, nil
}
func (d *fakeDocDB) CountDocuments(context.Context, string, string, string, databases.Query, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *fakeDocDB) AggregateDocuments(context.Context, string, string, string, databases.Query, []databases.AggregateSpec, string, databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (d *fakeDocDB) BulkUpdateDocuments(context.Context, string, string, string, []string, map[string]any, []databases.Permission, databases.Principal) (int64, error) {
	return 0, nil
}
func (d *fakeDocDB) EnsureCatalog(context.Context, string) error { return nil }

func (*fakeDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*fakeDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*fakeDocDB)(nil)

// fakeProjectRepo 返回单个固定 project。
type fakeProjectRepo struct {
	project *projects.Project
}

func (r *fakeProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.project == nil || r.project.ID != id {
		return nil, nil
	}
	return r.project, nil
}
func (r *fakeProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (r *fakeProjectRepo) ListProjects(context.Context) ([]projects.Project, error) { return nil, nil }
func (r *fakeProjectRepo) CreateProject(context.Context, *projects.Project) error   { return nil }
func (r *fakeProjectRepo) UpdateProject(context.Context, *projects.Project) error   { return nil }
func (r *fakeProjectRepo) DeleteProject(context.Context, string) error              { return nil }

// fakeOAuthProviderRepo 空实现（SignUp/CreateMagicURLSession 不依赖）。
type fakeOAuthProviderRepo struct{}

func (fakeOAuthProviderRepo) GetOAuthProvider(context.Context, string, string) (*projects.OAuthProvider, error) {
	return nil, nil
}
func (fakeOAuthProviderRepo) ListOAuthProviders(context.Context, string) ([]projects.OAuthProvider, error) {
	return nil, nil
}
func (fakeOAuthProviderRepo) UpsertOAuthProvider(context.Context, *projects.OAuthProvider) error {
	return nil
}
func (fakeOAuthProviderRepo) DeleteOAuthProvider(context.Context, string, string) error { return nil }

// clientgrpcCaptureMailer 记录发送的邮件（含收件人）。
type clientgrpcCaptureMailer struct {
	to   []string
	body []string
}

func (m *clientgrpcCaptureMailer) Send(_ context.Context, to, _, body string) error {
	m.to = append(m.to, to)
	m.body = append(m.body, body)
	return nil
}

var _ messaging.Mailer = (*clientgrpcCaptureMailer)(nil)

type stubRoleResolver struct{}

func (stubRoleResolver) LoadUserRoles(_ context.Context, _, userID string, _ *users.User) ([]string, error) {
	return []string{"users", "user:" + userID}, nil
}

// setupClientGRPC 组装 handler 测试环境（纯内存 + miniredis，无需数据库）。
func setupClientGRPC(t *testing.T) (context.Context, *AccountService, *fakeDocDB, *clientgrpcCaptureMailer, string) {
	t.Helper()
	ctx := context.Background()
	cfg := &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "clientgrpc-test-secret"}, // #nosec G101 -- 测试固定值
		},
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	docDB := &fakeDocDB{}
	projectID := "proj-grpc-1"
	projectRepo := &fakeProjectRepo{project: &projects.Project{
		ID: projectID, InternalID: 1, Name: "Test Project", Status: "active",
	}}

	roles := stubRoleResolver{}
	rotation := auth.NewRedisRefreshRotationStore(rdb)
	sessionRepo := &grpcMemSessionRepo{byID: map[string]*domainauth.Session{}}
	usersRepo := &grpcMemUserRepo{byID: map[string]*domainusers.User{}, byEmail: map[string]*domainusers.User{}}
	docDB.usersRepo = usersRepo
	docDB.sessionRepo = sessionRepo
	sessions := auth.NewSessionService(cfg, sessionRepo, roles, rotation)
	mailer := &clientgrpcCaptureMailer{}
	account := client.NewAccount(
		cfg,
		projectRepo,
		nil, // inviteRepo
		fakeOAuthProviderRepo{},
		sessions,
		auth.NewRedisOTPChallengeStore(rdb, cfg),
		auth.NewRedisOAuthStateStore(rdb),
		auth.NewRedisAccountTokenStore(rdb),
		nil, // loginThrottle
		rotation,
		nil, // idGen
		mailer,
		inframessaging.NewSMSService(cfg),
		auth.NewRedisRateLimiter(rdb),
		roles,
		auth.NewTOTPService(cfg, rdb),
		auth.NewRedisMFAChallengeStore(rdb),
		auth.NewRedisOneTimeTokenStore(rdb),
		nil, // auditRepo
		usersRepo,
		nil,
		sessionRepo,
		auth.NewOAuthAuthenticatorFactory(),
		auth.NewWeChatMiniProgramExchanger(),
		auth.NewOTPGenerator(),
		nil,
	)
	return ctx, NewAccountService(account), docDB, mailer, projectID
}

func signUpViaHandler(t *testing.T, ctx context.Context, s *AccountService, projectID, email string) string {
	t.Helper()
	resp, err := s.SignUp(ctx, &clientv1.SignUpRequest{
		ProjectId: projectID,
		Email:     email,
		Password:  "User@123",
		Name:      "GRPC User",
	})
	require.NoError(t, err)
	return resp.Account.GetId()
}

func principalCtx(ctx context.Context, projectID, userID, sessionID string) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    userID,
		SessionID: sessionID,
		Email:     "user@torchwood.local",
		Roles:     []string{"users", "user:" + userID},
	})
}

// TestAccountService_CreateMagicURLSession_ResponseContainsOnlyChallengeID
// （R04-P1-1）：响应只含 challengeId（+expireAt），secret 仅存在于邮件中，
// 不得出现在响应里。
func TestAccountService_CreateMagicURLSession_ResponseContainsOnlyChallengeID(t *testing.T) {
	ctx, s, _, mailer, projectID := setupClientGRPC(t)
	userID := signUpViaHandler(t, ctx, s, projectID, "magic@example.com")

	authCtx := principalCtx(ctx, projectID, userID, "session-1")
	resp, err := s.CreateMagicURLSession(authCtx, &clientv1.CreateMagicURLSessionRequest{
		ProjectId: projectID,
		Email:     "magic@example.com",
		Url:       "http://localhost/verify",
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetChallengeId())
	require.NotNil(t, resp.GetExpireAt())

	// secret 只出现在邮件 body（userId=...&secret=...），响应 JSON 不得包含。
	re := regexp.MustCompile(`secret=([a-f0-9]+)`)
	require.Len(t, mailer.body, 1)
	matches := re.FindStringSubmatch(mailer.body[0])
	require.Len(t, matches, 2)
	raw, err := protojson.Marshal(resp)
	require.NoError(t, err)
	require.NotContains(t, string(raw), matches[1], "响应不得泄露一次性 secret")
}

// TestAccountService_DeleteSessions_KeepCurrentPassthrough（R04-P1-1）：
// keep_current 透传给 use-case——true 保留当前会话，false 全删。
func TestAccountService_DeleteSessions_KeepCurrentPassthrough(t *testing.T) {
	ctx, s, docDB, _, projectID := setupClientGRPC(t)
	userID := signUpViaHandler(t, ctx, s, projectID, "sessions@example.com")

	list, err := docDB.sessionRepo.ListByUser(ctx, projectID, userID)
	require.NoError(t, err)
	require.NotEmpty(t, list)
	currentSessionID := list[0].ID
	authCtx := principalCtx(ctx, projectID, userID, currentSessionID)

	require.NoError(t, docDB.sessionRepo.Insert(ctx, projectID, &domainauth.Session{
		ID:     "session-2",
		UserID: userID,
	}))

	_, err = s.DeleteSessions(authCtx, &clientv1.DeleteSessionsRequest{KeepCurrent: true})
	require.NoError(t, err)
	cur, err := docDB.sessionRepo.GetByID(ctx, projectID, currentSessionID)
	require.NoError(t, err)
	require.NotNil(t, cur)
	other, err := docDB.sessionRepo.GetByID(ctx, projectID, "session-2")
	require.NoError(t, err)
	require.Nil(t, other)

	_, err = s.DeleteSessions(authCtx, &clientv1.DeleteSessionsRequest{KeepCurrent: false})
	require.NoError(t, err)
	cur, err = docDB.sessionRepo.GetByID(ctx, projectID, currentSessionID)
	require.NoError(t, err)
	require.Nil(t, cur)
}

// TestAccountService_ConfirmEmailChange_Passthrough（R05-P1-2 A 档）：
// handler 薄透传 + staging 端到端（内存 fakeDocDB + miniredis，-short 可跑）：
// 改邮箱只写 pending_email → 消费邮件 secret 确认后 email 才切换。
func TestAccountService_ConfirmEmailChange_Passthrough(t *testing.T) {
	ctx, s, docDB, mailer, projectID := setupClientGRPC(t)
	userID := signUpViaHandler(t, ctx, s, projectID, "grpc-stage@torchwood.local")
	authCtx := principalCtx(ctx, projectID, userID, "session-1")

	// 未设置 url 改邮箱 → InvalidArgument（handler 透传 use-case 校验）。
	newEmail := "grpc-stage-new@torchwood.local"
	_, err := s.UpdateAccount(authCtx, &clientv1.UpdateAccountRequest{
		Email:       &newEmail,
		OldPassword: "User@123",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 免登录（ACCESS_PUBLIC）：不校验 principal，错误 secret → Unauthenticated
	//（token 校验是唯一凭证，与 recovery 同一安全模型）。
	_, err = s.ConfirmEmailChange(ctx, &clientv1.ConfirmEmailChangeRequest{
		ProjectId: projectID,
		UserId:    "other-user",
		Secret:    "whatever",
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = s.UpdateAccount(authCtx, &clientv1.UpdateAccountRequest{
		Email:       &newEmail,
		OldPassword: "User@123",
		Url:         "http://localhost/confirm-email",
	})
	require.NoError(t, err)
	// staging：email 保持旧值，仅写入 pending_email。
	staged, err := docDB.usersRepo.GetByID(ctx, projectID, userID)
	require.NoError(t, err)
	require.Equal(t, "grpc-stage@torchwood.local", staged.Email)
	require.Equal(t, newEmail, staged.PendingEmail)

	// 从新邮箱收到的验证邮件提取一次性 secret。
	re := regexp.MustCompile(`secret=([a-f0-9]+)`)
	matches := re.FindStringSubmatch(mailer.body[0])
	require.Len(t, matches, 2, "新邮箱验证邮件必须携带 secret")

	resp, err := s.ConfirmEmailChange(authCtx, &clientv1.ConfirmEmailChangeRequest{
		ProjectId: projectID,
		UserId:    userID,
		Secret:    matches[1],
	})
	require.NoError(t, err)
	require.Equal(t, newEmail, resp.GetEmail())
	require.True(t, resp.GetEmailVerified())
	confirmed, err := docDB.usersRepo.GetByID(ctx, projectID, userID)
	require.NoError(t, err)
	require.Empty(t, confirmed.PendingEmail)

	// token 一次性：二次使用 → Unauthenticated。
	_, err = s.ConfirmEmailChange(authCtx, &clientv1.ConfirmEmailChangeRequest{
		ProjectId: projectID,
		UserId:    userID,
		Secret:    matches[1],
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestAccountService_ErrorCodeMapping（R04-P1-1）：handler 错误码映射。
func TestAccountService_ErrorCodeMapping(t *testing.T) {
	ctx, s, _, _, projectID := setupClientGRPC(t)
	userID := signUpViaHandler(t, ctx, s, projectID, "errors@torchwood.local")
	authCtx := principalCtx(ctx, projectID, userID, "session-1")

	// DeleteSession 空 session_id → InvalidArgument（R04-P3-2）：形状校验
	// 已上收到 validate 拦截器的 protovalidate 注解，此处直调 handler 绕过
	// 拦截器，InvalidArgument 来自 use-case 层防线，不变量两层共同保持。
	_, err := s.DeleteSession(authCtx, &clientv1.DeleteSessionRequest{SessionId: ""})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 不存在的会话 → NotFound（use-case 透传）。
	_, err = s.DeleteSession(authCtx, &clientv1.DeleteSessionRequest{SessionId: "no-such-session"})
	require.Equal(t, codes.NotFound, status.Code(err))

	// 不存在的 project → NotFound。
	_, err = s.CreateMagicURLSession(authCtx, &clientv1.CreateMagicURLSessionRequest{
		ProjectId: "no-such-project",
		Email:     "errors@torchwood.local",
		Url:       "http://localhost/verify",
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	// 非法 URL → InvalidArgument（不依赖 user 存在，独立验证）。
	_, err = s.CreateMagicURLSession(authCtx, &clientv1.CreateMagicURLSessionRequest{
		ProjectId: projectID,
		Email:     "errors@torchwood.local",
		Url:       "javascript:alert(1)",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestAccountService_SignUp_DuplicateEmail（R04-P1-1）：重复邮箱注册 →
// AlreadyExists。
func TestAccountService_SignUp_DuplicateEmail(t *testing.T) {
	ctx, s, _, _, projectID := setupClientGRPC(t)
	signUpViaHandler(t, ctx, s, projectID, "errors@torchwood.local")

	_, err := s.SignUp(ctx, &clientv1.SignUpRequest{
		ProjectId: projectID,
		Email:     "errors@torchwood.local",
		Password:  "User@123",
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

// TestAccountService_SignOut_WithoutPrincipalIsIdempotent（R04-P2-4）：
// SignOut 不再重复 Principal 校验——无 principal 时幂等成功。
func TestAccountService_SignOut_WithoutPrincipalIsIdempotent(t *testing.T) {
	ctx, s, _, _, _ := setupClientGRPC(t)
	_, err := s.SignOut(ctx, &clientv1.SignOutRequest{})
	require.NoError(t, err)
}

type grpcMemUserRepo struct {
	mu      sync.Mutex
	byID    map[string]*domainusers.User
	byEmail map[string]*domainusers.User
}

func (r *grpcMemUserRepo) GetByEmail(_ context.Context, _, email string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.byEmail[domainusers.NormalizeEmail(email)]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (r *grpcMemUserRepo) GetByID(_ context.Context, _, id string) (*domainusers.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.byID[id]
	if u == nil {
		return nil, nil
	}
	cp := *u
	return &cp, nil
}
func (r *grpcMemUserRepo) GetByPhone(context.Context, string, string) (*domainusers.User, error) {
	return nil, nil
}
func (r *grpcMemUserRepo) Insert(_ context.Context, _ string, user *domainusers.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byEmail[user.Email] != nil {
		return domainusers.ErrEmailAlreadyRegistered
	}
	cp := *user
	r.byID[user.ID] = &cp
	r.byEmail[user.Email] = &cp
	return nil
}
func (r *grpcMemUserRepo) Update(_ context.Context, _, id string, cols map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.byID[id]
	if u == nil {
		return status.Error(codes.NotFound, "user not found")
	}
	if v, ok := cols["email"].(string); ok {
		delete(r.byEmail, u.Email)
		u.Email = domainusers.NormalizeEmail(v)
		r.byEmail[u.Email] = u
	}
	if v, ok := cols["name"].(string); ok {
		u.Name = v
	}
	if v, ok := cols["prefs"].(map[string]any); ok {
		u.Prefs = v
	}
	if v, ok := cols["pending_email"].(string); ok {
		u.PendingEmail = v
	}
	if v, ok := cols["email_verified"].(bool); ok {
		u.EmailVerified = v
	}
	return nil
}
func (r *grpcMemUserRepo) Delete(context.Context, string, string) error { return nil }
func (r *grpcMemUserRepo) List(context.Context, string, domainusers.ListFilter) (*domainusers.ListResult, error) {
	return &domainusers.ListResult{}, nil
}
func (r *grpcMemUserRepo) UpdateFactors(context.Context, string, string, func(json.RawMessage) (json.RawMessage, error)) error {
	return nil
}

type grpcMemSessionRepo struct {
	mu   sync.Mutex
	byID map[string]*domainauth.Session
}

func (r *grpcMemSessionRepo) Insert(_ context.Context, _ string, s *domainauth.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *s
	r.byID[s.ID] = &cp
	return nil
}
func (r *grpcMemSessionRepo) GetByID(_ context.Context, _, id string) (*domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}
func (r *grpcMemSessionRepo) ListByUser(_ context.Context, _, userID string) ([]domainauth.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domainauth.Session
	for _, s := range r.byID {
		if s.UserID == userID {
			out = append(out, *s)
		}
	}
	return out, nil
}
func (r *grpcMemSessionRepo) Delete(_ context.Context, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, id)
	return nil
}
func (r *grpcMemSessionRepo) DeleteByUser(_ context.Context, _, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.byID {
		if s.UserID == userID {
			delete(r.byID, id)
		}
	}
	return nil
}
func (r *grpcMemSessionRepo) DeleteOldestByUser(context.Context, string, string, int) error {
	return nil
}

func (r *fakeProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }
