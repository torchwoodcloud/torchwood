package idgen

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSnowflake_ClockRollbackWaitsUntilCatchUp 时钟回拨防护（S7 缺陷 3 回归）：
// 回拨窗口内（now < lastMs）不得按回拨后的时钟发号——自旋等待系统时钟追平
// lastMs 再放行。旧实现在 now != lastMs 分支直接 sequence=0、lastMs=now，
// 回拨后与回拨前同毫秒同节点可发出完全相同的 ID（主键冲突）。同包测试直接
// 操纵私有 lastMs 字段模拟回拨。
func TestSnowflake_ClockRollbackWaitsUntilCatchUp(t *testing.T) {
	sf, err := NewSnowflake(1)
	require.NoError(t, err)

	idBefore := sf.Next()

	// 模拟时钟回拨：把 lastMs 拨到未来 ~50ms（等效于系统时钟从 lastMs 回退
	// 到当前——回拨窗口内 now < lastMs）。
	sf.mu.Lock()
	target := time.Now().Add(50 * time.Millisecond).UnixMilli()
	sf.lastMs = target
	sf.mu.Unlock()

	start := time.Now()
	idDuring := sf.Next()
	elapsed := time.Since(start)

	// 回拨窗口内不立即放行：自旋等待追平（耗时接近回拨幅度）。
	require.GreaterOrEqual(t, elapsed, 40*time.Millisecond,
		"回拨窗口内必须等待追平而非按回拨后的时钟发号")
	require.NotEqual(t, idBefore, idDuring, "回拨窗口内不得发出与回拨前相同的 ID")
	// 时间字段不回退：不低于回拨前的 lastMs（旧实现按回拨后时钟直接发号，
	// ms 必然小于 target——本断言即缺陷回归锚点）。
	ms := (idDuring>>(snowflakeNodeBits+snowflakeSeqBits)) + snowflakeEpochMs
	require.GreaterOrEqual(t, ms, target, "追平前不得发出时间字段早于 lastMs 的 ID")

	// 追平后恢复：连续发号保持唯一。
	seen := map[int64]struct{}{idBefore: {}, idDuring: {}}
	for i := 0; i < 100; i++ {
		id := sf.Next()
		require.NotContains(t, seen, id)
		seen[id] = struct{}{}
	}
}
