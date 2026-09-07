package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainevents "github.com/torchwooddev/torchwood/internal/domain/events"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	infraauth "github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/realtime"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
)

var _ CredentialValidator = (*infraauth.Validator)(nil)

// ---- fakes ----

type fakeValidator struct {
	mu        sync.Mutex
	principal *shared.Principal
	claims    *jwtparser.Claims
	err       error
	// projectAccess 控制 ValidateAdminProjectAccess 的结果。
	projectAccess bool
	validated     []string // 记录 ValidateAdminProjectAccess 时的 ProjectID
}

func (v *fakeValidator) Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	_, raw, err := shared.ParseAuthnRequest(req)
	if err != nil {
		return nil, err
	}
	return v.validate(raw)
}

func (v *fakeValidator) ValidateToken(ctx context.Context, token string) (*shared.Principal, error) {
	return v.validate(token)
}

func (v *fakeValidator) ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error) {
	return v.validate(raw)
}

func (v *fakeValidator) validate(raw string) (*shared.Principal, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return nil, v.err
	}
	if v.principal == nil {
		return nil, errUnauthenticated
	}
	out := *v.principal
	return &out, nil
}

func (v *fakeValidator) ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.validated = append(v.validated, principal.ProjectID)
	if !v.projectAccess {
		return errPermissionDenied
	}
	return nil
}

func (v *fakeValidator) ParseClaims(raw string) (*jwtparser.Claims, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.claims == nil {
		return nil, false
	}
	out := *v.claims
	return &out, true
}

var (
	errUnauthenticated  = &fakeErr{msg: "invalid or expired credential"}
	errPermissionDenied = &fakeErr{msg: "permission denied"}
)

// setErr 供测试在握手后注入验证失败（模拟撤销，M5 C4）。
func (v *fakeValidator) setErr(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.err = err
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

// fakeDocDB 是订阅校验用的 DocumentDB 桩。
type fakeDocDB struct {
	collections map[string]*databases.Collection // key db/coll
	documents   map[string]*databases.Document   // key db/coll/id
	docErr      map[string]error
}

func (f *fakeDocDB) GetDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, principal databases.Principal) (*databases.Document, error) {
	key := databaseID + "/" + collectionID + "/" + docID
	if err := f.docErr[key]; err != nil {
		return nil, err
	}
	if doc := f.documents[key]; doc != nil {
		return doc, nil
	}
	return nil, nil
}

func (f *fakeDocDB) GetCollection(ctx context.Context, projectID, databaseID, collectionID string) (*databases.Collection, error) {
	return f.collections[databaseID+"/"+collectionID], nil
}

// ---- 以下方法不参与订阅校验路径，保持桩实现 ----

