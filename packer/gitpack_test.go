package packer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// mustParse 测试内解析 URL（非法形态直接 Fatal）。
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// smallOpts 是测试用收紧预算（非生产缺省——快速且可触发超限分支）。
func smallOpts() PackOptions {
	return PackOptions{
		MaxRepoBytes: 4 << 20,
		MaxZipBytes:  4 << 20,
		MaxEntries:   100,
		FetchTimeout: 30 * time.Second,
	}
}

// packInDev 以 development 环境跑一次打包（file:// 仅 dev 可用）。
func packInDev(t *testing.T, req PackRequest, opts PackOptions) (*PackResponse, error) {
	t.Helper()
	t.Setenv(config.EnvVarRuntime, "development")
	return PackGit(context.Background(), req, opts)
}

// unzipToMap 解包 zip_base64 为 {条目名: 内容} 映射（并返回原始字节供
// checksum 断言）。
func unzipToMap(t *testing.T, b64 string) (map[string]string, []byte) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode zip_base64: %v", err)
	}
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
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", f.Name, err)
		}
		m[f.Name] = string(content)
	}
	return m, raw
}

// requireCode 断言 err 的 grpc code。
func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %v, got nil", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("expected code %v, got %v (err=%v)", want, got, err)
	}
}

// TestPackGitTable 是 pack 行为表驱动：ref 三形态 + HEAD、directory 子
// 目录、穿越/预算/拒收全部分支、commit SHA 回读与 checksum 正确性。
func TestPackGitTable(t *testing.T) {
	fx := writeFixtureRepo(t)

	// defaultFiles = 根目录打包的期望条目：symlink 跳过（若平台支持
	// 创建）；node_modules 仅在成为路径首段时拒收——badsub/node_modules
	// 首段是 badsub，随包携带（与 zip 上传通道同口径）。
	defaultFiles := map[string]string{}
	for k, v := range fx.files {
		defaultFiles[k] = v
	}

	cases := []struct {
		name      string
		req       PackRequest
		wantFiles map[string]string
		wantCode  codes.Code // 空 = 期望成功
	}{
		{
			name:      "default HEAD via bare local path",
			req:       PackRequest{URL: fx.dir},
			wantFiles: defaultFiles,
		},
		{
			name:      "default HEAD via file:// url",
			req:       PackRequest{URL: fx.fileURL()},
			wantFiles: defaultFiles,
		},
		{
			name:      "ref = branch",
			req:       PackRequest{URL: fx.dir, Ref: fx.branch},
			wantFiles: defaultFiles,
		},
		{
			name:      "ref = tag",
			req:       PackRequest{URL: fx.dir, Ref: "v1"},
			wantFiles: defaultFiles,
		},
		{
			name:      "ref = 40-hex sha",
			req:       PackRequest{URL: fx.dir, Ref: fx.head.String()},
			wantFiles: defaultFiles,
		},
		{
			name:      "directory = sub（zip 条目以子目录为根）",
			req:       PackRequest{URL: fx.dir, Directory: "sub"},
			wantFiles: map[string]string{"inner.txt": "inner\n", "deep.txt": "deep\n"},
		},
		{
			name:      "directory = ./sub（Clean 归一）",
			req:       PackRequest{URL: fx.dir, Directory: "./sub/"},
			wantFiles: map[string]string{"inner.txt": "inner\n", "deep.txt": "deep\n"},
		},
		{
			name:     "directory 穿越（..）",
			req:      PackRequest{URL: fx.dir, Directory: "../.."},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "directory 穿越嵌套（sub/../../..）",
			req:      PackRequest{URL: fx.dir, Directory: "sub/../../.."},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "directory 绝对路径",
			req:      PackRequest{URL: fx.dir, Directory: filepath.ToSlash(filepath.Dir(fx.dir))},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "directory 指向不存在的子目录",
			req:      PackRequest{URL: fx.dir, Directory: "nope"},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "directory=badsub：node_modules 升为路径首段 → 拒收",
			req:      PackRequest{URL: fx.dir, Directory: "badsub"},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "仓库根 node_modules → 拒收",
			req:      PackRequest{URL: writeNodeModulesRepo(t)},
			wantCode: codes.InvalidArgument,
		},
		{
			name:     "仓库不存在 → 明确 404",
			req:      PackRequest{URL: filepath.Join(t.TempDir(), "no-such-repo")},
			wantCode: codes.NotFound,
		},
		{
			name:     "ref 未命中（branch/tag 都没有）→ 明确 404",
			req:      PackRequest{URL: fx.dir, Ref: "no-such-ref"},
			wantCode: codes.NotFound,
		},
		{
			name:     "url 缺失",
			req:      PackRequest{},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := packInDev(t, tc.req, smallOpts())
			if tc.wantCode != codes.OK {
				requireCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("PackGit: %v", err)
			}
			// commit SHA 回读与 fixture 写入一致。
			if resp.CommitSHA != fx.head.String() {
				t.Fatalf("commit sha = %s, want %s", resp.CommitSHA, fx.head.String())
			}
			got, raw := unzipToMap(t, resp.ZipBase64)
			if len(got) != len(tc.wantFiles) {
				t.Fatalf("zip entries = %v, want %d entries", got, len(tc.wantFiles))
			}
			for name, want := range tc.wantFiles {
				if got[name] != want {
					t.Fatalf("entry %s = %q, want %q", name, got[name], want)
				}
			}
			// 任何条目都不得含 .git 与 symlink。
			for name := range got {
				if name == ".git" || strings.HasPrefix(name, ".git/") {
					t.Fatalf("zip leaks .git entry %s", name)
				}
				if name == "link.txt" {
					t.Fatalf("zip leaks symlink entry link.txt")
				}
			}
			// checksum = zip 字节的 hex sha256。
			sum := sha256.Sum256(raw)
			if resp.Checksum != hex.EncodeToString(sum[:]) {
				t.Fatalf("checksum = %s, want %s", resp.Checksum, hex.EncodeToString(sum[:]))
			}
		})
	}

	// symlink 跳过：仅当平台支持创建 symlink 时断言（Windows 无开发者
	// 模式 os.Symlink 失败 → fixture 无该条目，跳过 ≠ 放松）。
	if fx.hasSymlink {
		t.Run("symlink skipped", func(t *testing.T) {
			resp, err := packInDev(t, PackRequest{URL: fx.dir}, smallOpts())
			if err != nil {
				t.Fatalf("PackGit: %v", err)
			}
			got, _ := unzipToMap(t, resp.ZipBase64)
			if _, ok := got["link.txt"]; ok {
				t.Fatalf("symlink entry link.txt must be skipped")
			}
		})
	}
}

