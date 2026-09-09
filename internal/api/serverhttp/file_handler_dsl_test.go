package serverhttp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// TestPublicBucketDSLBuildEqual 公开桶匿名读路径的 bucketID DSL 必须经
// query.BuildEqual 转义构造（file_handler.go resolveReadContext）：
// 含引号/反斜杠/换行的恶意 bucketID 不能逃逸出 equal 参数或注入第二个过滤条件。
func TestPublicBucketDSLBuildEqual(t *testing.T) {
	malicious := `x",equal("public","true");limit(1)//`
	dsl := query.BuildEqual("$id", malicious)

	parsed, err := query.Parse(dsl)
	require.NoError(t, err)
	require.Len(t, parsed.Filters, 1, "恶意值不得拆成多个过滤条件")
	require.Equal(t, "equal", parsed.Filters[0].Op)
	require.Equal(t, "$id", parsed.Filters[0].Attribute)
	require.Equal(t, []string{malicious}, parsed.Filters[0].Values, "值必须完整还原为原始 bucketID")

	// 普通 UUID 正常构造。
	dsl = query.BuildEqual("$id", "3fa85f64-5717-4562-b3fc-2c963f66afa6")
	parsed, err = query.Parse(dsl)
	require.NoError(t, err)
	require.Equal(t, "3fa85f64-5717-4562-b3fc-2c963f66afa6", parsed.Filters[0].Values[0])
}

// TestIsValidBucketID（G4-3）：公开 bucket 匿名路径在 resolve 前校验 bucketID
// 格式（非空 + 字符集/长度），DSL 注入串/空白/超长一律拒绝。
func TestIsValidBucketID(t *testing.T) {
	valid := []string{
		"3fa85f64-5717-4562-b3fc-2c963f66afa6",
		"bucket-01_abc",
		"BUCKET42",
	}
	for _, id := range valid {
		require.Truef(t, isValidBucketID(id), "应合法: %q", id)
	}

	invalid := []string{
		"",
		"   ",
		`x",equal("public","true");limit(1)//`,
		"a b",
		"a/b",
		"a\\b",
		"a\nb",
		strings.Repeat("a", 65),
	}
	for _, id := range invalid {
		require.Falsef(t, isValidBucketID(id), "应非法: %q", id)
	}
}
