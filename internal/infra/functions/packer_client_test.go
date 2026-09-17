package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/functionspacker"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 SourcePacker 的 HTTP 适配（二期阶段 3，设计 §2）：POST
// /v1/pack/git 协议（shared_token header、请求形状、响应解码）与错误映射
// （429/504/400/401/其余 → grpc codes；url 未配置 fail-fast；响应读取上限）。

func packerTestCfg(url string) *config.AppConfig {
	return &config.AppConfig{Functions: &config.Functions{
		Packer: &config.Functions_Packer{Url: url, SharedToken: "test-shared-token"},
	}}
}

// TestPackerClient_PackGitProtocol 协议面：shared_token header 名
// （x-tw-packer-token，与 functionspacker/server.go 中间件同源）、请求体
// 逐字段（含一次性凭证）、响应解码（commit_sha/checksum/zip base64 字节级
// 往返）。
func TestPackerClient_PackGitProtocol(t *testing.T) {
	var gotToken string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/pack/git", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		gotToken = r.Header.Get("X-Tw-Packer-Token")
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"commit_sha":"0123456789abcdef0123456789abcdef01234567","checksum":"abc123","zip_base64":"` +
			base64.StdEncoding.EncodeToString([]byte("PK\x03\x04-zip-bytes")) + `"}`))
	}))
	defer srv.Close()

	client := NewPackerClient(packerTestCfg(srv.URL))
	commit, checksum, zip, err := client.PackGit(context.Background(), domainfunctions.GitSource{
		URL:       "https://git.example.com/acme/widget.git",
		Ref:       "main",
		Directory: "functions/greet",
		Username:  "git",
		Token:     "one-shot-token",
	})
	require.NoError(t, err)
	require.Equal(t, "test-shared-token", gotToken, "shared_token 走 x-tw-packer-token header")
	require.Equal(t, "https://git.example.com/acme/widget.git", gotBody["url"])
	require.Equal(t, "main", gotBody["ref"])
	require.Equal(t, "functions/greet", gotBody["directory"])
	require.Equal(t, "git", gotBody["username"])
	require.Equal(t, "one-shot-token", gotBody["token"], "一次性凭证随请求体送达 packer（仅调用栈内存）")
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", commit)
	require.Equal(t, "abc123", checksum)
	require.Equal(t, []byte("PK\x03\x04-zip-bytes"), zip, "zip base64 字节级往返")
}

// TestPackerClient_ErrorMapping 状态码映射表驱动（与 packer 服务端
// writeError 口径互逆）：429→ResourceExhausted、504→DeadlineExceeded、
// 400→InvalidArgument、401→FailedPrecondition（固定文案）、其余→Internal。
func TestPackerClient_ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		body       string
		wantCode   codes.Code
		wantMsg    string
	}{
		{name: "并发饱和 429", httpStatus: http.StatusTooManyRequests, body: `{"error":"packer at concurrency limit"}`, wantCode: codes.ResourceExhausted, wantMsg: "packer at concurrency limit"},
		{name: "打包超时 504", httpStatus: http.StatusGatewayTimeout, body: `{"error":"git pack exceeded functions.packer.fetch_timeout"}`, wantCode: codes.DeadlineExceeded, wantMsg: "fetch_timeout"},
		{name: "形状非法 400", httpStatus: http.StatusBadRequest, body: `{"error":"url is required"}`, wantCode: codes.InvalidArgument, wantMsg: "url is required"},
		{name: "认证失败 401", httpStatus: http.StatusUnauthorized, body: `{"error":"invalid packer token"}`, wantCode: codes.FailedPrecondition, wantMsg: "packer authentication failed"},
		{name: "内部错误 500", httpStatus: http.StatusInternalServerError, body: `{"error":"clone failed"}`, wantCode: codes.Internal, wantMsg: "clone failed"},
		{name: "非 JSON 错误体", httpStatus: http.StatusBadGateway, body: `upstream hiccup`, wantCode: codes.Internal, wantMsg: "packer error (http 502)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, _, _, err := NewPackerClient(packerTestCfg(srv.URL)).PackGit(context.Background(), domainfunctions.GitSource{URL: "https://git.example.com/a/b.git"})
			require.Equal(t, tc.wantCode, status.Code(err))
			require.ErrorContains(t, err, tc.wantMsg)
		})
	}
}

// TestPackerClient_URLEmptyFailFast url 未配置 = git 源未启用：不发起任何
// 网络请求直接 FailedPrecondition（固定文案；wire 装配恒可、增量启用）。
func TestPackerClient_URLEmptyFailFast(t *testing.T) {
	client := NewPackerClient(&config.AppConfig{Functions: &config.Functions{
		Packer: &config.Functions_Packer{},
	}})
	_, _, _, err := client.PackGit(context.Background(), domainfunctions.GitSource{URL: "https://git.example.com/a/b.git"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "functions.packer.url is not configured (git deployment source disabled)")
}

// TestPackerClient_ResponseLimit 响应读取上限随 config max_zip_bytes 派生
// （zip base64 ×4/3 + 1MiB 余量）：合法响应不受截断；超限响应被截断后
// 解码失败报 Internal（防失配/恶意响应无限读取）。
func TestPackerClient_ResponseLimit(t *testing.T) {
	padZip := func(n int) string {
		return base64.StdEncoding.EncodeToString(make([]byte, n))
	}
	t.Run("max_zip_bytes 内的合法响应完整读取", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"commit_sha":"c","checksum":"s","zip_base64":"` + padZip(3<<20) + `"}`))
		}))
		defer srv.Close()
		cfg := packerTestCfg(srv.URL)
		cfg.Functions.Packer.MaxZipBytes = functionspacker.DefaultMaxZipBytes
		_, _, zip, err := NewPackerClient(cfg).PackGit(context.Background(), domainfunctions.GitSource{URL: "https://git.example.com/a/b.git"})
		require.NoError(t, err)
		require.Len(t, zip, 3<<20)
	})
	t.Run("超限响应截断报错", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"commit_sha":"c","checksum":"s","zip_base64":"` + padZip(8<<20) + `"}`))
		}))
		defer srv.Close()
		cfg := packerTestCfg(srv.URL)
		// max_zip_bytes=3B → 上限 = 4B + 1MiB；8MiB base64 响应必然截断。
		cfg.Functions.Packer.MaxZipBytes = 3
		_, _, _, err := NewPackerClient(cfg).PackGit(context.Background(), domainfunctions.GitSource{URL: "https://git.example.com/a/b.git"})
		require.Equal(t, codes.Internal, status.Code(err), "截断的 base64 响应解码失败报 Internal")
	})
}
