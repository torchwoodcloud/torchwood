package realtime

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/realtime"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
)

// errInternalProbe 是探测路径的内部错误桩（非权限类 → fail-closed）。
var errInternalProbe = &fakeErr{msg: "probe backend failure"}

// aclProbeDocDB 在 fakeDocDB 之上覆写 ListDocuments，模拟 adapter 层
// listPermissionFilter 的 ListAccessDenied 行为（Round4 J5-5）：集合级无
// read 且 !documentSecurity 的集合，REST List 会直接 PermissionDenied。
type aclProbeDocDB struct {
	*fakeDocDB
	denyRead bool
}

func (f *aclProbeDocDB) ListDocuments(_ context.Context, _, _, _ string, _ databases.Query, _ databases.Principal) (*databases.DocumentList, error) {
	if f.denyRead {
		return nil, databases.ErrPermissionDenied
	}
	return &databases.DocumentList{}, nil
}

func (*aclProbeDocDB) ExecuteTransactions(context.Context, string, string, []databases.TransactionOp, databases.TransactionMode, databases.Principal) ([]databases.TransactionOpResult, error) {
	panic("ExecuteTransactions: not implemented in test fake")
}

func (*aclProbeDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	panic("ListChanges: not implemented in test fake")
}

var _ databases.DocumentDB = (*aclProbeDocDB)(nil)

// TestSubscribe_CollectionChannelRequiresRead：Round4 J5-5——集合频道订阅
// 必须通过 REST List 同款 read 门：
//   - adapter 探测返回 PermissionDenied → 拒订（NOT_FOUND，防枚举）；
//   - 有 read 权限（探测通过）→ 放行；
//   - platform admin（特权旁路文档 ACL，与 REST ensureReadableCollection
//     的旁路口径一致）→ 不探测直接放行。
func TestSubscribe_CollectionChannelRequiresRead(t *testing.T) {
	newServer := func(t *testing.T, principal *shared.Principal, denyRead bool) *httptest.Server {
		t.Helper()
		validator := &fakeValidator{
			principal:     principal,
			claims:        &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
			projectAccess: true, // admin 握手要过 ValidateAdminProjectAccess
		}
		docDB := &aclProbeDocDB{
			fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}},
			denyRead:  denyRead,
		}
		setupCollection(docDB.fakeDocDB, "app", "posts", false, false)
		hub := realtime.NewHub(nil)
		handler, err := NewHandler(&config.AppConfig{}, validator, docDB, hub, nil, nil)
		require.NoError(t, err)
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		return srv
	}

	subscribe := func(t *testing.T, srv *httptest.Server) string {
		t.Helper()
		c := dial(t, srv)
		sendJSON(t, c, hello("default", "jwt"))
		discardHelloOK(t, c)
		sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts"})
		var resp struct {
			Type string `json:"type"`
			Code string `json:"code"`
		}
		readTestFrame(t, c, &resp)
		if resp.Type == "error" {
			return resp.Code
		}
		return resp.Type
	}

	t.Run("denied without read permission", func(t *testing.T) {
		srv := newServer(t, endUserPrincipal("default", "u1"), true)
		require.Equal(t, errCodeNotFound, subscribe(t, srv), "无 read 权限不得订阅集合频道")
	})

	t.Run("allowed with read permission", func(t *testing.T) {
		srv := newServer(t, endUserPrincipal("default", "u1"), false)
		require.Equal(t, "subscribed", subscribe(t, srv))
	})

	t.Run("platform admin bypasses probe", func(t *testing.T) {
		srv := newServer(t, adminPrincipal("adm-1"), true)
		require.Equal(t, "subscribed", subscribe(t, srv), "特权主体与 REST List 同口径旁路")
	})
}

// TestSubscribe_CollectionChannelProbeFailClosed：探测返回非权限类错误时
// fail-closed 拒订并留日志。
func TestSubscribe_CollectionChannelProbeFailClosed(t *testing.T) {
	validator := &fakeValidator{
		principal: endUserPrincipal("default", "u1"),
		claims:    &jwtparser.Claims{TokenType: jwtparser.TokenTypeAccess},
	}
	docDB := &failingProbeDocDB{
		aclProbeDocDB: &aclProbeDocDB{
			fakeDocDB: &fakeDocDB{collections: map[string]*databases.Collection{}},
		},
	}
	setupCollection(docDB.fakeDocDB, "app", "posts", false, false)
	hub := realtime.NewHub(nil)
	handler, err := NewHandler(&config.AppConfig{}, validator, docDB, hub, nil, nil)
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := dial(t, srv)
	sendJSON(t, c, hello("default", "jwt"))
	discardHelloOK(t, c)
	sendJSON(t, c, map[string]any{"type": "subscribe", "id": "s1", "channel": "databases.app.collections.posts"})
	expectErrorFrame(t, c, errCodeNotFound)
}

// failingProbeDocDB 让 ListDocuments 返回内部错误（非权限类）。
type failingProbeDocDB struct {
	*aclProbeDocDB
}

func (f *failingProbeDocDB) ListDocuments(ctx context.Context, projectID, databaseID, collectionID string, q databases.Query, principal databases.Principal) (*databases.DocumentList, error) {
	return nil, errInternalProbe
}
