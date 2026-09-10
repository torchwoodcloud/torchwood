package worker

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRequeue_IncrementsAttempt payload 内嵌 attempt 递增（1→2→…），
// 其余字段（含 data）往返无损。
func TestRequeue_IncrementsAttempt(t *testing.T) {
	next, ok := requeue([]byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{\"x\":1}","attempt":1}`))
	require.True(t, ok)
	var m retryMessage
	require.NoError(t, json.Unmarshal(next, &m))
	require.Equal(t, "e1", m.ExecutionID)
	require.Equal(t, "fn_1", m.FunctionID)
	require.Equal(t, "p1", m.ProjectID)
	require.Equal(t, `{"x":1}`, m.Data)
	require.Equal(t, 2, m.Attempt)
}

// TestRequeue_OldFormatWithoutAttemptStartsAtOne 旧格式 payload（无 attempt
// 字段）首次重试 attempt=1（兼容）。
func TestRequeue_OldFormatWithoutAttemptStartsAtOne(t *testing.T) {
	next, ok := requeue([]byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{}"}`))
	require.True(t, ok)
	var m retryMessage
	require.NoError(t, json.Unmarshal(next, &m))
	require.Equal(t, 1, m.Attempt)
	require.Equal(t, "e1", m.ExecutionID)
	require.Equal(t, `{}`, m.Data)
}

// TestRequeue_RoundTripPreservesData 连续重抛（直到上限）每次往返均保留
// data 字段与全部 ID。
func TestRequeue_RoundTripPreservesData(t *testing.T) {
	payload := []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{\"nested\":{\"a\":1},\"s\":\"v\"}"}`)
	for i := 1; i <= maxProcessAttempts; i++ {
		next, ok := requeue(payload)
		require.True(t, ok, "attempt=%d 未超限应可重试", i)
		var m retryMessage
		require.NoError(t, json.Unmarshal(next, &m))
		require.Equal(t, i, m.Attempt)
		require.Equal(t, `{"nested":{"a":1},"s":"v"}`, m.Data)
		payload = next
	}
	// attempt 已达上限（=maxProcessAttempts）后再次失败即超限。
	next, ok := requeue(payload)
	require.False(t, ok)
	require.Nil(t, next)
}

// TestRequeue_ExhaustsAtLimit attempt 已等于 maxProcessAttempts 时再次失败
// 返回 ok=false（第 4 次失败才超限，maxProcessAttempts=3 语义）。
func TestRequeue_ExhaustsAtLimit(t *testing.T) {
	next, ok := requeue([]byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{}","attempt":3}`))
	require.False(t, ok)
	require.Nil(t, next)
}

// TestRequeue_InvalidPayloadNotRetried 无法解析的 payload 不重试（防御分支）。
func TestRequeue_InvalidPayloadNotRetried(t *testing.T) {
	next, ok := requeue([]byte("not json"))
	require.False(t, ok)
	require.Nil(t, next)
}
