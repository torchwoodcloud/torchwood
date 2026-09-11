package serverhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	appstorage "github.com/torchwoodcloud/torchwood/internal/app/storage"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infrastorage "github.com/torchwoodcloud/torchwood/internal/infra/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// serverWriteCtx 返回带 Server API 写主体（API key 类型）principal 的上下文：
// G6-4 后 CreateBucket use-case 要求 RequireServerPrincipal，测试直接调
// use-case 需注入主体。
func serverWriteCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "test-key", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
	})
}

// newUploadSessionStoreForTest 返回 miniredis 支撑的 UploadSessionStore。
func newUploadSessionStoreForTest(t *testing.T) domainstorage.UploadSessionStore {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return infrastorage.NewRedisUploadSessionStore(rdb)
}

type storageHTTPFixture struct {
	t         *testing.T
	db        *clients.Database
	projectID string
	apiSecret string
	handler   *FileHandler
	server    *httptest.Server
	bucketID  string
}

// fakeClock 手动推进的假时钟，实现 appstorage.Clock（J6-4）。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 将假时钟拨快 d。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func setupStorageHTTPFixture(t *testing.T, storageOpts ...appstorage.StorageOption) *storageHTTPFixture {
	t.Helper()

	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectID, _, projectCleanup := testutil.CreateTestProjectT(ctx, t, db)
	apiSecret, keyCleanup := testutil.CreateTestAPIKey(ctx, db, projectID, nil)

	cfg := &config.AppConfig{}
	cfg.Security = &config.Security{Jwt: &config.Security_Jwt{Secret: "test-file-token-secret"}}
	store := testutil.NewMemObjectStore()
	storageUC := appstorage.NewStorage(cfg, bunrepo.NewProjectRepository(db), store, newUploadSessionStoreForTest(t), bunrepo.NewBucketRepository(db), bunrepo.NewFileRepository(db), storageOpts...)
	validator := auth.NewValidator(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
	)
	handler, err := NewFileHandler(cfg, validator, storageUC, nil, nil, testPolicies())
	require.NoError(t, err)

	mux := runtime.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)

	bucket, err := storageUC.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: projectID,
		Name:      "http-test-bucket",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		server.Close()
		keyCleanup()
		projectCleanup()
		_ = db.Close()
	})

	return &storageHTTPFixture{
		t:         t,
		db:        db,
		projectID: projectID,
		apiSecret: apiSecret,
		handler:   handler,
		server:    server,
		bucketID:  bucket.ID,
	}
}

