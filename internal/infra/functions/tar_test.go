package functions

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTarDir_NormalizesModesIndependentOfUmask 镜像内文件 mode 与构建进程
// umask/属主解耦（与 functionsdispatcher/daemon.go tarDir 同约定、同事故链）：
// v1 模板 USER node 非 root，用户代码文件经 COPY 进镜像后必须可读。Linux 下
// 置 umask 0077 复现掩蔽形态（修复前 tar header 会带上 0600/0700）。
func TestTarDir_NormalizesModesIndependentOfUmask(t *testing.T) {
	old := setTestUmask(0o077)
	defer setTestUmask(old)

	dir := t.TempDir()
	// 写盘用收紧 mode（0600/0750）：直接模拟生产 umask 掩蔽后的磁盘状态，
	// 断言 tarDir 归一化向上收回 0644/0755（镜像内权限不依赖磁盘实际 mode）。
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("code"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "util.js"), []byte("util"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x"), 0o600))

	rd, err := tarDir(dir)
	require.NoError(t, err)
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
