package ident

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateSchemaResourceID_Valid(t *testing.T) {
	for _, id := range []string{"a", "default", "shop", "app", "cms", "acmeprodshop2026", strings.Repeat("a", 28)} {
		require.NoError(t, ValidateSchemaResourceID(id), id)
	}
}

func TestValidateSchemaResourceID_Invalid(t *testing.T) {
	for _, id := range []string{
		"",
		"A",
		"Shop",
		"1a",
		"1shop",
		"my-shop",
		"my_shop",
		"shop.app",
		"应用",
		"my shop",
		strings.Repeat("a", 29),
		// sentinel "_" 非 SchemaResourceID（charset 拒绝：不以小写字母开头）。
		// PR1 必须补：ProjectDataPlaneID 不得通过 ValidateSchemaResourceID。
		ProjectDataPlaneID,
	} {
		err := ValidateSchemaResourceID(id)
		require.Error(t, err, id)
		require.ErrorIs(t, err, ErrInvalidSchemaResourceID, id)
		require.Equal(t, errSchemaResourceID, err.Error(), id)
	}
}

func TestSchemaName(t *testing.T) {
	got, err := SchemaName("shop", "app")
	require.NoError(t, err)
	require.Equal(t, "tw_shop_app", got)

	got, err = SchemaName("default", "default")
	require.NoError(t, err)
	require.Equal(t, "tw_default_default", got)
}

func TestSchemaName_RejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		project, database string
	}{
		{"Shop", "app"},
		{"shop", "my_app"},
		{"my-shop", "app"},
		{"", "app"},
		{"shop", ""},
		{strings.Repeat("a", 29), "app"},
	} {
		got, err := SchemaName(tc.project, tc.database)
		require.Error(t, err, tc)
		require.ErrorIs(t, err, ErrInvalidSchemaResourceID, tc)
		require.Empty(t, got, tc)
	}
}

func TestProjectSchemaName(t *testing.T) {
	got, err := ProjectSchemaName("shop")
	require.NoError(t, err)
	require.Equal(t, "tw_shop", got)

	// "default" 作为 project id 合法（项目也可以叫 default）。
	got, err = ProjectSchemaName("default")
	require.NoError(t, err)
	require.Equal(t, "tw_default", got)

	// 长度上限：tw_(3) + 28 = 31。
	got, err = ProjectSchemaName(strings.Repeat("a", 28))
	require.NoError(t, err)
	require.Equal(t, "tw_"+strings.Repeat("a", 28), got)
}

func TestProjectSchemaName_RejectsInvalid(t *testing.T) {
	for _, id := range []string{
		"",
		"Shop",
		"my_shop",
		"my-shop",
		"1shop",
		strings.Repeat("a", 29),
		"应用",
	} {
		got, err := ProjectSchemaName(id)
		require.Error(t, err, id)
		require.Empty(t, got, id)
	}
}

func TestProjectDataPlaneID_IsIllegalSchemaResourceID(t *testing.T) {
	// sentinel "_" 非 SchemaResourceID：charset 拒绝（不以小写字母开头）。
	err := ValidateSchemaResourceID(ProjectDataPlaneID)
	require.Error(t, err)
	require.Equal(t, ProjectDataPlaneID, "_")

	// SchemaName(project, "_") 必然失败：sentinel 过不了 ValidateSchemaResourceID。
	got, err := SchemaName("shop", ProjectDataPlaneID)
	require.Error(t, err)
	require.Empty(t, got)
}

func TestOneAndTwoSegmentSchemasAreDisjoint(t *testing.T) {
	// 一段式 ProjectSchemaName 的返回值不得匹配两段式正则（反之亦然）。
	one, err := ProjectSchemaName("shop")
	require.NoError(t, err)
	require.False(t, IsTwoSegmentSchema(one), one)

	// 两段式业务 schema 不得被误判为一段式：tw_shop_app 含两道下划线。
	two, err := SchemaName("shop", "app")
	require.NoError(t, err)
	require.True(t, IsTwoSegmentSchema(two), two)

	// LIKE 陷阱防护：一段式 tw_shop 是两段式 tw_shop_default / tw_shop_app 的前缀，
	// 但 IsTwoSegmentSchema 对 tw_shop 返回 false，对两段式返回 true。
	require.False(t, IsTwoSegmentSchema("tw_shop"), "tw_shop must not be two-segment")
	require.True(t, IsTwoSegmentSchema("tw_shop_default"), "tw_shop_default must be two-segment")
	require.True(t, IsTwoSegmentSchema("tw_shop_app"), "tw_shop_app must be two-segment")
}

func TestSchemaName_SentinelNeverProducesOneSegment(t *testing.T) {
	// 防御：即便某处误用 SchemaName(project, "_")，也应失败而非退化为一段式。
	// 这里覆盖 charset 拒绝路径；DDL 分叉（businessSchema）是第二层防线（PR2）。
	for _, project := range []string{"shop", "default", "a"} {
		got, err := SchemaName(project, ProjectDataPlaneID)
		require.Error(t, err, project)
		require.Empty(t, got, project)
		// ProjectSchemaName 产出的名字与等价的两段式（project==sentinel）无关联。
		ps, perr := ProjectSchemaName(project)
		require.NoError(t, perr)
		require.NotEqual(t, got, ps)
	}
}

// TestSchemaResourceID_ValidAsMessageloopNamespace 锁定跨仓契约不变量（T5）：
// project id 被下游 messageloop 当 namespace 使用，其语法为
// [a-z0-9-]{1,32} 且不以 '-' 开头/结尾。当前 ValidateSchemaResourceID
// （^[a-z][a-z0-9]{0,27}$：小写字母开头、仅小写字母/数字、≤28）是该语法
// 的严格子集——本测试防止未来放宽 ID 规则时无声破坏 messageloop 侧的
// namespace 语法门（签出非法 namespace 会被清洗为零范围 Key）。
func TestSchemaResourceID_ValidAsMessageloopNamespace(t *testing.T) {
	for _, id := range []string{"a", "default", "acme", "acmeprodshop2026", strings.Repeat("a", 28)} {
		require.NoError(t, ValidateSchemaResourceID(id), id)
		// 逐条独立断言 messageloop namespace 语法（不 import 对端包，双实现
		// 对照锁定）。
		require.LessOrEqual(t, len(id), 32, id)
		require.NotEqual(t, '-', id[0], id)
		require.NotEqual(t, '-', id[len(id)-1], id)
		for _, ch := range id {
			require.Contains(t, "abcdefghijklmnopqrstuvwxyz0123456789-", string(ch), id)
		}
	}
}
