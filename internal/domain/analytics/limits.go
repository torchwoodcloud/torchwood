// Package analytics 是事件分析的领域层（docs/design/analytics.md）：
// 平台级上限常量的单一来源、事件领域类型与摄入仓储端口。proto 的
// protovalidate 注解与本文件常量对齐（PR 验收项）；复合校验（props 键数 /
// 16KiB 体积 / 钳制窗）由 app 用例层执行（PR2）。
package analytics

import (
	"regexp"
	"time"
)

// 平台级强制上限（设计 §3「上限」表）。修改任一值必须同步 proto 注解与
// 开发者文档。
const (
	// MaxBatchEvents 单请求事件数上限。
	MaxBatchEvents = 100
	// MaxPropsKeys 单事件 props 键数上限。
	MaxPropsKeys = 25
	// MaxEventBytes 单事件序列化体积上限（16 KiB）。
	MaxEventBytes = 16 << 10
	// MaxNameLen 事件名长度上限（= 1 个首字符 + 63 个主体字符）。
	MaxNameLen = 64
	// MaxPropKeyLen props 键长度上限（= 1 个首字符 + 63 个主体字符）。
	MaxPropKeyLen = 64
	// PropValueMaxLen prop 字符串值截断长度（超长截断入库，不拒收）。
	PropValueMaxLen = 256
	// MaxEventNames 事件名项目级软上限（D12：存量名不受影响，仅拒新名；
	// 并发摄取轻微超扣可容忍）。
	MaxEventNames = 1000
	// MaxSessionIDLen session_id 长度上限。
	MaxSessionIDLen = 255
	// MaxUserIDLen user_id 长度上限。
	MaxUserIDLen = 255
)

// occurred_at 钳制窗（D4：客户端时间为主语义，越界丢弃计入 skipped；
// ±24h/5min 取三案最紧——离线补传事件算对日，防伪由钳制窗承担）。
const (
	// ClampPast 是允许的最老事件时间相对 now 的偏移。
	ClampPast = -24 * time.Hour
	// ClampFuture 是允许的最新事件时间相对 now 的偏移（时钟漂移容差）。
	ClampFuture = 5 * time.Minute
)

// 形状正则（与 proto protovalidate pattern 逐字一致）。
var (
	// EventNamePattern 事件名：字母开头，主体为字母/数字/下划线/点/连字符。
	EventNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)
	// PropKeyPattern props 键：字母或下划线开头，主体为字母/数字/下划线/点。
	PropKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]{0,63}$`)
)
