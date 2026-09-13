package leaderboards

import (
	"testing"

	"github.com/stretchr/testify/require"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestApplyCreateDefaults(t *testing.T) {
	t.Parallel()
	in := &domainleaderboards.Board{ID: "daily_best"}
	applyCreateDefaults(in)
	require.Equal(t, domainleaderboards.SortDesc, in.Sort)
	require.Equal(t, domainleaderboards.TieBreakParallel, in.TieBreak)
	require.Equal(t, domainleaderboards.PolicyBest, in.Policy)
	require.Equal(t, domainleaderboards.PeriodNone, in.PeriodKind)
	require.Equal(t, int32(domainleaderboards.DefaultPerSubjectLimit), in.PerSubjectLimit)
	require.Equal(t, "user", in.SubjectKind)

	// 显式值不覆盖。
	in2 := &domainleaderboards.Board{ID: "b", Sort: domainleaderboards.SortAsc, Policy: domainleaderboards.PolicySum, PerSubjectLimit: 7}
	applyCreateDefaults(in2)
	require.Equal(t, domainleaderboards.SortAsc, in2.Sort)
	require.Equal(t, domainleaderboards.PolicySum, in2.Policy)
	require.Equal(t, int32(7), in2.PerSubjectLimit)
}

func TestBoardConfigDiff_Equal(t *testing.T) {
	t.Parallel()
	want := &domainleaderboards.Board{
		ID: "daily_best", Sort: domainleaderboards.SortDesc, TieBreak: domainleaderboards.TieBreakParallel,
		PeriodKind: domainleaderboards.PeriodDaily, PeriodTZ: "Asia/Shanghai", Policy: domainleaderboards.PolicyBest,
		PerSubjectLimit: domainleaderboards.DefaultPerSubjectLimit, SubjectKind: "user",
	}
	got := *want
	// rewards / 时间戳不在比较集：console 侧 rewards 编辑与时间戳不影响重放。
	got.Rewards = []domainleaderboards.RewardRule{{AssetCode: "coin", Amount: 5}}
	got.CreatedAt = got.CreatedAt.AddDate(0, 0, 1)
	require.Empty(t, boardConfigDiff(want, &got))
}

func TestBoardConfigDiff_FieldMismatches(t *testing.T) {
	t.Parallel()
	base := func() *domainleaderboards.Board {
		return &domainleaderboards.Board{
			ID: "b", Sort: domainleaderboards.SortDesc, TieBreak: domainleaderboards.TieBreakParallel,
			PeriodKind: domainleaderboards.PeriodDaily, PeriodTZ: "Asia/Shanghai", Policy: domainleaderboards.PolicyBest,
			ValueMax:         nil,
			ClientSubmit:     true,
			PerSubjectLimit:  100,
			RetentionPeriods: 12,
			SubjectKind:      "user",
		}
	}

	cases := []struct {
		name     string
		mutate   func(b *domainleaderboards.Board)
		contains string
	}{
		{"sort", func(b *domainleaderboards.Board) { b.Sort = domainleaderboards.SortAsc }, "sort: requested desc, existing asc"},
		{"tiebreak_order added", func(b *domainleaderboards.Board) {
			v := domainleaderboards.SortAsc
			b.TiebreakOrder = &v
		}, "tiebreak_order: requested <unset>, existing asc"},
		{"period_tz", func(b *domainleaderboards.Board) { b.PeriodTZ = "UTC" }, "period_tz: requested Asia/Shanghai, existing UTC"},
		{"policy", func(b *domainleaderboards.Board) { b.Policy = domainleaderboards.PolicySum }, "policy: requested best, existing sum"},
		{"value_max set", func(b *domainleaderboards.Board) {
			v := int64(500)
			b.ValueMax = &v
		}, "value_max: requested <unset>, existing 500"},
		{"client_submit", func(b *domainleaderboards.Board) { b.ClientSubmit = false }, "client_submit: requested true, existing false"},
		{"per_subject_submit_limit", func(b *domainleaderboards.Board) { b.PerSubjectLimit = 50 }, "per_subject_submit_limit: requested 100, existing 50"},
		{"retention_periods", func(b *domainleaderboards.Board) { b.RetentionPeriods = 6 }, "retention_periods: requested 12, existing 6"},
		{"subject_kind", func(b *domainleaderboards.Board) { b.SubjectKind = "team" }, "subject_kind: requested user, existing team"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := base()
			tc.mutate(got)
			diffs := boardConfigDiff(base(), got)
			require.Len(t, diffs, 1)
			require.Contains(t, diffs[0], tc.contains)
		})
	}
}

func TestResolveExisting(t *testing.T) {
	t.Parallel()
	want := &domainleaderboards.Board{ID: "b", Sort: domainleaderboards.SortDesc, TieBreak: domainleaderboards.TieBreakParallel, Policy: domainleaderboards.PolicyBest, PeriodKind: domainleaderboards.PeriodNone, PerSubjectLimit: 100, SubjectKind: "user"}

	// 相等 → 返回现状（重放 200）。
	existing := *want
	out, err := resolveExisting("b", want, &existing)
	require.NoError(t, err)
	require.Same(t, &existing, out)

	// 不等 → ALREADY_EXISTS 且消息带字段 diff（漂移信号）。
	drifted := *want
	drifted.PerSubjectLimit = 50
	_, err = resolveExisting("b", want, &drifted)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.AlreadyExists, st.Code())
	require.Contains(t, st.Message(), "per_subject_submit_limit: requested 100, existing 50")
}
