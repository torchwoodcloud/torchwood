package cmd

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIngestEventsPayload(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
		check   func(t *testing.T, req map[string]any)
	}{
		{name: "裸数组包 events 键", content: `[{"name":"order_paid"},{"name":"sub_renewed","userId":"u1"}]`,
			check: func(t *testing.T, req map[string]any) {
				events, ok := req["events"].([]any)
				require.True(t, ok)
				require.Len(t, events, 2)
			}},
		{name: "对象透传", content: `{"events":[{"name":"order_paid"}]}`,
			check: func(t *testing.T, req map[string]any) {
				_, ok := req["events"].([]any)
				require.True(t, ok)
				require.Len(t, req, 1)
			}},
		{name: "非法 JSON", content: `{invalid`, wantErr: "failed to parse ingest payload"},
		{name: "标量载荷拒绝", content: `42`, wantErr: "must be a JSON array"},
		{name: "JSONL 拒绝（防静默截断）", content: "{\"name\":\"a\"}\n{\"name\":\"b\"}\n",
			wantErr: "must be a single JSON array or object"},
		{name: "尾随垃圾拒绝", content: `[] trailing`, wantErr: "must be a single JSON array or object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ingestEventsPayload([]byte(tt.content))
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, req)
		})
	}
}

func TestGranularityJSON(t *testing.T) {
	gran, err := granularityJSON("hour")
	require.NoError(t, err)
	require.Equal(t, "ANALYTICS_GRANULARITY_HOUR", gran)
	gran, err = granularityJSON("day")
	require.NoError(t, err)
	require.Equal(t, "ANALYTICS_GRANULARITY_DAY", gran)
	gran, err = granularityJSON("ANALYTICS_GRANULARITY_DAY")
	require.NoError(t, err)
	require.Equal(t, "ANALYTICS_GRANULARITY_DAY", gran)
	_, err = granularityJSON("week")
	require.ErrorContains(t, err, "invalid --granularity")
}

func TestTsJSON(t *testing.T) {
	require.Equal(t, "2026-09-01T00:00:00Z", tsJSON("2026-09-01"))
	require.Equal(t, "2026-09-01T12:30:00+08:00", tsJSON("2026-09-01T12:30:00+08:00"))
	// 非 YYYY-MM-DD 形态原样透传（合法性与窗口护栏归服务端）。
	require.Equal(t, "not-a-date", tsJSON("not-a-date"))
}

func TestRequireWindow(t *testing.T) {
	require.ErrorContains(t, requireWindow("", "2026-09-30"), "--from and --to are required")
	require.ErrorContains(t, requireWindow("2026-09-01", ""), "--from and --to are required")
	require.NoError(t, requireWindow("2026-09-01", "2026-09-30"))
}

// TestBuildIngestReqMissingFile 断言 --file 缺失/不可读的错误路径。
func TestBuildIngestReqMissingFile(t *testing.T) {
	_, err := buildIngestReq("Z:/definitely/not/a/file.json")
	require.ErrorContains(t, err, "failed to read --file")
}

// TestAnalyticsFlagRegistration 冒烟：两组动词的旗标声明能挂上 FlagSet
// （防旗标名/别名笔误——flag 包对重复声明 panic，等于编译期检查）。
func TestAnalyticsFlagRegistration(t *testing.T) {
	v := newVerb(nil, "test", "test", "test", func(fs *flag.FlagSet) {
		registerWindowFlags(fs, new(string), new(string))
	}, nil)
	v.SetFlags(flag.NewFlagSet("test", flag.ContinueOnError))
	require.NotNil(t, v.fs.Lookup("from"))
	require.NotNil(t, v.fs.Lookup("to"))
}
