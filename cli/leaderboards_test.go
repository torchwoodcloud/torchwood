package cli

import (
	"flag"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// testInt64/testInt/testBool 把测试用例里的旗标字符串解析为构造器入参
// （空串解析失败归零值，与 flag 缺省行为一致）。
func testInt64(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
func testInt(s string) int     { v, _ := strconv.ParseInt(s, 10, 64); return int(v) }
func testBool(s string) bool   { b, _ := strconv.ParseBool(s); return b }

func TestBuildSubmitLeaderboardReq(t *testing.T) {
	tests := []struct {
		name        string
		boardID     string
		subjectID   string
		setValue    string
		value       int64
		setTiebreak bool
		tiebreak    int64
		period      string
		requestID   string
		wantErr     string
	}{
		{name: "缺 board-id", subjectID: "u1", wantErr: "missing board-id/subject-id"},
		{name: "缺 subject-id", boardID: "b1", wantErr: "missing board-id/subject-id"},
		{name: "未显式传 --value", boardID: "b1", subjectID: "u1", wantErr: "--value is required"},
		{name: "零分显式传入合法", boardID: "b1", subjectID: "u1", setValue: "0", wantErr: ""},
		{name: "全字段", boardID: "b1", subjectID: "u1", setValue: "100", value: 100,
			setTiebreak: true, tiebreak: 7, period: "2026-09", requestID: "req-1", wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := map[string]string{}
			if tt.setValue != "" {
				set["value"] = tt.setValue
			}
			if tt.setTiebreak {
				set["tiebreak-value"] = "7"
			}
			v := newPresenceVerb(t, func(fs *flag.FlagSet) {
				fs.Int64("value", 0, "")
				fs.Int64("tiebreak-value", 0, "")
			}, set)
			req, err := buildSubmitLeaderboardReq(v, tt.boardID, tt.subjectID, tt.value, tt.tiebreak, tt.period, tt.requestID)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.boardID, req["boardId"])
			require.Equal(t, tt.subjectID, req["subjectId"])
			require.Equal(t, tt.value, req["value"])
			if tt.setTiebreak {
				require.Equal(t, tt.tiebreak, req["tiebreakValue"])
			} else {
				_, ok := req["tiebreakValue"]
				require.False(t, ok, "未显式传 --tiebreak-value 不应设置键: %v", req)
			}
			if tt.period != "" {
				require.Equal(t, tt.period, req["period"])
			}
			if tt.requestID != "" {
				require.Equal(t, tt.requestID, req["requestId"])
			}
		})
	}
}

func TestBuildCreateBoardReq(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		cfg     boardConfig
		wantErr string
		want    map[string]any
	}{
		{name: "缺 id", wantErr: "missing board-id"},
		{name: "最小配置", id: "daily_wins", want: map[string]any{"id": "daily_wins"}},
		{name: "全配置", id: "daily_wins", cfg: boardConfig{
			sort: "asc", tiebreakOrder: "desc", tieBreak: "earliest",
			periodKind: "daily", periodTZ: "Asia/Shanghai", policy: "sum",
			subjectKind: "user", valueMin: -5, valueMax: 100,
			clientSubmit: true, perSubjectSubmitLimit: 10, retentionPeriods: 12,
		}, want: map[string]any{
			"id": "daily_wins", "sort": "asc", "tiebreakOrder": "desc", "tieBreak": "earliest",
			"periodKind": "daily", "periodTz": "Asia/Shanghai", "policy": "sum",
			"subjectKind": "user", "valueMin": int64(-5), "valueMax": int64(100),
			"clientSubmit": true, "perSubjectSubmitLimit": 10, "retentionPeriods": 12,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateBoardReq(tt.id, tt.cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, req)
		})
	}
}

func TestBuildUpdateBoardReq(t *testing.T) {
	tests := []struct {
		name             string
		id               string
		set              map[string]string
		clearTiebreak    bool
		clearValueBounds bool
		wantErr          string
		want             map[string]any
	}{
		{name: "缺 id", wantErr: "missing board-id"},
		{name: "仅 id（零改动）", id: "b1", want: map[string]any{"boardId": "b1"}},
		{name: "改 policy", id: "b1", set: map[string]string{"policy": "latest"},
			want: map[string]any{"boardId": "b1", "policy": "latest"}},
		{name: "显式清空", id: "b1", clearTiebreak: true, clearValueBounds: true,
			want: map[string]any{"boardId": "b1", "clearTiebreak": true, "clearValueBounds": true}},
		{name: "client-submit 显式 false 生效", id: "b1", set: map[string]string{"client-submit": "false"},
			want: map[string]any{"boardId": "b1", "clientSubmit": false}},
		{name: "数值字段 presence", id: "b1", set: map[string]string{"per-subject-submit-limit": "5", "value-min": "-3"},
			want: map[string]any{"boardId": "b1", "perSubjectSubmitLimit": 5, "valueMin": int64(-3)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newPresenceVerb(t, func(fs *flag.FlagSet) {
				fs.String("sort", "", "")
				fs.String("tiebreak-order", "", "")
				fs.String("tie-break", "", "")
				fs.String("period-kind", "", "")
				fs.String("period-tz", "", "")
				fs.String("policy", "", "")
				fs.Int64("value-min", 0, "")
				fs.Int64("value-max", 0, "")
				fs.Bool("client-submit", false, "")
				fs.Int("per-subject-submit-limit", 0, "")
				fs.Int("retention-periods", 0, "")
				fs.String("subject-kind", "", "")
			}, tt.set)
			req, err := buildUpdateBoardReq(v, tt.id, boardConfig{
				sort: tt.set["sort"], tiebreakOrder: tt.set["tiebreak-order"], tieBreak: tt.set["tie-break"],
				periodKind: tt.set["period-kind"], periodTZ: tt.set["period-tz"], policy: tt.set["policy"],
				subjectKind:           tt.set["subject-kind"],
				valueMin:              testInt64(tt.set["value-min"]),
				valueMax:              testInt64(tt.set["value-max"]),
				clientSubmit:          testBool(tt.set["client-submit"]),
				perSubjectSubmitLimit: testInt(tt.set["per-subject-submit-limit"]),
				retentionPeriods:      testInt(tt.set["retention-periods"]),
			}, tt.clearTiebreak, tt.clearValueBounds)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, req)
		})
	}
}
