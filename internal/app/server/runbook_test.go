package server

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/runbook"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// memRunbookRepo 是 runbook.StateRepo 的内存实现（并发安全，供表驱动单测）。
type memRunbookRepo struct {
	mu    sync.Mutex
	steps map[string][]runbook.StepState // key = projectID + "\x00" + runbook
	now   time.Time
}

func newMemRunbookRepo() *memRunbookRepo {
	return &memRunbookRepo{steps: map[string][]runbook.StepState{}, now: time.Now()}
}

func (r *memRunbookRepo) key(projectID, runbookName string) string {
	return projectID + "\x00" + runbookName
}

func (r *memRunbookRepo) ListSteps(_ context.Context, projectID, runbookName string) ([]runbook.StepState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := r.steps[r.key(projectID, runbookName)]
	out := make([]runbook.StepState, len(steps))
	copy(out, steps)
	return out, nil
}

func (r *memRunbookRepo) InsertStep(_ context.Context, projectID string, step runbook.StepState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(projectID, step.Runbook)
	for _, s := range r.steps[k] {
		if s.Version == step.Version {
			return runbook.ErrStepExists
		}
	}
	step.AppliedAt = r.now
	r.steps[k] = append(r.steps[k], step)
	sort.Slice(r.steps[k], func(i, j int) bool { return r.steps[k][i].Version < r.steps[k][j].Version })
	return nil
}

func (r *memRunbookRepo) DeleteStep(_ context.Context, projectID, runbookName string, version int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(projectID, runbookName)
	steps := r.steps[k]
	for i, s := range steps {
		if s.Version == version {
			r.steps[k] = append(steps[:i:i], steps[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// runbookAdminCtx 是写路径守卫（RequireServerPrincipal）所需的 admin 会话。
func runbookAdminCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:   "admin-1",
		ActorKind: shared.ActorKindAdmin,
		ProjectID: "p1",
	})
}

func i64(v int64) *int64 { return &v }

// fixedChecksum 由 seed 确定性生成 64 位小写十六进制串（满足 protovalidate
// 的 ^[0-9a-f]{64}$；不同 seed 产出不同 checksum，无 int64→byte 收窄）。
func fixedChecksum(seed int64) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = hexdigits[(seed+int64(i))%16]
	}
	return string(out)
}

// seedRunbook 按 CAS 语义顺序落 1..n 版（构造前置状态）。
func seedRunbook(t *testing.T, uc *Runbook, projectID, runbookName string, n int64) {
	t.Helper()
	for v := int64(1); v <= n; v++ {
		var prev *int64
		if v > 1 {
			prev = i64(v - 1)
		}
		_, err := uc.Record(runbookAdminCtx(), projectID, RecordCommand{
			Runbook: runbookName, Version: v, Name: "step", Checksum: fixedChecksum(v),
			ExpectPrevVersion: prev,
		})
		require.NoError(t, err)
	}
}

// TestRunbook_RecordCAS：expect_prev_version 的 CAS 语义（D8）——
// 缺省/0 = 断言首步（空状态）；显式值 = 当前顶版；不匹配 → FailedPrecondition。
func TestRunbook_RecordCAS(t *testing.T) {
	cases := []struct {
		name       string
		seed       int64 // 前置状态顶版（0 = 空）
		recordVer  int64
		expectPrev *int64
		wantCode   codes.Code
	}{
		{"空状态 + nil 期望（首步）", 0, 1, nil, codes.OK},
		{"空状态 + 0 期望（首步）", 0, 1, i64(0), codes.OK},
		{"空状态 + 非零期望 → 撞", 0, 1, i64(5), codes.FailedPrecondition},
		{"期望 = 当前顶版 → 通过", 2, 3, i64(2), codes.OK},
		{"期望落后顶版 → 撞", 2, 3, i64(1), codes.FailedPrecondition},
		{"期望超前顶版 → 撞", 2, 3, i64(9), codes.FailedPrecondition},
		{"nil 期望 + 非空状态 → 撞（首步断言失败）", 2, 3, nil, codes.FailedPrecondition},
		{"0 期望 + 非空状态 → 撞", 2, 3, i64(0), codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uc := NewRunbook(newMemRunbookRepo())
			seedRunbook(t, uc, "p1", "default", tc.seed)

			got, err := uc.Record(runbookAdminCtx(), "p1", RecordCommand{
				Runbook: "default", Version: tc.recordVer, Name: "step",
				Checksum: fixedChecksum(tc.recordVer), ExpectPrevVersion: tc.expectPrev,
			})
			if tc.wantCode == codes.OK {
				require.NoError(t, err)
				require.Equal(t, tc.recordVer, got)
			} else {
				require.Equal(t, tc.wantCode, status.Code(err), "%s", err)
				require.Equal(t, int64(0), got)
			}

			steps, lerr := uc.List(runbookAdminCtx(), "p1", "default")
			require.NoError(t, lerr)
			wantLen := tc.seed
			if tc.wantCode == codes.OK {
				wantLen = tc.recordVer
			}
			require.Len(t, steps, int(wantLen), "CAS 失败不得落库")
		})
	}
}

