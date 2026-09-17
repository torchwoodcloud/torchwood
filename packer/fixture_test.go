package packer

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// fixtureRepo 是 go-git 就地造出的测试仓库（不依赖系统 git 二进制）。
type fixtureRepo struct {
	dir    string // 仓库根（t.TempDir 之下）
	head   plumbing.Hash
	branch string // PlainInit 的缺省分支名（随 go-git 版本，通常 master）
	files  map[string]string
	// hasSymlink：Windows 无开发者模式/特权时 os.Symlink 失败，symlink
	// 条目缺席，相关断言按此条件化（跳过 ≠ 放松）。
	hasSymlink bool
}

// fileURL 返回仓库根的 file:// 形态 URL（Windows 盘符需三斜杠
// file:///C:/...）。
func (fx *fixtureRepo) fileURL() string {
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(fx.dir), "/")
}

// writeFixtureRepo 造标准 fixture 仓库：
//
//	handler.js / go.sum                     根目录常规文件
//	sub/inner.txt / sub/deep.txt            子目录（directory 打包对象）
//	badsub/keep.txt + badsub/node_modules/  directory=badsub 时 node_modules
//	                                        升为路径首段 → 拒收（zip 通道口径）
//	link.txt → handler.js                   symlink 条目（物化跳过）
//
// 轻量 tag v1 钉在与分支头同一提交（ref=tag/sha 三形态同 SHA 可断言）。
func writeFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	fx := &fixtureRepo{
		dir: dir,
		files: map[string]string{
			"handler.js":                  "export function handler(req) { return { status: 200, body: 'ok' } }\n",
			"go.sum":                      "example.com/dep v1.0.0 h1:abcdef=\n",
			"sub/inner.txt":               "inner\n",
			"sub/deep.txt":                "deep\n",
			"badsub/keep.txt":             "keep\n",
			"badsub/node_modules/junk.js": "junk\n",
		},
	}
	for rel, content := range fx.files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add %s: %v", rel, err)
		}
	}
	if err := os.Symlink("handler.js", filepath.Join(dir, "link.txt")); err != nil {
		t.Logf("symlink unavailable on this platform/filesystem, fixture skips it: %v", err)
	} else if _, err := wt.Add("link.txt"); err != nil {
		t.Fatalf("add link.txt: %v", err)
	} else {
		fx.hasSymlink = true
	}
	sig := &object.Signature{Name: "tw-test", Email: "tw@test.local", When: time.Unix(1700000000, 0)}
	h, err := wt.Commit("fixture", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := repo.CreateTag("v1", h, nil); err != nil {
		t.Fatalf("tag v1: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	fx.head = head.Hash()
	fx.branch = head.Name().Short()
	return fx
}

// writeNodeModulesRepo 造「仓库根 node_modules」fixture：默认打包即拒收
// （与 zip 上传通道同口径：首段 node_modules 整体拒绝）。
func writeNodeModulesRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	files := map[string]string{
		"app.js":                 "console.log('app')\n",
		"node_modules/junk.js":   "junk\n",
		"node_modules/sub/x.txt": "x\n",
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add %s: %v", rel, err)
		}
	}
	sig := &object.Signature{Name: "tw-test", Email: "tw@test.local", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("nm", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dir
}

// writeBigFileRepo 造「单文件超预算」fixture：内容为 crypto/rand 随机字节
// （deflate 不可压缩，zip 体积≈原文件），供 max_zip_bytes / max_repo_bytes
// 超限用例稳定触发。
func writeBigFileRepo(t *testing.T, size int) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	blob := make([]byte, size)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), blob, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := wt.Add("big.bin"); err != nil {
		t.Fatalf("add: %v", err)
	}
	sig := &object.Signature{Name: "tw-test", Email: "tw@test.local", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("big", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dir
}

// writeManyFilesRepo 造 n 个常规文件的仓库（条目超限用例）。
func writeManyFilesRepo(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("f%03d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(rel), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	sig := &object.Signature{Name: "tw-test", Email: "tw@test.local", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("many", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dir
}
