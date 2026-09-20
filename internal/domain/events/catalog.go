// 系统事件目录（System Event Catalog）：平台内一切非文档事件的唯一声明源。
// 文档事件（databases.documents.*）由用户 collection 运行时产生，无法枚举；
// 系统行为事件（auth/payments/economy/subscriptions…）是平台词表，在此登记。
//
// 机制约定（functions-v3 §4.1 增补）：新增一类系统事件 = 目录登记事件全名
// + 产生用例在同事务内 Publish——outbox/worker/匹配器/订阅语法零改动；
// 订阅串按目录前缀展开校验，展开为空即拒绝（fail-closed：拼错立刻 400，
// 不允许静默永不触发的订阅）。
//
// 依赖方向红线：本包不得 import 业务 domain 包（assets 已 import 本包，
// 反向即环）——payments/economy/subscriptions 的事件名以字符串字面量登记，
// 与各自 domain 包常量的同源性由 catalog_test.go 契约断言。
package events

import (
	"fmt"
	"sort"
	"strings"
)

// auth 域事件：本目录即唯一声明源（产生侧 app/client 与消费侧 functions
// 共用这些常量）。
const (
	AuthEventDomain = "auth"

	EventAuthUsersCreated   = "auth.users.created"
	EventAuthUsersSignedIn  = "auth.users.signed_in"
	EventAuthUsersSignedOut = "auth.users.signed_out"
)

// systemEventCatalog 是域 → 事件全名列表的目录。事件全名不假设段数
// （subscriptions.* 为两段形态，auth/payments/economy 为三段），匹配一律
// 按全名进行。
var systemEventCatalog = map[string][]string{
	AuthEventDomain: {
		EventAuthUsersCreated,
		EventAuthUsersSignedIn,
		EventAuthUsersSignedOut,
	},
	"payments": {
		"payments.orders.paid",
		"payments.orders.failed",
		"payments.orders.refunded",
	},
	"economy": {
		"economy.assets.granted",
		"economy.assets.consumed",
		"economy.assets.transferred",
		"economy.assets.mutated",
		"economy.assets.expired",
	},
	"subscriptions": {
		"subscriptions.activated",
		"subscriptions.renewed",
		"subscriptions.past_due",
		"subscriptions.canceled",
		"subscriptions.expired",
	},
}

// SystemEventDomains 返回目录内全部域名（排序稳定，观测/测试用）。
func SystemEventDomains() []string {
	out := make([]string, 0, len(systemEventCatalog))
	for d := range systemEventCatalog {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// SystemEvents 返回目录内全部事件全名（排序稳定，契约测试遍历用）。
func SystemEvents() []string {
	var out []string
	for _, events := range systemEventCatalog {
		out = append(out, events...)
	}
	sort.Strings(out)
	return out
}

// IsSystemEventName 报告事件全名是否在目录内。
func IsSystemEventName(name string) bool {
	if name == "" {
		return false
	}
	domain, _, _ := strings.Cut(name, ".")
	names, ok := systemEventCatalog[domain]
	if !ok {
		return false
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// ResolveSystemPatterns 解析并展开一条系统事件订阅串（functions 触发器
// config.events 的第二形态）。合法形态：
//
//	{domain}[*]                如 auth.*           → 该域全部事件
//	{domain}.{...}[.*]         如 auth.users.*     → 目录内该前缀的全部事件
//	{domain}.{...}.{op}        如 auth.users.created → 精确单事件
//
// 规则：首段必须是目录内域名；`*` 只允许出现在最后一段；中间段一律精确；
// 展开为空（域名对但前缀无命中，如 auth.orders.*）返回错误——fail-closed。
func ResolveSystemPatterns(s string) ([]string, error) {
	if s == "" {
		return nil, fmt.Errorf("empty system event pattern")
	}
	if len(s) > MaxSystemPatternBytes {
		return nil, fmt.Errorf("event pattern exceeds maximum of %d bytes", MaxSystemPatternBytes)
	}
	parts := strings.Split(s, ".")
	domain := parts[0]
	names, ok := systemEventCatalog[domain]
	if !ok {
		return nil, fmt.Errorf("unknown system event domain %q (known: %s)", domain, strings.Join(SystemEventDomains(), ", "))
	}
	// 裸域名（auth）不是订阅串——域级订阅必须显式写 auth.*（隐式等价
	// 会让「精确事件名拼错少写一段」静默放大为整域订阅）。
	if len(parts) == 1 {
		return nil, fmt.Errorf("bare domain is not a pattern; use %q for domain-wide subscription", domain+".*")
	}
	// `*` 只允许尾段；尾段之外出现即拒绝（auth.*.created 一期不支持）。
	for _, p := range parts[:len(parts)-1] {
		if p == systemEventWildcard {
			return nil, fmt.Errorf("wildcard is only allowed as the last segment")
		}
	}
	var out []string
	if parts[len(parts)-1] == systemEventWildcard {
		prefix := strings.Join(parts[:len(parts)-1], ".") + "."
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				out = append(out, n)
			}
		}
	} else {
		// 精确全名：目录内等值命中（不是前缀——事件名本身不带尾点）。
		for _, n := range names {
			if n == s {
				out = append(out, n)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("system event pattern %q matches no catalog events", s)
	}
	return out, nil
}

// systemEventWildcard 是订阅串通配段字面量（与文档形态的 EventSegmentAny
// 语义一致，独立常量避免跨包引用）。
const systemEventWildcard = "*"

// MaxSystemPatternBytes 是系统事件订阅串的字节上限（与 proto/server
// functions.proto EventTriggerConfig 的 repeated.items.string.max_len=200
// 同源；仅防滥用）。
const MaxSystemPatternBytes = 200
