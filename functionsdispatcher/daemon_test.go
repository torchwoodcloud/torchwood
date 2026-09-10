package functionsdispatcher

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
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
	old := setTestUmask(0o077)
	defer setTestUmask(old)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".tw-runner.js"), []byte("runner"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("code"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "util.js"), []byte("util"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM x"), 0o644))

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