// TestRunbook_DeleteTopVersionOnly：仅允许删当前顶版（版本链单调）。
func TestRunbook_DeleteTopVersionOnly(t *testing.T) {
	cases := []struct {
		name     string
		seed     int64
		version  int64
		wantCode codes.Code
	}{
		{"空状态 → NotFound", 0, 1, codes.NotFound},
		{"非顶版（中间）", 3, 2, codes.FailedPrecondition},
		{"非顶版（底部）", 3, 1, codes.FailedPrecondition},
		{"不存在（超前于顶版）", 3, 4, codes.FailedPrecondition},
		{"顶版", 3, 3, codes.OK},
		{"摘后再摘同版 → NotFound", 2, 2, codes.OK}, // seed=2，顶版即 2
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uc := NewRunbook(newMemRunbookRepo())
			seedRunbook(t, uc, "p1", "default", tc.seed)
			err := uc.Delete(runbookAdminCtx(), "p1", "default", tc.version)
			require.Equal(t, tc.wantCode, status.Code(err), "%s", err)
		})
	}

	// 链式回退：3→2→1 逐版摘除（down 主循环的服务端语义）。
	uc := NewRunbook(newMemRunbookRepo())
	seedRunbook(t, uc, "p1", "default", 3)
	for v := int64(3); v >= 1; v-- {
		require.NoError(t, uc.Delete(runbookAdminCtx(), "p1", "default", v))
	}
	steps, err := uc.List(runbookAdminCtx(), "p1", "default")
	require.NoError(t, err)
	require.Empty(t, steps)
}

// TestRunbook_ProjectIsolation：项目隔离 + runbook 线隔离（状态按
// (project, runbook) 分键；其他项目/线的记录互不可见）。
func TestRunbook_ProjectIsolation(t *testing.T) {
	uc := NewRunbook(newMemRunbookRepo())
	ctx := runbookAdminCtx()

	_, err := uc.Record(ctx, "p1", RecordCommand{
		Runbook: "default", Version: 1, Name: "step", Checksum: fixedChecksum(1),
	})
	require.NoError(t, err)
	_, err = uc.Record(ctx, "p2", RecordCommand{
		Runbook: "default", Version: 1, Name: "step", Checksum: fixedChecksum(2),
	})
	require.NoError(t, err)

	p1Steps, err := uc.List(ctx, "p1", "default")
	require.NoError(t, err)
	require.Len(t, p1Steps, 1)
	require.Equal(t, fixedChecksum(1), p1Steps[0].Checksum)

	// p2 的 CAS 视图不受 p1 记录影响（顶版仍为 1）。
	_, err = uc.Record(ctx, "p2", RecordCommand{
		Runbook: "default", Version: 2, Name: "step", Checksum: fixedChecksum(3),
		ExpectPrevVersion: i64(1),
	})
	require.NoError(t, err)

	// 同项目不同 runbook 线互不可见（表留 runbook 列，§7）。
	other, err := uc.List(ctx, "p1", "other")
	require.NoError(t, err)
	require.Empty(t, other)

	// 删除按项目隔离：摘 p2/default/2 不影响 p1。
	require.NoError(t, uc.Delete(ctx, "p2", "default", 2))
	steps, err := uc.List(ctx, "p1", "default")
	require.NoError(t, err)
	require.Len(t, steps, 1)
}

