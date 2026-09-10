// B13a（upsert 预锁冲突值键，§4.8 裁决④挂账）：死锁注入用例——两个并发批
// 以相反顺序 upsert 相互冲突的冲突值键（批 A: X→Y、批 B: Y→X），重复 N 轮
// 统计 PG 死锁中止（40P01）发生频率。判据：频率非零 → 实现预锁 + 测试转绿；
// 频率为零 → 维持"PG 死锁检测 + 幂等重试"决策。
//
// 互锁机理（裁决④的窗口）：lockTxTargets 已按 (collection, documentID) 排序
// 预锁——但 upsert 的真锁键是 upsertDocument 内部的**冲突值键** advisory
// lock，两批的 documentID 集合不相交时批间零互斥，冲突值锁按 op 请求序获取，
// 相反顺序即成环。测得非零频率后，修复 = lockTxTargets 把批内全部 upsert 的
// 冲突值键**排序后预锁**（全局一致序 → 无环；upsertDocument 内的取锁在事务
// 内可重入，语义不变）。
package documentdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// isPGDeadlock 报告错误链是否携带 PG 40P01（deadlock_detected）。
func isPGDeadlock(err error) bool {
	var fielder pgErrorFielder
	if !errors.As(err, &fielder) {
		return false
	}
	return fielder.Field('C') == "40P01"
}

// TestExecuteTransactions_UpsertConflictKeyDeadlockInjection：并发批以相反
// 冲突值键顺序 upsert，N 轮统计 40P01。修复后判据 = 全轮零死锁中止
// （控制组：同序两批天然零死锁）。
func TestExecuteTransactions_UpsertConflictKeyDeadlockInjection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "locks", "Locks",
		[]databases.Attribute{
			{ID: "code", Key: "code", Type: "string", Size: 64},
			{ID: "n", Key: "n", Type: "integer"},
		},
		[]databases.Index{{ID: "code_key", Type: "unique", Attributes: []string{"code"}}},
		nil, true))

	// 两批 documentID 集合不相交（lockTxTargets 的 docID 预锁互不干扰），
	// 冲突值键相反（A: X→Y，B: Y→X）。
	batchA := []databases.TransactionOp{
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-a",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "X", "n": int64(1)}},
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-b",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "Y", "n": int64(1)}},
	}
	batchB := []databases.TransactionOp{
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-c",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "Y", "n": int64(2)}},
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-d",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "X", "n": int64(2)}},
	}
	// 控制组：同序（B 批改为 X→Y）——排序预锁语义下天然零死锁。
	batchBSameOrder := []databases.TransactionOp{
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-c",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "X", "n": int64(2)}},
		{Type: databases.TransactionOpUpsert, CollectionID: "locks", DocumentID: "doc-d",
			ConflictColumns: []string{"code"}, Data: map[string]any{"code": "Y", "n": int64(2)}},
	}

	const rounds = 8
	runConcurrent := func(a, b []databases.TransactionOp) (deadlocks int, errs []error) {
		for i := 0; i < rounds; i++ {
			var wg sync.WaitGroup
			results := make([]error, 2)
			for j, batch := range [][]databases.TransactionOp{a, b} {
				wg.Add(1)
				go func(j int, batch []databases.TransactionOp) {
					defer wg.Done()
					_, err := docDB.ExecuteTransactions(ctx, projectID, "app", batch,
						databases.TransactionModeAtomic, databases.SystemPrincipal)
					results[j] = err
				}(j, batch)
			}
			wg.Wait()
			deadlocked := false
			for _, err := range results {
				if err == nil {
					continue
				}
				errs = append(errs, err)
				if isPGDeadlock(err) {
					deadlocked = true
				}
			}
			if deadlocked {
				deadlocks++
			}
		}
		return deadlocks, errs
	}

	opDeadlocks, opErrs := runConcurrent(batchA, batchB)
	t.Logf("opposite-order: %d/%d rounds hit PG 40P01 (errs=%d)", opDeadlocks, rounds, len(opErrs))
	for _, err := range opErrs {
		t.Logf("opposite-order error sample: %v", err)
		break
	}

	sameDeadlocks, sameErrs := runConcurrent(batchA, batchBSameOrder)
	t.Logf("same-order control: %d/%d rounds hit PG 40P01 (errs=%d)", sameDeadlocks, rounds, len(sameErrs))
	for _, err := range sameErrs {
		t.Logf("same-order error sample: %v", err)
		break
	}

	// 修复判据（预锁落地后锁定）：两个形态全轮零死锁中止——并发批可能以
	// 其他错误（或成功）收场，但 40P01 必须绝迹。
	require.Zerof(t, opDeadlocks, "opposite-order must not deadlock (errs=%v)", opErrs)
	require.Zerof(t, sameDeadlocks, "same-order control must not deadlock (errs=%v)", sameErrs)

	// 幂等收敛：全部轮次落库后，冲突键各自命中唯一行（重复 upsert 覆盖同值）。
	count, err := docDB.CountDocuments(ctx, projectID, "app", "locks", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.LessOrEqual(t, count, int64(4), fmt.Sprintf("each conflict key must resolve to one row, got %d", count))
}
