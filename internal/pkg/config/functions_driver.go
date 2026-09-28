package config

// 函数执行底座驱动的封闭值集（IMPL-T2-5 双执行底座）：functions.driver
// 的合法取值。仅 dispatcher 进程消费；未设置/未知值由 bootkit 校验层
// fail-closed（启动期拒绝，列出两选项与各自配置键）。string 而非 proto
// enum：config 绑定是结构体驱动逐叶解码（bind.go），enum 的 int32 承载
// 解不了 YAML 字符串；先例 storage.provider / idgen.default_strategy。
const (
	// FunctionsDriverDocker 是 docker 直接执行底座：dispatcher 持
	// docker.sock，函数实例 = 常驻容器（per-project bridge 网络），构建 =
	// 本地 docker build。
	FunctionsDriverDocker = "docker"
	// FunctionsDriverFleetly 是 fleetly 平台执行底座：函数实例 = fleetly
	// Tasks（swarm service 承载），构建 = fleetly build-from-upload。
	FunctionsDriverFleetly = "fleetly"
)
