package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVendoredAuthzProtoInSyncWithLibrary（grpcapi 阶段 1 守卫）：vendored 的
// grpcapi/v1/authz.proto 必须与库仓库本体字节一致，防止 replace 依赖升级后
// proto 注解契约静默漂移（genproto 生成代码与 vendored proto 分属两侧）。
// 库仓库不在位（CI 单仓环境，go.mod replace 仅本地存在）时跳过。
func TestVendoredAuthzProtoInSyncWithLibrary(t *testing.T) {
	t.Parallel()

	// go test 的 cwd 是包目录（cmd/server/internal/runtime）：4 级上溯到
	// torchwood 仓库根，5 级到父目录（grpcapi 与 torchwood 同级检出）。
	vendored := filepath.Join("..", "..", "..", "..",
		"proto", "third_party", "grpcapi", "v1", "authz.proto")
	library := filepath.Join("..", "..", "..", "..", "..",
		"grpcapi", "proto", "grpcapi", "v1", "authz.proto")

	if _, err := os.Stat(library); err != nil {
		t.Skipf("grpcapi 库仓库不在位（单仓 CI 环境），跳过 vendored proto 同步守卫: %v", err)
	}

	vendoredBytes, err := os.ReadFile(vendored)
	require.NoError(t, err, "vendored proto 读取失败: %s", vendored)
	libraryBytes, err := os.ReadFile(library)
	require.NoError(t, err, "库 proto 读取失败: %s", library)

	require.True(t, bytes.Equal(vendoredBytes, libraryBytes),
		"vendored proto 与 grpcapi 库本体漂移：\n  vendored: %s\n  library:  %s\n请同步 proto/third_party/grpcapi/v1/authz.proto（随库版本升级同 PR）",
		vendored, library)
}
