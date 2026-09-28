package dispatcher

import (
	"archive/zip"
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// digest64 构造恒定 64 hex 字符的 sha256 digest 形态字符串（测试可读性：
// digest64("cd") = "sha256:cdcd..." 共 64 字符）。
func digest64(seed string) string {
	s := strings.ToLower(seed)
	for len(s) < 64 {
		s += s
	}
	return "sha256:" + s[:64]
}

// 测试 zip 构造助手（原 daemon_integration_test.go 的共享夹具，随 docker
// 集成测试删除后保留最小面——prepareBuildContext 的表驱动用例仍消费）。

// makeEntryZip 构造单文件最小 zip。
func makeEntryZip(t *testing.T, name, code string) []byte {
	t.Helper()
	return makeEntryZipFiles(t, map[string]string{name: code})
}

// makeEntryZipFiles 构造含多文件（含子目录路径）的最小 zip：镜像权限回归
// 需要同时覆盖文件读取与子目录遍历（目录 x 位）。
func makeEntryZipFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(files[name]))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
