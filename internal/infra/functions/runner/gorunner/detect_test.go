package gorunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree 辅助：把 files 写入临时构建目录（模拟 zip 解压产物）。
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	srcFetch = "package fn\n\nimport \"net/http\"\n\nfunc Fetch(w http.ResponseWriter, r *http.Request) {}\n"
	srcMain  = "package fn\n\nfunc Main(data map[string]any, ctx map[string]string) (any, error) { return nil, nil }\n"
)

// TestDetectEntry 表驱动（设计 §1 + 独立复核 A6）：Fetch 优先 / Main 兜底 /
// 双缺报错；//go:build 约束与平台后缀按 GOOS=linux/GOARCH=amd64 评估；
// *_test.go 显式跳过；未导出与方法形态不算入口；语法错误显式上报。
func TestDetectEntry(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		want    Entry
		wantErr string
	}{
		{
			name:  "fetch only",
			files: map[string]string{"fn.go": srcFetch},
			want:  EntryFetch,
		},
		{
			name:  "main only",
			files: map[string]string{"fn.go": srcMain},
			want:  EntryMain,
		},
		{
			name: "both -> fetch wins (priority fixed)",
			files: map[string]string{
				"a_main.go":  srcMain,
				"b_fetch.go": srcFetch, // 文件序靠后仍胜出（优先级与文件枚举序无关）
			},
			want: EntryFetch,
		},
		{
			name:    "neither -> explicit error (align node index.js must export main or fetch)",
			files:   map[string]string{"fn.go": "package fn\n\nfunc helper() {}\n"},
			want:    EntryNone,
			wantErr: "go function must export Fetch or Main",
		},
		{
			name:    "empty dir",
			files:   map[string]string{},
			want:    EntryNone,
			wantErr: "go function must export Fetch or Main",
		},
		{
			name: "//go:build ignore file is excluded (compiler-same semantics)",
			files: map[string]string{
				"fn.go":      srcMain,
				"example.go": "//go:build ignore\n\npackage main\n\nimport \"net/http\"\n\nfunc Fetch(w http.ResponseWriter, r *http.Request) {}\n",
			},
			want: EntryMain,
		},
		{
			name: "//go:build ignore only -> no entry",
			files: map[string]string{
				"example.go": "//go:build ignore\n\npackage main\n\nimport \"net/http\"\n\nfunc Fetch(w http.ResponseWriter, r *http.Request) {}\n",
			},
			want:    EntryNone,
			wantErr: "go function must export Fetch or Main",
		},
		{
			name: "test files skipped (export_test.go pattern)",
			files: map[string]string{
				"fn.go":          srcMain,
				"export_test.go": "package fn\n\nimport \"net/http\"\n\nfunc Fetch(w http.ResponseWriter, r *http.Request) {}\n",
			},
			want: EntryMain,
		},
		{
			name: "GOOS mismatch skipped (windows-only file under linux context)",
			files: map[string]string{
				"fn.go":            srcMain,
				"extra_windows.go": "package fn\n\nimport \"net/http\"\n\nfunc Fetch(w http.ResponseWriter, r *http.Request) {}\n",
			},
			want: EntryMain,
		},
		{
			name: "GOOS match included (linux suffix file)",
			files: map[string]string{
				"fn.go":          srcMain,
				"extra_linux.go": srcFetch,
			},
			want: EntryFetch,
		},
		{
			name: "//go:build linux constraint matched under explicit linux context",
			files: map[string]string{
				"fn.go":   srcMain,
				"hook.go": "//go:build linux\n\n" + srcFetch,
			},
			want: EntryFetch,
		},
		{
			name: "unexported functions ignored",
			files: map[string]string{
				"fn.go": "package fn\n\nimport \"net/http\"\n\nfunc fetch(w http.ResponseWriter, r *http.Request) {}\nfunc mainx() {}\n",
			},
			want:    EntryNone,
			wantErr: "go function must export Fetch or Main",
		},
		{
			name: "methods are not package entries",
			files: map[string]string{
				"fn.go": "package fn\n\nimport \"net/http\"\n\ntype S struct{}\n\nfunc (S) Fetch(w http.ResponseWriter, r *http.Request) {}\n\nfunc Main(data map[string]any, ctx map[string]string) (any, error) { return nil, nil }\n",
			},
			want: EntryMain,
		},
		{
			name: "subdirectories are not scanned (root package only)",
			files: map[string]string{
				"fn.go":      srcMain,
				"sub/sub.go": srcFetch,
			},
			want: EntryMain,
		},
		{
			name: "syntax error surfaces explicitly",
			files: map[string]string{
				"fn.go": "package fn\n\nfunc Fetch(",
			},
			want:    EntryNone,
			wantErr: "parse fn.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectEntry(writeTree(t, tc.files))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("DetectEntry err = %v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectEntry: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DetectEntry = %d, want %d", got, tc.want)
			}
		})
	}
}
