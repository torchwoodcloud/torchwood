package dispatcher

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTarDir_NormalizesModesIndependentOfUmask 权限坏档修复（EACCES 秒退事故）：
// 构建进程 umask 会掩蔽 os.WriteFile/OpenFile 声明的 0644（umask 0077 → 落盘
// 0600），FileInfoHeader 忠实保留磁盘实际 mode 经 COPY 进镜像——模板 USER node
// 读 .tw-runner.js 即 EACCES、容器秒退。tarDir 必须在 tar header 单点归一化：
// 文件恒 0644、目录恒 0755、属主归零，镜像权限与 dispatcher 以何用户/何
// umask 运行解耦。Linux 下置 umask 0077 复现生产掩蔽形态（修复前本测试必红）。
func TestTarDir_NormalizesModesIndependentOfUmask(t *testing.T) {
	restoreUmask := setTestUmask(0o077)
	defer restoreUmask()

	dir := t.TempDir()
	// 写盘用收紧 mode（0600/0750）：直接模拟生产 umask 掩蔽后的磁盘状态
	// （0644 声明值被 umask 0077 掩蔽成 0600 的形态），断言 tarDir 归一化能
	// 向上收回 0644/0755——镜像内权限不依赖磁盘上的实际 mode。
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".tw-runner.js"), []byte("runner"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("code"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "util.js"), []byte("util"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x"), 0o600))

	rd := tarDir(dir)
	defer func() { _ = rd.Close() }()
	tr := tar.NewReader(rd)
	sawFile, sawDir := 0, 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeDir {
			sawDir++
			require.Equal(t, int64(0o755), hdr.Mode, "dir %s", hdr.Name)
		} else {
			sawFile++
			require.Equal(t, int64(0o644), hdr.Mode, "file %s", hdr.Name)
		}
		require.Zero(t, hdr.Uid, "entry %s", hdr.Name)
		require.Zero(t, hdr.Gid, "entry %s", hdr.Name)
	}
	require.Positive(t, sawFile)
	require.Equal(t, 1, sawDir)
}

// TestTarDir_StreamingContentRoundTrip 流式等价性（P2 S13）：tarDir 改
// io.Pipe 流式后，tar 字节语义与全量缓冲版一致——条目集合、条目名（目录
// 带尾斜杠）、文件内容逐字节保真；流正常收尾（EOF 即写侧 Close(nil)）。
func TestTarDir_StreamingContentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Dockerfile":     "FROM scratch\nCOPY . .\n",
		".tw-runner.js":  "runner script \x00 binary-safe",
		"lib/util.js":    "module.exports = 42;",
		"deep/a/b/c.txt": "nested",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}

	rd := tarDir(dir)
	defer func() { _ = rd.Close() }()
	got := map[string]string{}
	sawDirs := map[string]bool{}
	tr := tar.NewReader(rd)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if hdr.Typeflag == tar.TypeDir {
			require.True(t, strings.HasSuffix(hdr.Name, "/"), "目录条目必须带尾斜杠: %s", hdr.Name)
			sawDirs[hdr.Name] = true
			continue
		}
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		got[hdr.Name] = string(b)
	}
	require.Equal(t, files, got, "流式 tar 的文件内容必须与源逐字节一致")
	require.Contains(t, sawDirs, "lib/", "目录条目保留")
	require.Contains(t, sawDirs, "deep/a/b/", "嵌套目录条目保留")
}