func (f *fakeDocDB) CreateDatabase(ctx context.Context, projectID, id, name string) error { return nil }
func (f *fakeDocDB) GetDatabase(ctx context.Context, projectID, id string) (*databases.Database, error) {
	return nil, nil
}
func (f *fakeDocDB) ListDatabases(ctx context.Context, projectID string) ([]databases.Database, error) {
	return nil, nil
}
func (f *fakeDocDB) DeleteDatabase(ctx context.Context, projectID, id string) error { return nil }
func (f *fakeDocDB) CreateCollection(ctx context.Context, projectID, databaseID, collectionID, name string, attrs []databases.Attribute, idxs []databases.Index, perms []databases.Permission, documentSecurity bool) error {
	return nil
}
func (f *fakeDocDB) ListCollections(ctx context.Context, projectID, databaseID string, q databases.ListQuery) ([]databases.Collection, databases.ListMeta, error) {
	return nil, databases.ListMeta{}, nil
}
func (f *fakeDocDB) DeleteCollection(ctx context.Context, projectID, databaseID, collectionID string) error {
	return nil
}
func (f *fakeDocDB) UpdateCollection(ctx context.Context, projectID, databaseID, collectionID string, patch databases.CollectionPatch) error {
	return nil
}
func (f *fakeDocDB) CreateAttribute(ctx context.Context, projectID, databaseID, collectionID string, attr databases.Attribute) error {
	return nil
}
func (f *fakeDocDB) DeleteAttribute(ctx context.Context, projectID, databaseID, collectionID, key string) error {
	return nil
}
func (f *fakeDocDB) CreateIndex(ctx context.Context, projectID, databaseID, collectionID string, idx databases.Index) error {
	return nil
}
func (f *fakeDocDB) DeleteIndex(ctx context.Context, projectID, databaseID, collectionID, indexID string) error {
	return nil
}
func (f *fakeDocDB) CreateDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	return doc, nil
}
func (f *fakeDocDB) UpsertDocument(ctx context.Context, projectID, databaseID, collectionID string, doc databases.Document, conflictColumns []string, perms []databases.Permission, principal databases.Principal) (databases.Document, error) {
	return doc, nil
}
func (f *fakeDocDB) UpdateDocument(ctx context.Context, projectID, databaseID, collectionID string, update databases.DocumentUpdate, principal databases.Principal) (databases.Document, error) {
	return update.Document, nil
}
func (f *fakeDocDB) DeleteDocument(ctx context.Context, projectID, databaseID, collectionID, docID string, opts databases.DeleteOptions, principal databases.Principal) error {
	return nil
}
func (f *fakeDocDB) ListDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (*databases.DocumentList, error) {
	return &databases.DocumentList{}, nil
}
func (f *fakeDocDB) CountDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (int64, error) {
	return 0, nil
}
func (f *fakeDocDB) AggregateDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, aggs []databases.AggregateSpec, groupBy string, principal databases.Principal) ([]databases.AggregateGroup, error) {
	return nil, nil
}
func (f *fakeDocDB) BulkUpdateDocuments(ctx context.Context, projectID, databaseID, collectionID string, documentIDs []string, data map[string]any, perms []databases.Permission, principal databases.Principal) (int64, error) {
	return 0, nil
}
func (f *fakeDocDB) BulkDeleteDocuments(ctx context.Context, projectID, databaseID, collectionID string, documentIDs []string, principal databases.Principal) (int64, error) {
	return 0, nil
}
func (f *fakeDocDB) EnsureCatalog(ctx context.Context, projectID string) error { return nil }

func (*fakeDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*fakeDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	return nil, false, 0, nil
}

var _ databases.DocumentDB = (*fakeDocDB)(nil)

// ---- helpers ----

func testHandler(t *testing.T, validator *fakeValidator, docDB *fakeDocDB) (*Handler, *realtime.Hub, *httptest.Server) {
	t.Helper()
	cfg := &config.AppConfig{}
	hub := realtime.NewHub(nil)
	h, err := NewHandler(cfg, validator, docDB, hub, nil)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, hub, srv
}

// wsURL 把 httptest 地址转成 ws://。
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// closeHandshakeBody 排空并关闭 websocket 握手响应体（bodyclose）。
func closeHandshakeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), nil)
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, data))
}

// readFrame 读取一帧并解码（带超时）。
func readTestFrame(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	require.NoError(t, json.Unmarshal(data, v))
}

func hello(projectID, token string) map[string]any {
	m := map[string]any{"type": "hello", "project_id": projectID}
	if token != "" {
		m["access_token"] = token
	}
	return m
}

// endUserPrincipal 构造普通用户 principal。
func endUserPrincipal(projectID, userID string) *shared.Principal {
	return &shared.Principal{
		ActorID:        idgen.ID(userID),
		ActorKind:      shared.ActorKindEndUser,
		CredentialType: shared.CredentialTypeToken,
		ProjectID:      projectID,
		UserID:         userID,
		Roles:          []string{"users", "user:" + userID},
	}
}

func adminPrincipal(actorID string) *shared.Principal {
	return &shared.Principal{
		ActorID:         idgen.ID(actorID),
		ActorKind:       shared.ActorKindAdmin,
		CredentialType:  shared.CredentialTypeToken,
		IsPlatformAdmin: true,
		AdminID:         actorID,
		Roles:           []string{"admin", "console"},
	}
}

