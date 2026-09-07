package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
)

// TestAuthzMatrixDoc_NoDrift CI 锁：重新渲染的授权矩阵文档必须与磁盘上的
// docs/developer/authz-matrix.md 逐字节一致。改了 proto 策略声明后需运行
// `task gen:authz-matrix` 重新生成并随同策略变更一起提交（漂移即红）。
func TestAuthzMatrixDoc_NoDrift(t *testing.T) {
	t.Parallel()

	set, err := BuildMethodPolicies(authzFileDescriptors()...)
	require.NoError(t, err)

	rendered, err := RenderAuthzMatrix(set)
	require.NoError(t, err)

	disk, err := os.ReadFile(filepath.Join("..", "..", "docs", "developer", "authz-matrix.md"))
	require.NoError(t, err, "docs/developer/authz-matrix.md 不存在：请运行 task gen:authz-matrix 生成初版")

	require.Equal(t, string(disk), string(rendered),
		"授权矩阵文档与策略注册表漂移：请运行 task gen:authz-matrix 并提交更新后的 docs/developer/authz-matrix.md")
}

// TestAuthzMatrixDoc_RenderRejectsEmpty 防空洞：nil/空注册表必须报错，
// 防止静默生成空文档覆盖磁盘。
func TestAuthzMatrixDoc_RenderRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := RenderAuthzMatrix(nil)
	require.Error(t, err)

	empty, err := domainauth.NewPolicySet(nil)
	require.NoError(t, err)
	_, err = RenderAuthzMatrix(empty)
	require.Error(t, err)
}
