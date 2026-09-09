// B13b（realtime 扇出掩码缓存，§4.3/§11-J A5 挂账）：扇出压测——N 订阅者
// × M 事件的 hub.Dispatch 耗时分布 + ACL 评估成本占比取数。判定"逐订阅者
// ACL 评估"是否为可测瓶颈：对照组（PlatformAdmin 旁路 VisibleTo）与实验组
// （逐订阅者 ACL 判定）同形对比，差值即 ACL 评估份额；另附同体量
// VisibleTo 裸成本微测。判据：压测证明需要 → 实施掩码缓存 + 失效；否则
// 记录"维持逐订阅者判定 + 压测数字"（结论回写 15-exit-poc B13b）。
package realtime

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/events"
)

// 压测规模（判据下限：订阅者 ≥100、事件 ≥1000）。
const (
	loadSubs   = 200
	loadEvents = 1200
)

// percentile 取已排序样本的分位数（0..1）。
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

// fanoutProfile 建 N 订阅者单集合频道并顺序派发 M 个事件，返回逐次
// Dispatch 耗时样本。adminBypass=true 时订阅者走 PlatformAdmin 旁路
// （VisibleTo 不评估）= 对照组。
func fanoutProfile(b *testing.B, adminBypass bool) ([]time.Duration, []*Conn) {
	b.Helper()
	hub := NewHub(nil)
	conns := make([]*Conn, loadSubs)
	for i := range conns {
		c := &Conn{
			ID:            fmt.Sprintf("c%03d", i),
			PlatformAdmin: adminBypass,
			Send:          make(chan map[string]any, loadEvents+1),
		}
		if !adminBypass {
			// 每连接一个用户角色；事件 ACL 授予前半用户——ACL 判定有真实
			// 的通过/拒绝两路（拒绝路扫完 ACE 表，是评估成本上界形态）。
			c.DocPrincipal = databases.Principal{Roles: []string{fmt.Sprintf("user:u%03d", i)}}
		}
		conns[i] = c
		// 频道名 = 信封的 CollectionChannel（databases.<db>.collections.<coll>）。
		hub.Subscribe("databases.d1.collections.docs", c)
	}

	acl := events.ACLSnapshot{
		DocumentSecurity: true,
		DocHasPerms:      true,
		DocumentPermissions: func() []databases.Permission {
			perms := make([]databases.Permission, 0, loadSubs/2)
			for i := 0; i < loadSubs/2; i++ {
				perms = append(perms, databases.Permission{Type: "read", Role: fmt.Sprintf("user:u%03d", i)})
			}
			return perms
		}(),
	}

	samples := make([]time.Duration, 0, loadEvents)
	for m := 0; m < loadEvents; m++ {
		start := time.Now()
		hub.Dispatch(events.Envelope{
			EventID:      fmt.Sprintf("evt-%s-%05d", b.Name(), m),
			Event:        events.EventDocumentsUpdate,
			ProjectID:    "p1",
			DatabaseID:   "d1",
			CollectionID: "docs",
			DocumentID:   fmt.Sprintf("doc-%05d", m),
			Version:      int64(m + 1),
			CreatedAt:    time.Now(),
			ACL:          acl,
			Seq:          int64(m + 1),
		})
		samples = append(samples, time.Since(start))
	}
	return samples, conns
}

func reportFanoutLatency(b *testing.B, label string, samples []time.Duration) {
	b.Helper()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum time.Duration
	for _, d := range samples {
		sum += d
	}
	b.Logf("%s: n=%d mean=%v p50=%v p95=%v p99=%v max=%v",
		label, len(samples), sum/time.Duration(len(samples)),
		percentile(samples, 0.50), percentile(samples, 0.95),
		percentile(samples, 0.99), samples[len(samples)-1])
}

// BenchmarkHubDispatch_FanoutLoadProfile 是 B13b 的取数入口：
//
//	go test ./internal/infra/realtime -bench BenchmarkHubDispatch_FanoutLoadProfile -benchtime 1x
//
// 输出 Dispatch 延迟分布、ACL 评估份额与 VisibleTo 裸成本（结论回写
// 15-exit-poc B13b）；同时断言交付完整性（授予订阅者收满全部事件）。
func BenchmarkHubDispatch_FanoutLoadProfile(b *testing.B) {
	// 实验组：逐订阅者 ACL 评估（真实路径）。
	aclSamples, conns := fanoutProfile(b, false)
	reportFanoutLatency(b, "dispatch with per-subscriber ACL (200 subs x 1200 events)", aclSamples)
	// 交付完整性：前半订阅者（ACL 授予）与后半夜拒——夜拒者 0 帧、授予者满帧。
	for i, c := range conns {
		want := 0
		if i < loadSubs/2 {
			want = loadEvents
		}
		require.Equal(b, want, len(c.Send), "subscriber %d delivery mismatch", i)
	}

	// 对照组：PlatformAdmin 旁路 VisibleTo（同扇出形状、零 ACL 评估）。
	adminSamples, adminConns := fanoutProfile(b, true)
	reportFanoutLatency(b, "dispatch with PlatformAdmin bypass (200 subs x 1200 events)", adminSamples)
	require.Equal(b, loadEvents, len(adminConns[0].Send))

	var aclMean, adminMean time.Duration
	for _, d := range aclSamples {
		aclMean += d
	}
	aclMean /= time.Duration(len(aclSamples))
	for _, d := range adminSamples {
		adminMean += d
	}
	adminMean /= time.Duration(len(adminSamples))
	b.Logf("ACL evaluation share: %v/dispatch (%.1f%% of ACL-path mean, %.0fns per subscriber)",
		aclMean-adminMean, 100*float64(aclMean-adminMean)/float64(aclMean),
		float64((aclMean-adminMean).Nanoseconds())/float64(loadSubs))

	// VisibleTo 裸成本微测（同体量：200 评估/事件 × 1200 事件；拒绝路扫完
	// ACE 表）。
	acl := events.ACLSnapshot{
		DocumentSecurity:    true,
		DocHasPerms:         true,
		DocumentPermissions: []databases.Permission{{Type: "read", Role: "user:u000"}},
	}
	p := databases.Principal{Roles: []string{"user:u150"}}
	start := time.Now()
	const evals = loadSubs * loadEvents
	for i := 0; i < evals; i++ {
		if events.VisibleTo(acl, p) {
			b.Fatal("expected deny for u150")
		}
	}
	b.Logf("VisibleTo bare cost: %v/eval over %d evals", time.Since(start)/evals, evals)
}