// ---- 握手 ----

func TestHandshake_EndUserJWTOK(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt-token"))

	var resp struct {
		Type         string `json:"type"`
		ConnectionID string `json:"connection_id"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "hello_ok", resp.Type)
	require.NotEmpty(t, resp.ConnectionID)
}

func TestHandshake_RejectsProjectMismatch(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("other-project", "jwt-token"))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

func TestHandshake_RejectsRefreshToken(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeRefresh},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("default", "refresh-token"))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

func TestHandshake_RejectsGuest(t *testing.T) {
	validator := &fakeValidator{}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("default", ""))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

func TestHandshake_RejectsAPIKeyHeader(t *testing.T) {
	validator := &fakeValidator{}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Api-Key": []string{"key-1"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("default", ""))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

func TestHandshake_RejectsMultipleCredentials(t *testing.T) {
	validator := &fakeValidator{}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_console=abc"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	// token + cookie 并存 → 关连接（错误帧后 close）。
	sendJSON(t, c, hello("default", "jwt-token"))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

// TestHandshake_RejectsEndUserSessionCookie：无效端用户会话 cookie 仍拒（B10 后有效 TORCHWOOD_session_<project> 与 JWT 二选一可握手）。
func TestHandshake_RejectsEndUserSessionCookie(t *testing.T) {
	validator := &fakeValidator{}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_proj-x=abc"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("proj-x", ""))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

// TestHandshake_AcceptsEndUserSessionCookie：有效端用户会话 cookie（TORCHWOOD_session_<project>）可单独握手（B10）。
func TestHandshake_AcceptsEndUserSessionCookie(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("proj-x", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess, ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_proj-x=valid-cookie"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("proj-x", ""))
	var resp struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "hello_ok", resp.Type)
}

// TestHandshake_AdminBindsProjectBeforeAccessCheck：admin 必须先绑
// ProjectID 再 ValidateAdminProjectAccess（校验记录里必须带 hello.project_id）。
func TestHandshake_AdminBindsProjectBeforeAccessCheck(t *testing.T) {
	validator := &fakeValidator{
		principal:     adminPrincipal("a1"),
		claims:        &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
		projectAccess: true,
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_console=console-session"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })

	sendJSON(t, c, hello("proj-x", ""))

	var resp struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "hello_ok", resp.Type)
	validator.mu.Lock()
	require.Equal(t, []string{"proj-x"}, validator.validated)
	validator.mu.Unlock()
}

func TestHandshake_AdminWithoutProjectAccessRejected(t *testing.T) {
	validator := &fakeValidator{
		principal: adminPrincipal("a1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_console=console-session"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("proj-x", ""))
	expectErrorFrame(t, c, errCodeUnauthenticated)
}

func expectErrorFrame(t *testing.T, c *websocket.Conn, code string) {
	t.Helper()
	var resp struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "error", resp.Type)
	require.Equal(t, code, resp.Code)
}

// ---- 订阅 ----

func setupCollection(docDB *fakeDocDB, dbID, collID string, disabled, system bool) {
	docDB.collections[dbID+"/"+collID] = &databases.Collection{
		ID:               collID,
		DatabaseID:       dbID,
		ProjectID:        "default",
		DocumentSecurity: true,
		Disabled:         disabled,
		IsSystem:         system,
		Permissions:      []databases.Permission{{Type: "read", Role: "any"}},
	}
}

// TestSubscribe_CollectionChannel：正常集合订阅成功；系统/停用/不存在
// 一律 NOT_FOUND（防枚举）。
func TestSubscribe_CollectionChannel(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	docDB := &fakeDocDB{collections: map[string]*databases.Collection{}}
	setupCollection(docDB, "app", "posts", false, false)
	setupCollection(docDB, "app", "sys", false, true)
	setupCollection(docDB, "app", "disabled", true, false)

	_, _, srv := testHandler(t, validator, docDB)
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "c1", "channel": "databases.app.collections.posts"})
	var ok struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Channel string `json:"channel"`
	}
	readTestFrame(t, c, &ok)
	require.Equal(t, "subscribed", ok.Type)
	require.Equal(t, "c1", ok.ID)

	for _, ch := range []string{
		"databases.app.collections.sys",
		"databases.app.collections.disabled",
		"databases.app.collections.nope",
	} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "id": "c2", "channel": ch})
		expectErrorFrame(t, c, errCodeNotFound)
	}
}

// TestSubscribe_DocumentChannelRequiresRead：文档存在但无 read 权 →
// NOT_FOUND。
func TestSubscribe_DocumentChannelRequiresRead(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	docDB := &fakeDocDB{
		collections: map[string]*databases.Collection{},
		documents: map[string]*databases.Document{
			"app/posts/p1": {ID: "p1"},
		},
		docErr: map[string]error{
			"app/posts/p2": errPermissionDenied,
		},
	}
	_, _, srv := testHandler(t, validator, docDB)
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts.documents.p1"})
	var ok struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	readTestFrame(t, c, &ok)
	require.Equal(t, "subscribed", ok.Type)

	for _, ch := range []string{
		"databases.app.collections.posts.documents.p2", // 无 read
		"databases.app.collections.posts.documents.p3", // 不存在
	} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s2", "channel": ch})
		expectErrorFrame(t, c, errCodeNotFound)
	}
}

// TestSubscribe_InvalidChannel：格式/标识符非法 → INVALID_ARGUMENT。
func TestSubscribe_InvalidChannel(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	for _, ch := range []string{
		"databases..collections.x",
		"chat.room.one",                             // 非 databases 前缀
		"databases.app.foos.posts",                  // 缺 collections 段
		"databases.app.collections.posts.documents", // 缺 doc id
		"databases.app.collections.px.y",            // coll 含点 → 越权防护
		"accounts",                                  // 缺 userId
		"accounts.",                                 // 空 userId
		"accounts.u1.extra",                         // 多段
		"accounts.bad user",                         // 非法 userId
	} {
		sendJSON(t, c, map[string]any{"type": "subscribe", "id": "x", "channel": ch})
		expectErrorFrame(t, c, errCodeInvalidArgument)
	}
}

// TestSubscribe_QuotaExceeded：第 33 个订阅被拒（RESOURCE_EXHAUSTED），
// 连接保持（后续 subscribe 仍可成功）。
func TestSubscribe_QuotaExceeded(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	docDB := &fakeDocDB{collections: map[string]*databases.Collection{}}
	for i := 0; i < 40; i++ {
		setupCollection(docDB, "app", fmt.Sprintf("posts%d", i), false, false)
	}
	_, _, srv := testHandler(t, validator, docDB)
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	for i := 0; i < 32; i++ {
		sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s", "channel": fmt.Sprintf("databases.app.collections.posts%d", i)})
		readTestFrame(t, c, &struct {
			Type string `json:"type"`
		}{})
	}
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s", "channel": "databases.app.collections.posts32"})
	expectErrorFrame(t, c, errCodeResourceExhausted)

	// 连接保持：unsubscribe 后仍可再订。
	sendJSON(t, c, map[string]any{"type": "unsubscribe", "id": "s", "channel": "databases.app.collections.posts0"})
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s", "channel": "databases.app.collections.posts33"})
	var ok struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &ok)
	require.Equal(t, "subscribed", ok.Type)
}

// TestConnection_QuotaExceeded：同一用户第 5 条连接被拒（RESOURCE_EXHAUSTED）。
func TestConnection_QuotaExceeded(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	var conns []*websocket.Conn
	for i := 0; i < 4; i++ {
		c := dial(t, srv)
		conns = append(conns, c)
		sendJSON(t, c, hello("default", "jwt"))
		discardHelloOK(t, c)
	}
	c5 := dial(t, srv)
	sendJSON(t, c5, hello("default", "jwt"))
	expectErrorFrame(t, c5, errCodeResourceExhausted)

	// 关闭一条后新连接恢复。
	_ = conns[0].CloseNow()
	require.Eventually(t, func() bool {
		c6 := dial(t, srv)
		defer func() { _ = c6.CloseNow() }()
		sendJSON(t, c6, hello("default", "jwt"))
		var resp struct {
			Type string `json:"type"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		typ, data, err := c6.Read(ctx)
		if err != nil || typ != websocket.MessageText {
			return false
		}
		_ = json.Unmarshal(data, &resp)
		return resp.Type == "hello_ok"
	}, 5*time.Second, 100*time.Millisecond)
}