func (f *storageHTTPFixture) upload(content []byte, headers map[string]string, contentType string) (string, string, int) {
	f.t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if contentType == "" {
		part, err := writer.CreateFormFile("file", "test.txt")
		require.NoError(f.t, err)
		_, err = part.Write(content)
		require.NoError(f.t, err)
	} else {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="test.txt"`},
			"Content-Type":        {contentType},
		})
		require.NoError(f.t, err)
		_, err = part.Write(content)
		require.NoError(f.t, err)
	}
	require.NoError(f.t, writer.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.server.URL+"/v1/storage/buckets/"+f.bucketID+"/files", body)
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()

	var payload struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	}
	if resp.StatusCode == http.StatusCreated {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&payload))
	}
	return payload.ID, payload.MimeType, resp.StatusCode
}

func (f *storageHTTPFixture) download(path string, headers map[string]string) (int, []byte, http.Header) {
	f.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.server.URL+path, nil)
	require.NoError(f.t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	return resp.StatusCode, data, resp.Header
}

// TestFileHandler_Acceptance covers manual checklist §5.1–5.4:
// multipart upload, download, inline view, API Key auth.
func TestFileHandler_Acceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	fix := setupStorageHTTPFixture(t)
	content := []byte("multipart acceptance payload")
	headers := map[string]string{"X-Api-Key": fix.apiSecret}

	fileID, mimeType, status := fix.upload(content, headers, "text/plain")
	require.Equal(t, http.StatusCreated, status)
	require.NotEmpty(t, fileID)
	require.Equal(t, "text/plain", mimeType)

	downloadPath := "/v1/storage/buckets/" + fix.bucketID + "/files/" + fileID + "/download"
	code, got, respHeaders := fix.download(downloadPath, headers)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, content, got)
	// 下载/查看端点统一加固：禁 MIME 嗅探 + 沙箱 CSP。
	require.Equal(t, "nosniff", respHeaders.Get("X-Content-Type-Options"))
	require.Contains(t, respHeaders.Get("Content-Security-Policy"), "sandbox")

	viewPath := "/v1/storage/buckets/" + fix.bucketID + "/files/" + fileID + "/view"
	code, gotView, respHeaders := fix.download(viewPath, headers)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, content, gotView)
	require.NotEmpty(t, respHeaders.Get("Content-Type"))
	// text/plain 在白名单内，/view 保持 inline。
	require.Contains(t, respHeaders.Get("Content-Disposition"), "inline")
	require.Equal(t, "nosniff", respHeaders.Get("X-Content-Type-Options"))
	require.Contains(t, respHeaders.Get("Content-Security-Policy"), "sandbox")
}

// TestFileHandler_DangerousMimeHardening covers P0-2: 危险 MIME 上传时被归一化为
// application/octet-stream，SVG 即使白名单内也强制按附件下载，杜绝存储型同源 XSS。
func TestFileHandler_DangerousMimeHardening(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	fix := setupStorageHTTPFixture(t)
	headers := map[string]string{"X-Api-Key": fix.apiSecret}

	// text/html（带参数）上传后强制改判为 octet-stream，/view 不再 inline。
	htmlID, mimeType, status := fix.upload([]byte("<script>alert(1)</script>"), headers, "text/html; charset=utf-8")
	require.Equal(t, http.StatusCreated, status)
	require.NotEmpty(t, htmlID)
	require.Equal(t, "application/octet-stream", mimeType)

	viewPath := "/v1/storage/buckets/" + fix.bucketID + "/files/" + htmlID + "/view"
	code, _, respHeaders := fix.download(viewPath, headers)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "application/octet-stream", respHeaders.Get("Content-Type"))
	require.Contains(t, respHeaders.Get("Content-Disposition"), "attachment")
	require.NotContains(t, respHeaders.Get("Content-Disposition"), "inline")

	// SVG 可内嵌脚本：即使 MIME 未归一化，/view 也强制降级为附件下载。
	svgID, svgMime, status := fix.upload([]byte(`<svg onload="alert(1)"/>`), headers, "image/svg+xml")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, "image/svg+xml", svgMime)

	svgViewPath := "/v1/storage/buckets/" + fix.bucketID + "/files/" + svgID + "/view"
	code, _, svgHeaders := fix.download(svgViewPath, headers)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, svgHeaders.Get("Content-Disposition"), "attachment")
	require.NotContains(t, svgHeaders.Get("Content-Disposition"), "inline")
	require.Equal(t, "nosniff", svgHeaders.Get("X-Content-Type-Options"))
	require.Contains(t, svgHeaders.Get("Content-Security-Policy"), "sandbox")

	// 白名单图片（image/png）在 /view 仍可 inline。
	pngID, pngMime, status := fix.upload([]byte("fake-png"), headers, "image/png")
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, "image/png", pngMime)

	pngViewPath := "/v1/storage/buckets/" + fix.bucketID + "/files/" + pngID + "/view"
	code, _, pngHeaders := fix.download(pngViewPath, headers)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, pngHeaders.Get("Content-Disposition"), "inline")
}

// TestFileHandler_UserJWTProjectScope covers manual checklist §5.5:
// end-user JWT operates only on the project embedded in the token.
func TestFileHandler_UserJWTProjectScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectA, _, cleanupA := testutil.CreateTestProject(ctx, db)
	projectB, _, cleanupB := testutil.CreateTestProject(ctx, db)

	cfg := &config.AppConfig{}
	store := testutil.NewMemObjectStore()
	projectRepo := bunrepo.NewProjectRepository(db)
	storageUC := appstorage.NewStorage(cfg, projectRepo, store, newUploadSessionStoreForTest(t), bunrepo.NewBucketRepository(db), bunrepo.NewFileRepository(db))
	account := newFileHandlerTestAccount(cfg, projectRepo, db)

	_, tokens, _, _, err := account.SignUp(ctx, client.SignUpCommand{
		ProjectID: projectA,
		Email:     "storage-http@torchwood.local",
		Password:  "User@123456",
		Name:      "Storage HTTP",
	})
	require.NoError(t, err)

	bucketA, err := storageUC.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: projectA,
		Name:      "bucket-a",
	})
	require.NoError(t, err)
	bucketB, err := storageUC.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: projectB,
		Name:      "bucket-b",
	})
	require.NoError(t, err)

	validator := auth.NewValidator(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
	)
	handler, err := NewFileHandler(cfg, validator, storageUC, nil, nil, testPolicies())
	require.NoError(t, err)
	mux := runtime.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)

	t.Cleanup(func() {
		server.Close()
		cleanupB()
		cleanupA()
		_ = db.Close()
	})

	userHeaders := map[string]string{"Authorization": "Bearer " + tokens.AccessToken}
	want := []byte("project-a-only")

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "owned.txt")
	require.NoError(t, err)
	_, err = part.Write(want)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/storage/buckets/"+bucketA.ID+"/files", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range userHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotEmpty(t, created.ID)

	// Forged X-Torchwood-Project must not grant access to another project's bucket.
	bodyB := &bytes.Buffer{}
	writerB := multipart.NewWriter(bodyB)
	partB, err := writerB.CreateFormFile("file", "blocked.txt")
	require.NoError(t, err)
	_, err = partB.Write([]byte("should-not-upload"))
	require.NoError(t, err)
	require.NoError(t, writerB.Close())

	reqB, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/storage/buckets/"+bucketB.ID+"/files", bodyB)
	require.NoError(t, err)
	reqB.Header.Set("Content-Type", writerB.FormDataContentType())
	reqB.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	reqB.Header.Set("X-Torchwood-Project", projectB)
	respB, err := http.DefaultClient.Do(reqB)
	require.NoError(t, err)
	_ = respB.Body.Close()
	require.NotEqual(t, http.StatusCreated, respB.StatusCode)

	downloadReq, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		server.URL+"/v1/storage/buckets/"+bucketA.ID+"/files/"+created.ID+"/download",
		nil,
	)
	require.NoError(t, err)
	for k, v := range userHeaders {
		downloadReq.Header.Set(k, v)
	}
	downloadResp, err := http.DefaultClient.Do(downloadReq)
	require.NoError(t, err)
	defer func() { _ = downloadResp.Body.Close() }()
	require.Equal(t, http.StatusOK, downloadResp.StatusCode)
	got, err := io.ReadAll(downloadResp.Body)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestFileHandler_APIKeyRequiresStorageScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	apiSecret, keyCleanup := testutil.CreateTestAPIKey(ctx, db, projectID, []string{"users"})

	cfg := &config.AppConfig{}
	store := testutil.NewMemObjectStore()
	storageUC := appstorage.NewStorage(cfg, bunrepo.NewProjectRepository(db), store, newUploadSessionStoreForTest(t), bunrepo.NewBucketRepository(db), bunrepo.NewFileRepository(db))
	validator := auth.NewValidator(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
	)
	handler, err := NewFileHandler(cfg, validator, storageUC, nil, nil, testPolicies())
	require.NoError(t, err)
	mux := runtime.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)

	bucket, err := storageUC.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: projectID,
		Name:      "scope-test-bucket",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		server.Close()
		keyCleanup()
		projectCleanup()
		_ = db.Close()
	})

	_, _, status := (&storageHTTPFixture{
		t:         t,
		projectID: projectID,
		apiSecret: apiSecret,
		handler:   handler,
		server:    server,
		bucketID:  bucket.ID,
	}).upload([]byte("blocked"), map[string]string{"X-Api-Key": apiSecret}, "")
	require.Equal(t, http.StatusForbidden, status)
}

func TestFileHandler_AdminRequiresProjectAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectID, _, projectCleanup := testutil.CreateTestProject(ctx, db)
	otherProjectID, _, otherCleanup := testutil.CreateTestProject(ctx, db)

	cfg := &config.AppConfig{}
	store := testutil.NewMemObjectStore()
	storageUC := appstorage.NewStorage(cfg, bunrepo.NewProjectRepository(db), store, newUploadSessionStoreForTest(t), bunrepo.NewBucketRepository(db), bunrepo.NewFileRepository(db))
	admin, adminCleanup := testutil.CreateTestAdmin(ctx, db, "member")
	token, err := testutil.SignAdminToken(cfg, admin)
	require.NoError(t, err)
	require.NoError(t, testutil.GrantAdminProject(ctx, db, admin.ID, projectID))

	validator := auth.NewValidator(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
	)
	handler, err := NewFileHandler(cfg, validator, storageUC, nil, nil, testPolicies())
	require.NoError(t, err)
	mux := runtime.NewServeMux()
	handler.Register(mux)
	server := httptest.NewServer(mux)

	bucket, err := storageUC.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: otherProjectID,
		Name:      "foreign-bucket",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		server.Close()
		adminCleanup()
		otherCleanup()
		projectCleanup()
		_ = db.Close()
	})

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "blocked.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("should-not-upload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/storage/buckets/"+bucket.ID+"/files", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Torchwood-Project", otherProjectID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestFileHandler_PublicBucketAnonymousRead: public bucket 文件可在无凭证时经
// ?project= 匿名读取；非 public bucket 匿名读仍 401。
func TestFileHandler_PublicBucketAnonymousRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)

	// 建 public bucket 并上传文件。
	bucket, err := f.handler.storage.CreateBucket(serverWriteCtx(), appstorage.CreateBucketCommand{
		ProjectID: f.projectID,
		Name:      "public-bucket",
		Public:    true,
	})
	require.NoError(t, err)

	fileID, _, status := f.uploadTo(bucket.ID, []byte("public content"), map[string]string{"X-Api-Key": f.apiSecret}, "")
	require.Equal(t, http.StatusCreated, status)

	// 匿名（无任何凭证）+ project 参数 → 200。
	code, data, _ := f.download(
		"/v1/storage/buckets/"+bucket.ID+"/files/"+fileID+"/view?project="+f.projectID,
		nil,
	)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "public content", string(data))

	// 无 project 参数 → 401（无法定位项目）。
	code, _, _ = f.download("/v1/storage/buckets/"+bucket.ID+"/files/"+fileID+"/view", nil)
	require.Equal(t, http.StatusUnauthorized, code)

	// 非 public bucket 匿名读 → 401。
	privateID, _, status := f.uploadTo(f.bucketID, []byte("private content"), map[string]string{"X-Api-Key": f.apiSecret}, "")
	require.Equal(t, http.StatusCreated, status)
	code, _, _ = f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+privateID+"/view?project="+f.projectID,
		nil,
	)
	require.Equal(t, http.StatusUnauthorized, code)
}

// TestFileHandler_FileTokenDownload: 有效 token 匿名下载成功；过期与篡改 401；
// 绑定其他文件的 token 无效。
func TestFileHandler_FileTokenDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	fc := &fakeClock{now: time.Now()}
	f := setupStorageHTTPFixture(t, appstorage.WithClock(fc))

	fileID, _, status := f.upload([]byte("token-protected"), map[string]string{"X-Api-Key": f.apiSecret}, "")
	require.Equal(t, http.StatusCreated, status)

	// 无 token 匿名下载 → 401（bucket 非 public）。
	code, _, _ := f.download("/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/download", nil)
	require.Equal(t, http.StatusUnauthorized, code)

	token, err := f.handler.storage.CreateFileToken(ctx, f.projectID, f.bucketID, fileID, 300, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)

	// 有效 token → 200。
	code, data, _ := f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/download?token="+token.Token,
		nil,
	)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "token-protected", string(data))

	// 篡改 token → 401。
	code, _, _ = f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/download?token="+token.Token+"x",
		nil,
	)
	require.Equal(t, http.StatusUnauthorized, code)

	// 过期 token → 401（假时钟拨快，替代真实 sleep 1.1s，J6-4）。
	short, err := f.handler.storage.CreateFileToken(ctx, f.projectID, f.bucketID, fileID, 1, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	fc.Advance(2 * time.Second)
	code, _, _ = f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/download?token="+short.Token,
		nil,
	)
	require.Equal(t, http.StatusUnauthorized, code)
}

// TestFileHandler_Preview: 图片上传后 preview 生成指定尺寸缩略图；
// 非图片类型拒绝。
func TestFileHandler_Preview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)

	// 生成 40x20 红色 PNG。
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))
	draw.Draw(src, src.Bounds(), &image.Uniform{C: color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var pngBuf bytes.Buffer
	require.NoError(t, png.Encode(&pngBuf, src))

	fileID, _, status := f.uploadTo(f.bucketID, pngBuf.Bytes(), map[string]string{"X-Api-Key": f.apiSecret}, "image/png")
	require.Equal(t, http.StatusCreated, status)

	// 缩放 10x10 → 200 且输出仍为 PNG。
	code, data, headers := f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/preview?width=10&height=10",
		map[string]string{"X-Api-Key": f.apiSecret},
	)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "image/png", headers.Get("Content-Type"))
	require.NotEmpty(t, data)
	decoded, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 10, decoded.Bounds().Dx(), "Fit 保持宽高比：40x20 → 10x5")
	require.Equal(t, 5, decoded.Bounds().Dy())

	// 无缩放参数 → 原图回源。
	code, data, _ = f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+fileID+"/preview",
		map[string]string{"X-Api-Key": f.apiSecret},
	)
	require.Equal(t, http.StatusOK, code)
	decoded, err = png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 40, decoded.Bounds().Dx())

	// 非图片（text/plain）→ 400。
	txtID, _, status := f.upload([]byte("plain"), map[string]string{"X-Api-Key": f.apiSecret}, "text/plain")
	require.Equal(t, http.StatusCreated, status)
	code, _, _ = f.download(
		"/v1/storage/buckets/"+f.bucketID+"/files/"+txtID+"/preview?width=10",
		map[string]string{"X-Api-Key": f.apiSecret},
	)
	require.Equal(t, http.StatusBadRequest, code)
}

// uploadTo 与 upload 相同，但可指定目标 bucket（默认 bucket 由 fixture 持有）。
func (f *storageHTTPFixture) uploadTo(bucketID string, content []byte, headers map[string]string, contentType string) (string, string, int) {
	f.t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if contentType == "" {
		part, err := writer.CreateFormFile("file", "test.txt")
		require.NoError(f.t, err)
		_, err = part.Write(content)
		require.NoError(f.t, err)
	} else {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="file"; filename="test.png"`},
			"Content-Type":        {contentType},
		})
		require.NoError(f.t, err)
		_, err = part.Write(content)
		require.NoError(f.t, err)
	}
	require.NoError(f.t, writer.Close())

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.server.URL+"/v1/storage/buckets/"+bucketID+"/files", body)
	require.NoError(f.t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()

	var payload struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
	}
	if resp.StatusCode == http.StatusCreated {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&payload))
	}
	return payload.ID, payload.MimeType, resp.StatusCode
}

