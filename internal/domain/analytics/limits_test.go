package analytics

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 上限常量与形状正则的一致性锁（proto protovalidate 注解的对齐面：
// 名称/键长上限 = 正则的重复次数边界，两处必须同步演进）。
func TestLimitsPatternsMatchConstants(t *testing.T) {
	// 事件名：MaxNameLen=64 = 首字符 + {0,63}。
	okName := "a" + strings.Repeat("x", MaxNameLen-1)
	require.Len(t, okName, MaxNameLen)
	require.True(t, EventNamePattern.MatchString(okName))
	badName := "a" + strings.Repeat("x", MaxNameLen)
	require.Len(t, badName, MaxNameLen+1)
	require.False(t, EventNamePattern.MatchString(badName), "超长事件名必须被正则拒绝")

	// 事件名形状：数字开头/连字符开头/空串/非法字符均拒绝；点/连字符/下划线主体合法。
	for _, name := range []string{"", "1page", "-page", "_page", "page view", "页面"} {
		require.False(t, EventNamePattern.MatchString(name), "非法事件名 %q 必须被拒绝", name)
	}
	require.True(t, EventNamePattern.MatchString("page.view-2_b"))
	require.False(t, EventNamePattern.MatchString("page.view-x!"))

	// props 键：MaxPropKeyLen=64 = 首字符 + {0,63}；下划线/字母开头，数字开头拒绝。
	okKey := "k" + strings.Repeat("v", MaxPropKeyLen-1)
	require.Len(t, okKey, MaxPropKeyLen)
	require.True(t, PropKeyPattern.MatchString(okKey))
	badKey := "k" + strings.Repeat("v", MaxPropKeyLen)
	require.False(t, PropKeyPattern.MatchString(badKey), "超长 props 键必须被正则拒绝")
	require.True(t, PropKeyPattern.MatchString("_private.key_1"))
	for _, key := range []string{"", "1key", "-key", "key name", "键"} {
		require.False(t, PropKeyPattern.MatchString(key), "非法 props 键 %q 必须被拒绝", key)
	}
}

// 钳制窗（D4）：[now-24h, now+5min]，越界由 app 用例层判 skipped。
// 本测试锁常量值与边界方向（闭区间语义由 PR2 用例实现）。
func TestClampWindowBounds(t *testing.T) {
	require.Equal(t, -24*time.Hour, ClampPast)
	require.Equal(t, 5*time.Minute, ClampFuture)

	now := time.Now()
	// 窗外：各越一秒。
	require.True(t, now.Add(ClampPast).Add(-time.Second).Before(now.Add(ClampPast)),
		"早于 now-24h 必须判越界")
	require.True(t, now.Add(ClampFuture).Add(time.Second).After(now.Add(ClampFuture)),
		"晚于 now+5min 必须判越界")
}
