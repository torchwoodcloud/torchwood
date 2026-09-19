package dispatcher

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// engines.node major 粒度相交性表驱动（docs/design/functions-runtime-
// selection.md §5）：断言的是「range 与 major M 的版本空间是否相交」，
// 不是完整 semver 求值——宽松 range 放行、排除 major 的 range 拒绝。
func TestNodeEnginesAllowsMajor(t *testing.T) {
	cases := []struct {
		engines string
		major   int
		want    bool
	}{
		// 未声明 / 通配。
		{"*", 18, true},
		{"x", 24, true},
		// bare major（x-range）。
		{"22", 22, true},
		{"22", 23, false},
		{"22.x", 22, true},
		{"22.*", 21, false},
		// 比较符。
		{">=18", 24, true},
		{">=18", 17, false},
		{">22", 22, false}, // >22 ≡ >=23.0.0
		{">22", 23, true},
		{"<24", 23, true},
		{"<24", 24, false},
		{"<=22", 22, true},
		{"<=22", 23, false},
		{"=22.4.1", 22, true},
		{"=22.4.1", 23, false},
		// caret / tilde。
		{"^22.0.0", 22, true},
		{"^22.0.0", 23, false},
		{"^22", 22, true},
		{"^22", 21, false},
		{"~22.3.4", 22, true},
		{"~22.3.4", 21, false},
		{"~22", 22, true},
		// 范围组合。
		{">=20 <24", 22, true},
		{">=20 <24", 24, false},
		{">=20 <24", 19, false},
		{">22.0 <23", 22, true},  // 22.0.1 满足 → 相交
		{">=20.5", 20, true},     // ≥20.5 ≡ >=20.5.0，20.5.0 存在
		{">=20.6 <21", 20, true}, // 20.6.0 存在
		{">=20.6 <21", 21, false},
		// 连字符 range。
		{"1.2.3 - 2.3.4", 1, true},
		{"1.2.3 - 2.3.4", 2, true},
		{"1.2.3 - 2.3.4", 3, false},
		{"1.2.3 - 2", 2, true},
		// OR 组：任一命中即放行。
		{">=14 <15 || >=18", 22, true},
		{">=14 <15 || >=18", 14, true},
		{">=14 <15 || >=18", 16, false},
		// prerelease / 构建元数据剥离。
		{"^22.0.0-beta.1", 22, true},
		{"22.0.0+build.7", 22, true},
		// 无法解析的 token：放行（护栏不是安全边界——误放只是退化回无
		// engines 现状，误拒伤部署）。
		{"latest", 18, true},
		{"= 22 ||", 18, true},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, nodeEnginesAllowsMajor(tc.engines, tc.major),
			"engines %q vs major %d", tc.engines, tc.major)
	}
}

// checkNodeEngines 接线：空声明零作用、非 node family 零作用、不相交时
// InvalidArgument 且错误文案含声明 range 与可选 runtime。
func TestCheckNodeEngines(t *testing.T) {
	// 未声明 / 通配命中：零作用。
	require.NoError(t, checkNodeEngines("", "node-24.0"))
	require.NoError(t, checkNodeEngines("*", "node-24.0"))
	// 非 node family（go / image / 未知 ID）：engines 不消费。
	require.NoError(t, checkNodeEngines("<18", "go-1.26"))
	require.NoError(t, checkNodeEngines("<18", "image"))
	require.NoError(t, checkNodeEngines("<18", "deno-2.0"))

	// 不相交 → InvalidArgument，文案含两侧值与可用 runtime 列表。
	err := checkNodeEngines(">=20 <23", "node-24.0")
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	msg := status.Convert(err).Message()
	require.Contains(t, msg, ">=20 <23")
	require.Contains(t, msg, "node-24.0")
	require.Contains(t, msg, "node-22.0", "错误文案须列出可选 node runtime")
}