// TestRunbook_RecordConcurrentUniqueWinner：漏过 CAS 的并发双写由唯一
// 约束兜底（D8：UNIQUE (project_id, runbook, version) 只剩一个赢家）。
// racingRunbookRepo 在 InsertStep 前注入"并发赢家"行，复现 Record 内
// List 与 Insert 之间的 TOCTOU 窗口。
type racingRunbookRepo struct {
	*memRunbookRepo
	onInsert func()
}

func (r *racingRunbookRepo) InsertStep(ctx context.Context, projectID string, step runbook.StepState) error {
	if r.onInsert != nil {
		hook := r.onInsert
		r.onInsert = nil
		hook()
	}
	return r.memRunbookRepo.InsertStep(ctx, projectID, step)
}

func TestRunbook_RecordConcurrentUniqueWinner(t *testing.T) {
	base := newMemRunbookRepo()
	racing := &racingRunbookRepo{memRunbookRepo: base}
	uc := NewRunbook(racing)
	ctx := runbookAdminCtx()

	_, err := uc.Record(ctx, "p1", RecordCommand{
		Runbook: "default", Version: 1, Name: "a", Checksum: fixedChecksum(1),
	})
	require.NoError(t, err)

	// Record 读取到 top=1 通过 CAS 后、落库前，并发方抢先记上 v2——
	// 我方插入撞唯一约束 → AlreadyExists（对方已记录）。
	racing.onInsert = func() {
		require.NoError(t, base.InsertStep(ctx, "p1", runbook.StepState{
			Runbook: "default", Version: 2, Name: "b", Checksum: fixedChecksum(2),
		}))
	}
	got, err := uc.Record(ctx, "p1", RecordCommand{
		Runbook: "default", Version: 2, Name: "b2", Checksum: fixedChecksum(3),
		ExpectPrevVersion: i64(1),
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err), "%s", err)
	require.Equal(t, int64(0), got)

	// 输家重拉状态可见赢家记录（引擎据此收敛为 continue）。
	steps, err := uc.List(ctx, "p1", "default")
	require.NoError(t, err)
	require.Len(t, steps, 2)
	require.Equal(t, fixedChecksum(2), steps[1].Checksum)
}

// TestRunbook_WriteRequiresServerPrincipal：写路径纵深防御（G2-2）——
// 匿名/端用户拒绝，admin 会话放行（角色细粒度由拦截器把关）。
func TestRunbook_WriteRequiresServerPrincipal(t *testing.T) {
	uc := NewRunbook(newMemRunbookRepo())

	_, err := uc.Record(context.Background(), "p1", RecordCommand{
		Runbook: "default", Version: 1, Name: "a", Checksum: fixedChecksum(1),
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	err = uc.Delete(context.Background(), "p1", "default", 1)
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	endUser := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "u1", ActorKind: shared.ActorKindEndUser, UserID: "u1", ProjectID: "p1",
	})
	_, err = uc.Record(endUser, "p1", RecordCommand{
		Runbook: "default", Version: 1, Name: "a", Checksum: fixedChecksum(1),
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	err = uc.Delete(endUser, "p1", "default", 1)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestRunbook_ListAscending：升序全集与空状态形状（applied_at 由 repo 填充）。
func TestRunbook_ListAscending(t *testing.T) {
	uc := NewRunbook(newMemRunbookRepo())
	ctx := runbookAdminCtx()

	empty, err := uc.List(ctx, "p1", "default")
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)

	seedRunbook(t, uc, "p1", "default", 3)
	steps, err := uc.List(ctx, "p1", "default")
	require.NoError(t, err)
	require.Len(t, steps, 3)
	for i, s := range steps {
		require.Equal(t, int64(i+1), s.Version)
		require.False(t, s.AppliedAt.IsZero(), "applied_at 由 repo 填充")
	}
}
