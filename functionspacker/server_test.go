package functionspacker

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// stubPack 返回固定响应的打包桩（HTTP 面测试与 gitpack 解耦）。
func stubPack(resp *PackResponse, err error) func(context.Context, PackRequest, PackOptions) (*PackResponse, error) {
	return func(context.Context, PackRequest, PackOptions) (*PackResponse, error) {
		return resp, err
	}
}

func postPack(t *testing.T, url string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/v1/pack/git", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestPackServerTokenMiddleware：token 配置时缺/错 401、对 200；token 空
// 豁免；healthz 恒豁免（liveness 静态探针不带凭据）。
func TestPackServerTokenMiddleware(t *testing.T) {
	srv := newPackServer("sekret", smallOpts(), 2)
	srv.pack = stubPack(&PackResponse{CommitSHA: "c", Checksum: "s", ZipBase64: "eg=="}, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"缺 token → 401", nil, http.StatusUnauthorized},
		{"错 token → 401", map[string]string{"X-Tw-Packer-Token": "wrong"}, http.StatusUnauthorized},
		{"对 token → 200", map[string]string{"X-Tw-Packer-Token": "sekret"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postPack(t, ts.URL, tc.headers, `{"url":"https://example.com/o/r"}`)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	t.Run("healthz 豁免 token", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatalf("get healthz: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("token 未配置豁免校验", func(t *testing.T) {
		open := newPackServer("", smallOpts(), 2)
		open.pack = stubPack(&PackResponse{}, nil)
		ots := httptest.NewServer(open)
		defer ots.Close()
		resp := postPack(t, ots.URL, nil, `{"url":"https://example.com/o/r"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}

// TestPackServerConcurrencyLimit：concurrency=1 时第一个请求占住信号量、
// 第二个请求立即 429（两道独立闸的 packer 侧闸口语义）。
func TestPackServerConcurrencyLimit(t *testing.T) {
	srv := newPackServer("", smallOpts(), 1)
	release := make(chan struct{})
	started := make(chan struct{})
	srv.pack = func(context.Context, PackRequest, PackOptions) (*PackResponse, error) {
		close(started)
		<-release
		return &PackResponse{}, nil
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	firstDone := make(chan *http.Response, 1)
	go func() {
		firstDone <- postPack(t, ts.URL, nil, `{"url":"https://example.com/o/r"}`)
	}()
	<-started

	second := postPack(t, ts.URL, nil, `{"url":"https://example.com/o/r"}`)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.StatusCode)
	}
	close(release)
	if first := <-firstDone; first.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.StatusCode)
	}
}

// TestPackServerTimeoutMapping：fetch_timeout 封顶触发的超时映射 504。
func TestPackServerTimeoutMapping(t *testing.T) {
	srv := newPackServer("", smallOpts(), 2)
	srv.opts.FetchTimeout = 50 * time.Millisecond
	srv.pack = func(ctx context.Context, _ PackRequest, _ PackOptions) (*PackResponse, error) {
		<-ctx.Done() // 模拟慢 clone 撞上 fetch_timeout 封顶
		return nil, ctx.Err()
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	resp := postPack(t, ts.URL, nil, `{"url":"https://example.com/o/r"}`)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
}

// TestPackServerBadRequest：参数错映射 400。
func TestPackServerBadRequest(t *testing.T) {
	srv := newPackServer("", smallOpts(), 2)
	srv.pack = func(context.Context, PackRequest, PackOptions) (*PackResponse, error) {
		t.Fatal("pack must not be called for invalid requests")
		return nil, nil
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	t.Run("非法 JSON → 400", func(t *testing.T) {
		resp := postPack(t, ts.URL, nil, `{not-json`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("url 缺失 → 400", func(t *testing.T) {
		resp := postPack(t, ts.URL, nil, `{"ref":"main"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// TestPackGitEndToEnd 是端到端冒烟：真实 packServer + 真实 PackGit +
// file:// fixture 仓 → POST /v1/pack/git → 响应 zip 可解压、内容与
// fixture 一致、checksum/commit SHA 正确（阶段 3 server 侧 SourcePacker
// 消费的同款契约）。
func TestPackGitEndToEnd(t *testing.T) {
	t.Setenv(config.EnvVarRuntime, "development")
	fx := writeFixtureRepo(t)

	srv := newPackServer("", smallOpts(), 2) // pack 字段缺省 = 真实 PackGit
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body, err := json.Marshal(PackRequest{URL: fx.fileURL()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := postPack(t, ts.URL, nil, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got PackResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// commit SHA 与 fixture 写入一致。
	if got.CommitSHA != fx.head.String() {
		t.Fatalf("commit sha = %s, want %s", got.CommitSHA, fx.head.String())
	}
	// zip 可解压且内容与 fixture 一致（symlink 跳过；.git 不外泄）。
	want := map[string]string{}
	for k, v := range fx.files {
		want[k] = v
	}
	raw, err := base64.StdEncoding.DecodeString(got.ZipBase64)
	if err != nil {
		t.Fatalf("decode zip_base64: %v", err)
	}
	entries := readZipEntries(t, raw)
	if len(entries) != len(want) {
		t.Fatalf("zip entries = %v, want %d entries", entries, len(want))
	}
	for name, content := range want {
		if entries[name] != content {
			t.Fatalf("entry %s = %q, want %q", name, entries[name], content)
		}
	}
	if _, ok := entries["link.txt"]; ok && fx.hasSymlink {
		t.Fatalf("symlink entry link.txt must be skipped")
	}
	// checksum 与字节 sha256 一致。
	sum := sha256.Sum256(raw)
	if got.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("checksum = %s, want %s", got.Checksum, hex.EncodeToString(sum[:]))
	}
}

func readZipEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	m := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", f.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		_ = rc.Close()
		m[f.Name] = buf.String()
	}
	return m
}