// TestPackGitBudgets 覆盖两级预算与条目上限的独立触发。
func TestPackGitBudgets(t *testing.T) {
	t.Run("worktree 超过 max_repo_bytes", func(t *testing.T) {
		repo := writeBigFileRepo(t, 64<<10)
		opts := smallOpts()
		opts.MaxRepoBytes = 32 << 10 // < 64KiB 文件
		_, err := packInDev(t, PackRequest{URL: repo}, opts)
		requireCode(t, err, codes.ResourceExhausted)
	})
	t.Run("物化 zip 超过 max_zip_bytes", func(t *testing.T) {
		repo := writeBigFileRepo(t, 64<<10)
		opts := smallOpts()
		opts.MaxZipBytes = 32 << 10 // 随机字节不可压缩，zip ≈ 64KiB > 32KiB
		_, err := packInDev(t, PackRequest{URL: repo}, opts)
		requireCode(t, err, codes.ResourceExhausted)
	})
	t.Run("条目数超过 MaxEntries", func(t *testing.T) {
		repo := writeManyFilesRepo(t, 12)
		opts := smallOpts()
		opts.MaxEntries = 10
		_, err := packInDev(t, PackRequest{URL: repo}, opts)
		requireCode(t, err, codes.ResourceExhausted)
	})
}

// TestPackGitDeterministic 同一 commit 两次打包产出字节级一致的 zip
// （条目零 Modified + walk 字典序 → checksum 稳定，可作为不可变快照锚）。
func TestPackGitDeterministic(t *testing.T) {
	fx := writeFixtureRepo(t)
	a, err := packInDev(t, PackRequest{URL: fx.dir}, smallOpts())
	if err != nil {
		t.Fatalf("first pack: %v", err)
	}
	b, err := packInDev(t, PackRequest{URL: fx.dir}, smallOpts())
	if err != nil {
		t.Fatalf("second pack: %v", err)
	}
	if a.Checksum != b.Checksum {
		t.Fatalf("checksum not deterministic: %s vs %s", a.Checksum, b.Checksum)
	}
}

