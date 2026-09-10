package consolegrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/require"
	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/protobuf/encoding/protojson"
)

type bootstrapFixture struct {
	url         string
	projectRepo projects.Repository
	apiKeyRepo  projects.APIKeyRepository
	docDB       databases.DocumentDB
}

// setupBootstrapFixture 用真实数据库装配 bootstrap 全链路：
// gateway(RegisterConsoleAuthServiceHandlerServer) + 真实 use-case。
func setupBootstrapFixture(t *testing.T) *bootstrapFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	cfg := &config.AppConfig{Security: &config.Security{
		Jwt:        &config.Security_Jwt{Secret: "bootstrap-integration-secret"}, // #nosec G101 -- 测试固定密钥
		SetupToken: "bootstrap-setup-token",
	}}

	adminRepo := bunrepo.NewAdminRepository(db)
	projectRepo := bunrepo.NewProjectRepository(db)
	apiKeyRepo := bunrepo.NewAPIKeyRepository(db)
	admins := console.NewAdmins(adminRepo, bunrepo.NewAdminProjectRepository(db), db)
	projects := server.NewProjects(projectRepo, docDB, db, projectschema.NewSchemaManager(db), bunrepo.NewAdminProjectRepository(db), nil)
	auth := console.NewAuth(cfg, adminRepo, nil, nil, nil)
	setupUC := console.NewSetup(cfg, admins, projects, auth, adminRepo, bunrepo.NewAdminProjectRepository(db), projectRepo)
	svc := NewAuthService(auth, setupUC)

	// 生产网关用 authOutgoingHeaderMatcher 透传 set-cookie + protojson
	// （UseProtoNames）；测试 mux 对齐该配置。
	marshaler := &runtime.JSONPb{
		MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false},
		UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
	}
	mux := runtime.NewServeMux(
		runtime.WithOutgoingHeaderMatcher(func(key string) (string, bool) {
			if key == "set-cookie" {
				return key, true
			}
			return runtime.DefaultHeaderMatcher(key)
		}),
		runtime.WithMarshalerOption("*", marshaler),
		runtime.WithMarshalerOption("*/*", marshaler),
		runtime.WithMarshalerOption("application/json", marshaler),
	)
	require.NoError(t, consolev1.RegisterConsoleAuthServiceHandlerServer(ctx, mux, svc))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &bootstrapFixture{
		url:         srv.URL,
		projectRepo: projectRepo,
		apiKeyRepo:  apiKeyRepo,
		docDB:       docDB,
	}
}

type signUpPayload struct {
	Admin struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	} `json:"admin"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func TestBootstrap_SignUpEndToEnd(t *testing.T) {
	fixture := setupBootstrapFixture(t)
	ctx := context.Background()

	// 1) 首次 sign-up：owner admin + 指定 project/database + 会话 cookie；
	//    不生成 API Key。
	body := bytes.NewBufferString(`{"email":"owner@torchwood.local","password":"Pass@1234","setup_token":"bootstrap-setup-token","project_id":"shop","database_id":"app"}`)
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.url+"/v1/console/auth/sign-up", body)
	require.NoError(t, err)
	postReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(postReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload signUpPayload
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	_ = resp.Body.Close()

	require.NotEmpty(t, payload.Admin.Role)
	require.Equal(t, "owner", payload.Admin.Role)
	require.Equal(t, "owner@torchwood.local", payload.Admin.Email)
	require.NotEmpty(t, payload.AccessToken)
	require.NotEmpty(t, payload.RefreshToken)

	project, err := fixture.projectRepo.GetProject(ctx, "shop")
	require.NoError(t, err)
	require.NotNil(t, project)
	appDB, err := fixture.docDB.GetDatabase(ctx, "shop", "app")
	require.NoError(t, err)
	require.NotNil(t, appDB)
	sysDB, err := fixture.docDB.GetDatabase(ctx, "shop", databases.SystemDatabaseID)
	require.NoError(t, err)
	require.Nil(t, sysDB, "cut 后 catalog 无 database_id='_'")
	keys, err := fixture.apiKeyRepo.ListAPIKeys(ctx, "shop")
	require.NoError(t, err)
	require.Empty(t, keys)

	// 会话 cookie 已下发（SignUp 复用 setSessionCookies）。
	var foundSession, foundRefresh bool
	for _, c := range resp.Header.Values("Set-Cookie") {
		foundSession = foundSession || strings.Contains(c, "TORCHWOOD_session_console=")
		foundRefresh = foundRefresh || strings.Contains(c, "TORCHWOOD_console_refresh=")
	}
	require.True(t, foundSession, "session cookie missing: %v", resp.Header.Values("Set-Cookie"))
	require.True(t, foundRefresh, "refresh cookie missing: %v", resp.Header.Values("Set-Cookie"))

	// 2) setup-status 返回 needs_setup=false。
	statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.url+"/v1/console/auth/setup-status", nil)
	require.NoError(t, err)
	resp, err = http.DefaultClient.Do(statusReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var statusResp struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&statusResp))
	_ = resp.Body.Close()
	require.False(t, statusResp.NeedsSetup)

	// 3) 二次 sign-up → FailedPrecondition（grpc-gateway 映射为 HTTP 400）。
	body = bytes.NewBufferString(`{"email":"second@torchwood.local","password":"Pass@1234","setup_token":"bootstrap-setup-token","project_id":"shop","database_id":"app"}`)
	retryReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.url+"/v1/console/auth/sign-up", body)
	require.NoError(t, err)
	retryReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(retryReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	var errResp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&errResp))
	_ = resp.Body.Close()
	require.Contains(t, errResp.Message, "setup already completed")
}

func TestBootstrap_SetupStatusBeforeSignUp(t *testing.T) {
	fixture := setupBootstrapFixture(t)
	ctx := context.Background()

	statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.url+"/v1/console/auth/setup-status", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(statusReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var statusResp struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&statusResp))
	_ = resp.Body.Close()
	require.True(t, statusResp.NeedsSetup)
}
