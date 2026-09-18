package config

// Functions 执行路由模式常量（四期 4b，M7 三档模型的可实现两档；设计
// docs/design/functions-runtimes-and-sources.md §4）。字符串值即
// functions.dispatcher.routing_mode 的合法配置值；"replicated" 实验档
// 不在实现范围（设计 §4 M7 降档裁决）。消费方：bootkit 启动期校验与
// dispatcher 的池/构建链门控共用同一声明源。
const (
	// FunctionsRoutingModeLocal 是缺省路由模式（向后兼容）：镜像不分发，
	// 函数镜像只存在于其构建节点，冷启动转发 BuildNode。
	FunctionsRoutingModeLocal = "local"
	// FunctionsRoutingModeRegistry 是镜像全局化模式（M1）：构建成功后
	// push functions.docker.registry，任意节点冷启动本地 miss 则 pull。
	FunctionsRoutingModeRegistry = "registry"
)

// NormalizedFunctionsRoutingMode 归一化 routing_mode 原始配置值：空与
// "local"（含大小写变体）→ local；"registry" → registry；其他原样返回
// （合法性判断归启动期校验，消费侧对未知值按 local 处理——fail-safe 缺省，
// 校验拦截在先，此处兜底防误漂移）。
func NormalizedFunctionsRoutingMode(raw string) string {
	switch raw {
	case FunctionsRoutingModeRegistry:
		return FunctionsRoutingModeRegistry
	case "", FunctionsRoutingModeLocal:
		return FunctionsRoutingModeLocal
	default:
		return raw
	}
}
