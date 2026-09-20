// 外部测试包：契约断言需 import 业务 domain 包（assets 等），被测包本体
// 不得反向 import（依赖红线），外部测试包无此环。
package events_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubscriptions "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
)

// ——词表同源契约：目录字符串字面量必须与各业务 domain 包常量逐字相等
//（catalog.go 不 import 业务包是依赖方向红线，同源性由此处断言保证）——

func TestCatalogMatchesDomainConstants(t *testing.T) {
	// payments / economy / subscriptions 的目录条目 == domain 包事件常量全集。
	require.Contains(t, domainevents.SystemEvents(), domainpayments.EventOrderPaid)
	require.Contains(t, domainevents.SystemEvents(), domainpayments.EventOrderFailed)
	require.Contains(t, domainevents.SystemEvents(), domainpayments.EventOrderRefunded)
	for _, e := range []string{
		domainassets.EventGranted, domainassets.EventConsumed, domainassets.EventTransferred,
		domainassets.EventMutated, domainassets.EventExpired,
	} {
		require.Contains(t, domainevents.SystemEvents(), e)
	}
	for _, e := range []string{
		domainsubscriptions.EventActivated, domainsubscriptions.EventRenewed,
		domainsubscriptions.EventPastDue, domainsubscriptions.EventCanceled,
		domainsubscriptions.EventExpired,
	} {
		require.Contains(t, domainevents.SystemEvents(), e)
	}
}

// 机制验收契约：目录内每个事件都能被「{domain}.*」订阅串展开命中——
// 新登记一个域/事件而忘记目录语义时在此失败，而不是静默不可订阅。
func TestCatalogDomainWildcardCoversAllEvents(t *testing.T) {
	for _, domain := range domainevents.SystemEventDomains() {
		expanded, err := domainevents.ResolveSystemPatterns(domain + ".*")
		require.NoError(t, err, domain)
		require.NotEmpty(t, expanded, domain)
	}
	for _, e := range domainevents.SystemEvents() {
		require.True(t, domainevents.IsSystemEventName(e), e)
	}
	// 每个事件的首段即其域名（MatchSystem 前提：事件名可路由回目录域）。
	for _, e := range domainevents.SystemEvents() {
		domain, _, _ := strings.Cut(e, ".")
		require.Contains(t, domainevents.SystemEventDomains(), domain, e)
	}
}

func TestResolveSystemPatterns(t *testing.T) {
	t.Run("合法形态", func(t *testing.T) {
		cases := []struct {
			in   string
			want []string
		}{
			{"auth.*", []string{
				domainevents.EventAuthUsersCreated,
				domainevents.EventAuthUsersSignedIn,
				domainevents.EventAuthUsersSignedOut,
			}},
			{"auth.users.*", []string{
				domainevents.EventAuthUsersCreated,
				domainevents.EventAuthUsersSignedIn,
				domainevents.EventAuthUsersSignedOut,
			}},
			{"auth.users.created", []string{domainevents.EventAuthUsersCreated}},
			{"payments.orders.*", []string{
				"payments.orders.paid", "payments.orders.failed", "payments.orders.refunded",
			}},
			// 两段形态域：subscriptions 无 resource 层，域级通配合法。
			{"subscriptions.*", []string{
				"subscriptions.activated", "subscriptions.renewed", "subscriptions.past_due",
				"subscriptions.canceled", "subscriptions.expired",
			}},
			{"subscriptions.activated", []string{"subscriptions.activated"}},
		}
		for _, c := range cases {
			got, err := domainevents.ResolveSystemPatterns(c.in)
			require.NoError(t, err, c.in)
			require.ElementsMatch(t, c.want, got, c.in)
		}
	})

	t.Run("非法形态 fail-closed", func(t *testing.T) {
		bad := []string{
			"",                     // 空
			"*",                    // 裸通配（无域名）
			"auth",                 // 裸域名（非订阅串——域级订阅须显式 auth.*）
			"typo.users.*",         // 未登记域
			"authx.*",              // 域名拼错
			"auth.unknown.*",       // 域对但前缀无命中
			"auth.orders.*",        // 域对但 resource 拼错
			"auth.*.created",       // 中间段通配（一期不允许）
			"auth.users.created.*", // 精确事件名后跟通配（前缀无命中）
			"payments.orders.paidx", // 事件名拼错（前缀无命中）
		}
		for _, s := range bad {
			_, err := domainevents.ResolveSystemPatterns(s)
			require.Error(t, err, s)
		}
	})
}

func TestIsSystemEventName(t *testing.T) {
	require.True(t, domainevents.IsSystemEventName(domainevents.EventAuthUsersSignedIn))
	require.False(t, domainevents.IsSystemEventName("auth.users.upsert"))          // 词表外
	require.False(t, domainevents.IsSystemEventName("databases.documents.create")) // 文档事件不属于目录
	require.False(t, domainevents.IsSystemEventName(""))
}