// TestCheckDialAddress 是 IP guard 表驱动：loopback/private/link-local/
// unspecified 拒、公网放行、allow_insecure 全放行、非 IP fail-closed。
func TestCheckDialAddress(t *testing.T) {
	cases := []struct {
		address       string
		allowInsecure bool
		wantBlocked   bool
	}{
		{"127.0.0.1:443", false, true},
		{"127.8.8.8:443", false, true},                             // 127/8 整段
		{"10.1.2.3:80", false, true},                               // 10/8
		{"172.16.0.9:443", false, true},                            // 172.16/12
		{"192.168.1.1:443", false, true},                           // 192.168/16
		{"169.254.169.254:80", false, true},                        // link-local（云元数据端点）
		{"[::1]:443", false, true},                                 // IPv6 loopback
		{"[fe80::1]:443", false, true},                             // IPv6 link-local
		{"0.0.0.0:80", false, true},                                // unspecified
		{"93.184.216.34:443", false, false},                        // 公网样例
		{"[2606:2800:220:1:248:1893:25c8:1946]:443", false, false}, // 公网 IPv6
		{"127.0.0.1:443", true, false},                             // allow_insecure 全放行
		{"169.254.169.254:80", true, false},                        // allow_insecure 全放行
		{"example.invalid:443", false, true},                       // 非 IP fail-closed
	}
	for _, tc := range cases {
		err := checkDialAddress(tc.address, tc.allowInsecure)
		if tc.wantBlocked && err == nil {
			t.Errorf("checkDialAddress(%q, insecure=%v) = nil, want blocked", tc.address, tc.allowInsecure)
		}
		if !tc.wantBlocked && err != nil {
			t.Errorf("checkDialAddress(%q, insecure=%v) = %v, want allowed", tc.address, tc.allowInsecure, err)
		}
	}
}

// TestValidateSourceURL 覆盖 URL 校验分支（scheme 白名单 / userinfo 拒绝 /
// file 与裸路径仅 development / allow_insecure 放行 http）。
func TestValidateSourceURL(t *testing.T) {
	// 裸路径用例取平台绝对路径（Windows 盘符路径 IsAbs 才为真）。
	absLocal := filepath.Join(os.TempDir(), "repo")
	cases := []struct {
		name          string
		raw           string
		devEnv        bool
		allowInsecure bool
		wantCode      codes.Code
		wantLocal     bool // 期望归一为本地路径形态（无 "://"）
	}{
		{name: "https 放行", raw: "https://github.com/o/r.git", wantCode: codes.OK},
		{name: "http 默认拒绝", raw: "http://github.com/o/r.git", wantCode: codes.InvalidArgument},
		{name: "http + allow_insecure 放行", raw: "http://gitea.internal/o/r.git", allowInsecure: true, wantCode: codes.OK},
		{name: "userinfo 拒绝（防 URL 落日志泄密）", raw: "https://user:tok@github.com/o/r.git", wantCode: codes.InvalidArgument}, // #nosec G101 -- 测试夹具伪凭证（负向用例：断言该形态被拒绝）
		{name: "ssh scheme 拒绝", raw: "ssh://git@github.com/o/r.git", wantCode: codes.InvalidArgument},
		{name: "file:// 非 dev 拒绝", raw: "file:///tmp/repo", wantCode: codes.InvalidArgument},
		{name: "file:// dev 放行并归一本地路径", raw: "file:///tmp/repo", devEnv: true, wantCode: codes.OK, wantLocal: true},
		{name: "file:// 带远端 host 拒绝", raw: "file://evil.example/tmp/repo", devEnv: true, wantCode: codes.InvalidArgument},
		{name: "裸绝对路径 dev 放行", raw: absLocal, devEnv: true, wantCode: codes.OK, wantLocal: true},
		{name: "裸绝对路径非 dev 拒绝", raw: absLocal, wantCode: codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateSourceURL(tc.raw, tc.devEnv, tc.allowInsecure)
			if tc.wantCode != codes.OK {
				requireCode(t, err, tc.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("validateSourceURL: %v", err)
			}
			if tc.wantLocal != isLocalSource(got) {
				t.Fatalf("local form = %v (got %q), want %v", isLocalSource(got), got, tc.wantLocal)
			}
		})
	}
}

// TestPackGitFileURLRejectedOutsideDev：file:// 在生产（含缺省）环境拒绝。
func TestPackGitFileURLRejectedOutsideDev(t *testing.T) {
	for _, env := range []string{"production", ""} {
		t.Setenv(config.EnvVarRuntime, env)
		_, err := PackGit(context.Background(), PackRequest{URL: "file:///tmp/repo"}, smallOpts())
		requireCode(t, err, codes.InvalidArgument)

		_, err = PackGit(context.Background(), PackRequest{URL: os.TempDir()}, smallOpts())
		requireCode(t, err, codes.InvalidArgument)
	}
}

// TestLocalPathFromURL 覆盖 Windows 盘符三斜杠形态的还原。
func TestLocalPathFromURL(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"file:///tmp/repo", filepath.FromSlash("/tmp/repo")},
		{"file:///C:/tmp/repo", filepath.FromSlash("C:/tmp/repo")},
		{"file://localhost/tmp/repo", filepath.FromSlash("/tmp/repo")},
	}
	for _, tc := range cases {
		u := mustParse(t, tc.raw)
		if got := localPathFromURL(u); got != tc.want {
			t.Errorf("localPathFromURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
