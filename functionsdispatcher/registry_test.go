package functionsdispatcher

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// —— InstanceRecord 兼容解码用例（v3 §1.3「兼容陷阱」）——
// 注册表只能从记录侧对账（清 Redis 键升级会泄漏容器），旧记录
//（busy 布尔、spawned_at/idle_since RFC3339 字符串）必须双读兼容，
// 新记录按新字段名编码。

// TestInstanceRecord_UnmarshalJSONLegacyCompat 旧记录双读兼容：
// busy=true → inflight=1；RFC3339 时间字符串解析为毫秒。
func TestInstanceRecord_UnmarshalJSONLegacyCompat(t *testing.T) {
	spawned := time.Date(2026, 9, 1, 8, 0, 0, 123_000_000, time.UTC)
	idle := time.Date(2026, 9, 1, 8, 5, 0, 0, time.UTC)
	raw := `{"instance_id":"inst-legacy","container_id":"inst-legacy","ip":"10.0.0.7",` +
		`"deployment_id":"dep-1","busy":true,"draining":false,"requests":7,` +
		`"spawned_at":"` + spawned.Format(time.RFC3339Nano) + `",` +
		`"idle_since":"` + idle.Format(time.RFC3339Nano) + `",` +
		`"lease_until_ms":1797012345678,` +
		`"min_instances":1,"idle_ttl_seconds":300,"max_requests":1000}`

	rec, err := decodeRecord(raw)
	require.NoError(t, err)
	require.Equal(t, "inst-legacy", rec.InstanceID)
	require.Equal(t, 1, rec.Inflight, "busy=true 必须推导 inflight=1")
	require.Equal(t, 7, int(rec.Requests))
	require.Equal(t, spawned.UnixMilli(), rec.SpawnedAtMS, "RFC3339 spawned_at 必须解析为毫秒")
	require.Equal(t, idle.UnixMilli(), rec.IdleSinceMS, "RFC3339 idle_since 必须解析为毫秒")
	require.Equal(t, int64(1797012345678), rec.LeaseUntilMS)

	// busy=false → inflight=0（可被认领）。
	rec, err = decodeRecord(strings.ReplaceAll(raw, `"busy":true`, `"busy":false`))
	require.NoError(t, err)
	require.Zero(t, rec.Inflight, "busy=false 必须推导 inflight=0")
}

// TestInstanceRecord_UnmarshalJSONNewFormat 新记录按新字段名解码；inflight
// 显式存在时不回退 busy 推导；毫秒字段优先于 legacy 字符串。
func TestInstanceRecord_UnmarshalJSONNewFormat(t *testing.T) {
	raw := `{"instance_id":"inst-new","container_id":"inst-new","ip":"10.0.0.8",` +
		`"deployment_id":"dep-1","inflight":2,"concurrency":4,"draining":true,` +
		`"requests":3,"timeouts":1,"spawned_at_ms":1797012345000,"idle_since_ms":1797012350000,` +
		`"lease_until_ms":1797012400000,"min_instances":0,"idle_ttl_seconds":60,"max_requests":10}`
	rec, err := decodeRecord(raw)
	require.NoError(t, err)
	require.Equal(t, 2, rec.Inflight)
	require.Equal(t, 4, rec.Concurrency)
	require.True(t, rec.Draining)
	require.Equal(t, 1, rec.Timeouts)
	require.Equal(t, int64(1797012345000), rec.SpawnedAtMS)
	require.Equal(t, int64(1797012350000), rec.IdleSinceMS)

	// inflight 缺席 + busy 缺席 → 0（新写入侧不再产生 busy 字段）。
	rec, err = decodeRecord(`{"instance_id":"inst-min","container_id":"c","ip":"10.0.0.9","deployment_id":"d"}`)
	require.NoError(t, err)
	require.Zero(t, rec.Inflight)
	require.Zero(t, rec.Concurrency, "concurrency 缺省留零，claim Lua 按 1 兜底")
}

// TestInstanceRecord_EncodeRoundTrip 新记录编码往返：所有字段保持，且不再
// 写出 busy/RFC3339 旧字段（Lua 可安全改写全部字段的前提，v3 §1.3 排雷）。
func TestInstanceRecord_EncodeRoundTrip(t *testing.T) {
	now := time.Now()
	in := InstanceRecord{
		InstanceID:     "inst-rt",
		ContainerID:    "inst-rt",
		IP:             "10.0.0.1",
		DeploymentID:   "dep-1",
		Inflight:       2,
		Concurrency:    4,
		Requests:       9,
		Timeouts:       1,
		SpawnedAtMS:    now.UnixMilli(),
		IdleSinceMS:    now.Add(-time.Second).UnixMilli(),
		LeaseUntilMS:   now.Add(leaseTTL).UnixMilli(),
		MinInstances:   1,
		IdleTTLSeconds: 300,
		MaxRequests:    1000,
	}
	raw, err := encodeRecord(in)
	require.NoError(t, err)
	require.NotContains(t, raw, `"busy"`, "编码不得再写出 busy 旧字段")
	require.NotContains(t, raw, `"spawned_at":`, "编码不得再写出 RFC3339 spawned_at")
	require.NotContains(t, raw, `"idle_since":`, "编码不得再写出 RFC3339 idle_since")

	out, err := decodeRecord(raw)
	require.NoError(t, err)
	require.Equal(t, in, *out, "编码往返必须保真")
}

// TestInstanceRecord_UnmarshalJSONCorrupted 损坏记录必须显式失败（List 静默
// 吞掉并交 reaper 幽灵对账，decode 不得返回半零值记录）。
func TestInstanceRecord_UnmarshalJSONCorrupted(t *testing.T) {
	_, err := decodeRecord(`{not json`)
	require.Error(t, err)
	_, err = decodeRecord(`{"ip":"10.0.0.1"}`)
	require.Error(t, err, "缺 instance_id 的记录必须拒绝")
}