// ---- 事件投递 ----

// TestEventDelivery_FilteredByPermsAndNoACL：非 admin 按 _perms 收到事件，
// 出站帧无 acl；admin 全收。
func TestEventDelivery_FilteredByPermsAndNoACL(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	docDB := &fakeDocDB{collections: map[string]*databases.Collection{}}
	setupCollection(docDB, "app", "posts", false, false)
	_, hub, srv := testHandler(t, validator, docDB)

	u1 := dial(t, srv)
	sendJSON(t, u1, hello("default", "jwt"))
	discardHelloOK(t, u1)
	sendJSON(t, u1, map[string]any{"type": "subscribe", "id": "s", "channel": "databases.app.collections.posts"})
	readTestFrame(t, u1, &struct {
		Type string `json:"type"`
	}{})

	hub.Dispatch(eventsEnvelope("ev-1"))

	var ev struct {
		Type    string         `json:"type"`
		Channel string         `json:"channel"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, u1, &ev)
	require.Equal(t, "event", ev.Type)
	require.Equal(t, "databases.app.collections.posts", ev.Channel)
	require.Equal(t, "ev-1", ev.Payload["event_id"])
	raw, _ := json.Marshal(ev)
	require.NotContains(t, string(raw), "acl")
	require.NotContains(t, string(raw), "collection_permissions")

	// 无 read 权限的用户订同一频道收不到事件。
	validator.mu.Lock()
	validator.principal = endUserPrincipal("default", "u2")
	validator.mu.Unlock()
	u2 := dial(t, srv)
	sendJSON(t, u2, hello("default", "jwt"))
	discardHelloOK(t, u2)
	sendJSON(t, u2, map[string]any{"type": "subscribe", "id": "s", "channel": "databases.app.collections.posts"})
	readTestFrame(t, u2, &struct {
		Type string `json:"type"`
	}{})

	hub.Dispatch(eventsEnvelope("ev-2"))
	readTestFrame(t, u1, &struct {
		Type string `json:"type"`
	}{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	typ, _, err := u2.Read(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "u2 无 read 权不得收到事件 (typ=%v)", typ)
}

// TestEventDelivery_PlatformAdminBypass：platform admin 旁路 _perms。
func TestEventDelivery_PlatformAdminBypass(t *testing.T) {
	validator := &fakeValidator{
		principal:     adminPrincipal("a1"),
		claims:        &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
		projectAccess: true,
	}
	docDB := &fakeDocDB{collections: map[string]*databases.Collection{}}
	setupCollection(docDB, "app", "posts", false, false)
	_, hub, srv := testHandler(t, validator, docDB)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_console=console-session"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("default", ""))
	discardHelloOK(t, c)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s", "channel": "databases.app.collections.posts"})
	readTestFrame(t, c, &struct {
		Type string `json:"type"`
	}{})

	hub.Dispatch(eventsEnvelope("ev-admin"))
	var ev struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, c, &ev)
	require.Equal(t, "ev-admin", ev.Payload["event_id"])
}

// ---- JWT 到期断开 ----

func TestHandshake_TokenExpiryDisconnects(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess, ExpiresAt: time.Now().Add(2 * time.Second).Unix()},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	// 到期后服务端以策略违规关闭（原因 token_expired）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "token_expired"), "got: %v", err)
}

// TestRevalidation_RevokedCredentialDisconnects（M5 C4）：握手后撤销凭证
// （validator 转为拒绝），周期复验在 min(距 exp 剩余/2, 5min) 内发现并以
// 策略违规 + 原因 credential_revoked 主动断连——不再等 JWT 到期。
func TestRevalidation_RevokedCredentialDisconnects(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess, ExpiresAt: time.Now().Add(8 * time.Second).Unix()},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	// 握手后撤销：下一次复验（剩余/2 ≈ 4s）即收连接。
	validator.setErr(errUnauthenticated)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		require.Error(t, err)
		if strings.Contains(err.Error(), "credential_revoked") {
			return
		}
		if strings.Contains(err.Error(), "token_expired") {
			t.Fatalf("应在 token 到期前由复验断连: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("复验未在期限内断连: %v", err)
		}
	}
}

func discardHelloOK(t *testing.T, c *websocket.Conn) {
	t.Helper()
	var resp struct {
		Type string `json:"type"`
	}
	readTestFrame(t, c, &resp)
	require.Equal(t, "hello_ok", resp.Type)
}

// TestConnection_KeepAliveBeyond60Seconds：连接保持 > 60s 只靠 ping 续命。
// 用与 gateway 相同的 http.Server 超时配置（ReadTimeout/WriteTimeout=0、
// ReadHeaderTimeout=10s；回归 WithTimeout(60s) 会让 ReadTimeout 打在
// hijack 后的 conn 上，约 60s 断开）。
func TestConnection_KeepAliveBeyond60Seconds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow keep-alive test")
	}
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})
	handler := srv.Config.Handler

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
	}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		_ = httpSrv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, "ws://"+ln.Addr().String(), nil)
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	// 只靠 ping 续命：回 pong，保持 70s 不断开。
	// 读窗口 35s > ping 间隔 30s：正常路径每次读到的是服务端 ping。
	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithTimeout(context.Background(), 35*time.Second)
		typ, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			require.FailNow(t, "连接在 70s 内被断开: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var f struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "ping" {
			sendJSON(t, c, map[string]any{"type": "pong"})
		}
	}
}

// TestSubscribe_AccountsChannelSelfOK：本人可订 accounts.{uid}。
func TestSubscribe_AccountsChannelSelfOK(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "a1", "channel": "accounts.u1"})
	var ok struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Channel string `json:"channel"`
	}
	readTestFrame(t, c, &ok)
	require.Equal(t, "subscribed", ok.Type)
	require.Equal(t, "accounts.u1", ok.Channel)
}

// TestSubscribe_AccountsChannelOtherRejected：他人 uid 订阅拒（NOT_FOUND，防枚举）。
func TestSubscribe_AccountsChannelOtherRejected(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})
	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)

	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "a2", "channel": "accounts.u2"})
	expectErrorFrame(t, c, errCodeNotFound)
}

// TestSubscribe_AccountsChannelPlatformAdminOK：platform admin 可订任意 uid。
func TestSubscribe_AccountsChannelPlatformAdminOK(t *testing.T) {
	validator := &fakeValidator{
		principal:     adminPrincipal("a1"),
		claims:        &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
		projectAccess: true,
	}
	_, _, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, hsResp, err := websocket.Dial(ctx, wsURL(srv), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{"TORCHWOOD_session_console=console-session"}},
	})
	require.NoError(t, err)
	closeHandshakeBody(t, hsResp)
	t.Cleanup(func() { _ = c.CloseNow() })
	sendJSON(t, c, hello("default", ""))
	discardHelloOK(t, c)

	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "a3", "channel": "accounts.u9"})
	var ok struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
	}
	readTestFrame(t, c, &ok)
	require.Equal(t, "subscribed", ok.Type)
	require.Equal(t, "accounts.u9", ok.Channel)
}

// TestEventDelivery_AccountsChannelDomainSplit：经济事件按频道天然过滤、
// 出站无 acl，payload 含 domain 供客户端分流。
func TestEventDelivery_AccountsChannelDomainSplit(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	_, hub, srv := testHandler(t, validator, &fakeDocDB{collections: map[string]*databases.Collection{}})

	u1 := dial(t, srv)
	sendJSON(t, u1, hello("default", "jwt"))
	discardHelloOK(t, u1)
	sendJSON(t, u1, map[string]any{"type": "subscribe", "id": "s", "channel": "accounts.u1"})
	readTestFrame(t, u1, &struct {
		Type string `json:"type"`
	}{})

	hub.Dispatch(domainevents.Envelope{
		EventID:   "pay-1",
		Event:     "payments.orders.paid",
		ProjectID: "default",
		Domain:    "payments",
		Channel:   "accounts.u1",
		CreatedAt: time.Now(),
		Attrs:     map[string]any{"order_id": "o1", "amount": int64(1999)},
	})
	var pay struct {
		Type    string         `json:"type"`
		Channel string         `json:"channel"`
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, u1, &pay)
	require.Equal(t, "event", pay.Type)
	require.Equal(t, "accounts.u1", pay.Channel)
	require.Equal(t, "payments", pay.Payload["domain"])
	require.Equal(t, "payments.orders.paid", pay.Payload["event"])
	require.Equal(t, "o1", pay.Payload["order_id"])
	raw, _ := json.Marshal(pay)
	require.NotContains(t, string(raw), `"acl"`)

	hub.Dispatch(domainevents.Envelope{
		EventID:   "eco-1",
		Event:     "economy.assets.granted",
		ProjectID: "default",
		Domain:    "economy",
		Channel:   "accounts.u1",
		CreatedAt: time.Now(),
		Attrs:     map[string]any{"def_code": "gold", "delta": int64(100)},
	})
	var eco struct {
		Payload map[string]any `json:"payload"`
	}
	readTestFrame(t, u1, &eco)
	require.Equal(t, "economy", eco.Payload["domain"])
	require.Equal(t, "economy.assets.granted", eco.Payload["event"])
	require.Equal(t, "gold", eco.Payload["def_code"])

	// 他人频道事件不得误投。
	hub.Dispatch(domainevents.Envelope{
		EventID:   "pay-other",
		Event:     "payments.orders.paid",
		ProjectID: "default",
		Domain:    "payments",
		Channel:   "accounts.u2",
		CreatedAt: time.Now(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := u1.Read(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestParseChannelDispatcher(t *testing.T) {
	db, ok := parseChannel("databases.app.collections.posts")
	require.True(t, ok)
	require.Equal(t, channelKindDatabases, db.kind)
	require.Equal(t, "app", db.dbID)
	require.Equal(t, "posts", db.collID)
	require.Empty(t, db.docID)

	acc, ok := parseChannel("accounts.01HZXUSERID00000000000000")
	require.True(t, ok)
	require.Equal(t, channelKindAccounts, acc.kind)
	require.Equal(t, "01HZXUSERID00000000000000", acc.userID)

	_, ok = parseChannel("chat.room.one")
	require.False(t, ok)
	_, ok = parseChannel("accounts.u1.extra")
	require.False(t, ok)
	_, ok = parseChannel("databases.my_app.collections.posts")
	require.False(t, ok)
}

// eventsEnvelope 构造 u1 可读、u2 不可读的事件（doc perms user:u1）。
func eventsEnvelope(eventID string) domainevents.Envelope {
	return domainevents.Envelope{
		EventID:      eventID,
		Event:        domainevents.EventDocumentsUpdate,
		ProjectID:    "default",
		DatabaseID:   "app",
		CollectionID: "posts",
		DocumentID:   "p1",
		Version:      2,
		CreatedAt:    time.Now(),
		ACL: domainevents.ACLSnapshot{
			DocumentSecurity: true,
			CollectionPermissions: []databases.Permission{
				{Type: "read", Role: "any"},
			},
			DocumentPermissions: []databases.Permission{
				{Type: "read", Role: "user:u1"},
			},
			DocHasPerms: true,
		},
	}
}
