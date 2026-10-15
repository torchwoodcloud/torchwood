package webhooks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 订阅匹配器：复用 eventmatch 内核的命中/不命中语义 + webhook 载体去重。

func TestSubscriptionIndex_DocumentMatch(t *testing.T) {
	idx := NewSubscriptionIndex([]Webhook{
		{ID: "wh-1", ProjectID: "p1", Status: StatusEnabled,
			Events: []string{"databases.app.collections.notes.documents.create"}},
		{ID: "wh-2", ProjectID: "p1", Status: StatusEnabled,
			Events: []string{"databases.app.collections.*.documents.*"}},
		{ID: "wh-3", ProjectID: "p1", Status: StatusPaused, // 暂停不入索引
			Events: []string{"databases.app.collections.notes.documents.create"}},
		{ID: "wh-4", ProjectID: "p2", Status: StatusEnabled, // 跨项目隔离
			Events: []string{"databases.app.collections.notes.documents.create"}},
	})

	got := idx.Match("p1", "app", "notes", "create")
	require.ElementsMatch(t, []string{"wh-1", "wh-2"}, got)

	// 同一 webhook 多条订阅串命中同一事件 → 去重一次（外部端点无平台幂等
	// 兜底；与函数触发器的多次投递语义 deliberate 差异，见包注释）。
	dup := NewSubscriptionIndex([]Webhook{
		{ID: "wh-x", ProjectID: "p1", Status: StatusEnabled, Events: []string{
			"databases.app.collections.notes.documents.create",
			"databases.app.collections.*.documents.create",
		}},
	})
	require.Equal(t, []string{"wh-x"}, dup.Match("p1", "app", "notes", "create"))

	require.Equal(t, []string{"wh-2"}, idx.Match("p1", "app", "notes", "delete"), "wh-2 通配 op")
	require.Nil(t, idx.Match("p1", "otherdb", "notes", "create"), "database 段必须精确")
	require.Nil(t, idx.Match("p3", "app", "notes", "create"))
}

func TestSubscriptionIndex_SystemMatch(t *testing.T) {
	idx := NewSubscriptionIndex([]Webhook{
		{ID: "wh-1", ProjectID: "p1", Status: StatusEnabled, Events: []string{"auth.*"}},
		{ID: "wh-2", ProjectID: "p1", Status: StatusEnabled, Events: []string{"auth.users.created"}},
		{ID: "wh-3", ProjectID: "p1", Status: StatusEnabled, Events: []string{"payments.orders.paid"}},
	})

	require.ElementsMatch(t, []string{"wh-1", "wh-2"}, idx.MatchSystem("p1", "auth.users.created"))
	require.Equal(t, []string{"wh-1"}, idx.MatchSystem("p1", "auth.users.signed_in"), "auth.* 展开覆盖域内全部目录事件")
	require.Nil(t, idx.MatchSystem("p1", "payments.subscriptions.renewed"))
	require.Nil(t, idx.MatchSystem("p2", "auth.users.created"))
}

func TestDeliveryPolicy_LadderAndTerminal(t *testing.T) {
	p := NewDeliveryPolicy([]time.Duration{10 * time.Second, time.Minute, 8 * time.Hour}, 3, 24*time.Hour)
	created := time.Unix(0, 0)

	// 梯度按次推进；末值循环。
	require.Equal(t, 10*time.Second, p.backoffFor(1))
	require.Equal(t, time.Minute, p.backoffFor(2))
	require.Equal(t, 8*time.Hour, p.backoffFor(3))
	require.Equal(t, 8*time.Hour, p.backoffFor(50), "越界取末值循环")

	// 常规领取：Attempt + 推进。
	d := &Delivery{Attempts: 0, CreatedAt: created}
	plan := p.PlanAttempt(d, created.Add(time.Minute))
	require.True(t, plan.Attempt)
	require.False(t, plan.Terminal)
	require.Equal(t, created.Add(time.Minute).Add(10*time.Second), plan.Next)

	// 梯度耗尽：Terminal（第 3 次尝试是最后一次，第 4 次不再执行）。
	d.Attempts = 3
	plan = p.PlanAttempt(d, created.Add(time.Minute))
	require.False(t, plan.Attempt)
	require.True(t, plan.Terminal)

	// 越窗：即使 attempts 未耗尽也终态。
	d.Attempts = 0
	d.CreatedAt = time.Now().Add(-25 * time.Hour)
	plan = p.PlanAttempt(d, time.Now())
	require.True(t, plan.Terminal)
}

func TestDeliveryPolicy_Defaults(t *testing.T) {
	p := NewDeliveryPolicy(nil, 0, 0)
	require.Equal(t, DefaultRetryBackoff, p.Backoff)
	require.Equal(t, DefaultMaxAttempts, p.MaxAttempts)
	require.Equal(t, DefaultRetryWindow, p.RetryWindow)
	// 缺省梯度首两档（10s → 1m）。
	require.Equal(t, 10*time.Second, p.backoffFor(1))
	require.Equal(t, time.Minute, p.backoffFor(2))
}

func TestWebhookHelpers(t *testing.T) {
	// https 强制。
	require.NoError(t, ValidateURL("https://example.com/hook"))
	require.Error(t, ValidateURL("http://example.com/hook"))
	require.Error(t, ValidateURL(""))
	require.Error(t, ValidateURL("https://"+string(make([]byte, 4096))))

	// 指纹：稳定、可区分、非明文。
	f1 := Fingerprint("whsec_a")
	require.Equal(t, Fingerprint("whsec_a"), f1)
	require.NotEqual(t, Fingerprint("whsec_b"), f1)
	require.NotContains(t, f1, "whsec_a")

	// 轮换宽限窗判定。
	now := time.Now()
	require.False(t, EffectivePreviousSecret(now, time.Time{}, time.Hour), "从未轮换不在窗内")
	require.True(t, EffectivePreviousSecret(now, now.Add(-time.Hour), 24*time.Hour))
	require.False(t, EffectivePreviousSecret(now, now.Add(-25*time.Hour), 24*time.Hour))
	require.True(t, EffectivePreviousSecret(now, now.Add(-2*time.Hour), 0), "非法窗宽回退缺省")
}