func newFileHandlerTestAccount(cfg *config.AppConfig, projectRepo projects.Repository, db *clients.Database) *client.Account {
	usersRepo := bunrepo.NewUserRepository(db)
	sessionRepo := bunrepo.NewSessionRepository(db)
	identities := bunrepo.NewIdentityRepository(db)
	roles := client.NewUserRoles(usersRepo, bunrepo.NewMembershipRepository(db))
	sessions := auth.NewSessionService(cfg, sessionRepo, roles, nil)
	return client.NewAccount(cfg, projectRepo, nil, nil, sessions, nil, nil, nil, nil, nil, nil, nil, nil, nil, roles, nil, nil, nil, nil, usersRepo, identities, sessionRepo, auth.NewOAuthAuthenticatorFactory(), auth.NewWeChatMiniProgramExchanger(), auth.NewOTPGenerator(), nil, nil)
}

// testPolicies 定义于 auth_test.go（同包共享）。

// TestFileHandler_AdminUploadNoOwnerAttribution 回归测试：Console（admin 会话）
// 上传曾把 AdminID 写入 owner_user_id，违反 files_owner_user_id_fkey（指向项目
// 数据面 users(id)）报 SQLSTATE 23503。修复后 storage 归属仅接受 EndUser
// （shared.StorageOwnerID），admin 创建的文件 owner 落 NULL。单发与分片
// complete 两条路径都断言。
func TestFileHandler_AdminUploadNoOwnerAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := setupStorageHTTPFixture(t)

	admin, adminCleanup := testutil.CreateTestAdmin(context.Background(), f.db, "member")
	defer adminCleanup()
	require.NoError(t, testutil.GrantAdminProject(context.Background(), f.db, admin.ID, f.projectID))
	cfg := &config.AppConfig{}
	cfg.Security = &config.Security{Jwt: &config.Security_Jwt{Secret: "test-file-token-secret"}}
	token, err := testutil.SignAdminToken(cfg, admin)
	require.NoError(t, err)
	adminHeaders := map[string]string{
		"Authorization":       "Bearer " + token,
		"X-Torchwood-Project": f.projectID,
	}

	// 单发：POST files（multipart）。
	fileID, _, status := f.upload([]byte("admin upload"), adminHeaders, "")
	require.Equal(t, http.StatusCreated, status)
	file, err := bunrepo.NewFileRepository(f.db).GetByID(context.Background(), f.projectID, fileID)
	require.NoError(t, err)
	require.Empty(t, file.OwnerUserID, "admin 创建的文件不归属项目用户，owner_user_id 应为 NULL")

	// 分片：create 会话 → 1 片 → complete（CompleteUpload 的 owner 回落路径）。
	status, created := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads", map[string]any{
		"name": "admin-chunked.bin",
		"size": 1024,
	}, adminHeaders)
	require.Equal(t, http.StatusCreated, status)
	uploadID, _ := created["upload_id"].(string)
	require.NotEmpty(t, uploadID)
	chunkStatus, _ := f.uploadChunkViaHTTP(f.bucketID, uploadID, 1, make([]byte, 1024), adminHeaders)
	require.Equal(t, http.StatusOK, chunkStatus)
	status, completed := f.doJSON(http.MethodPost, "/v1/storage/buckets/"+f.bucketID+"/uploads/"+uploadID+"/complete", nil, adminHeaders)
	require.Equal(t, http.StatusOK, status)
	completedID, _ := completed["id"].(string)
	require.NotEmpty(t, completedID)
	file, err = bunrepo.NewFileRepository(f.db).GetByID(context.Background(), f.projectID, completedID)
	require.NoError(t, err)
	require.Empty(t, file.OwnerUserID)
}
