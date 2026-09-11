package leaderboards

import "time"

// MergeSubmit 计算同一 (board, period, subject) 重复提交的合并结果。
// improved 报告排序位置是否改善：best 仅在 (value, tiebreak_value) 字典序
// 严格变优时为 true——updated_at 只在 improved 时推进，保证 earliest 语义
// （"最早达成当前名次"）不被重复提交污染。
func MergeSubmit(b *Board, cur *Entry, value int64, tb *int64, now time.Time) (newValue int64, newTb *int64, improved bool) {
	switch b.Policy {
	case PolicyLatest:
		return value, copyTiebreak(tb), true
	case PolicySum:
		merged := cur.Value + value
		var nextTb *int64
		if tb != nil {
			nextTb = tb
		} else {
			nextTb = cur.TiebreakValue
		}
		return merged, nextTb, true
	default: // PolicyBest：方向跟随 sort
		if valueBetter(b.Sort, value, cur.Value) {
			return value, copyTiebreak(tb), true
		}
		if value == cur.Value && b.HasTiebreak() && tb != nil && cur.TiebreakValue != nil {
			if tiebreakBetter(*b.TiebreakOrder, *tb, *cur.TiebreakValue) {
				return value, copyTiebreak(tb), true
			}
		}
		return cur.Value, cur.TiebreakValue, false
	}
}

func valueBetter(dir SortDirection, candidate, current int64) bool {
	if dir == SortAsc {
		return candidate < current
	}
	return candidate > current
}

func tiebreakBetter(dir SortDirection, candidate, current int64) bool {
	if dir == SortAsc {
		return candidate < current
	}
	return candidate > current
}

func copyTiebreak(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}
