// Package functions 是 Torchwood Go 函数的官方运行时 SDK（functions 五期 5a）。
//
// owner 裁决（2026-09-18）：Go 函数采用「用户持有 main + SDK」模型——用户
// 代码编译为根 main 包，经本 SDK 承接平台执行协议；平台侧 Go 构建分支坍缩
// 为 `go build .`。本包是 runner 契约（:18080，协议公开化裁决）的自包含
// 参考实现：仅依赖标准库、不 import 任何 internal 包，外部用户可经 Go
// proxy 独立拉取构建。
//
// # 运行契约（与平台 runner runner.js / dispatcher/pool.go 消费面逐条对齐）
//
//   - 监听 TW_RUNNER_PORT（缺省 18080，bind 0.0.0.0）；
//   - GET /_tw/health → 200 JSON（就绪后恒 ready；进程 panic 于 init 属
//     用户代码自身崩溃，进程自然退出 = 恒 not-ready，dispatcher 启动探针
//     超时后回收实例）；
//   - POST /：body = TW_DATA JSON；身份六件经分发 header 传递：
//     x-tw-execution-token / x-tw-execution-id / x-tw-source（缺省回落
//     "server"）/ x-tw-invoking-user-id / x-tw-project-id；apiBaseUrl 来自
//     env TW_API_BASE_URL；
//   - x-tw-trigger-envelope（base64 JSON {method,path,raw_query,headers}）：
//     fetch 风格还原为真 *http.Request（url = http://trigger{path}?
//     {raw_query}，headers 原样，body = 分发 body）；main 风格重组为封套
//     TW_DATA（与 runner.js 逐字段同构）；
//   - x-tw-timeout-seconds：per-request 超时（缺省 30s），到点回 500 封套
//     并放弃等待（handler goroutine 残跑，结果 channel 缓冲 1 防泄漏）；
//   - panic 捕获 → 500 封套（不杀实例）；TW_DATA 解析失败 → 400；
//   - 响应封套：成功 200 {"ok":true,"result":<JSON>,"stdout":"","stderr":""}
//     （Go 无 per-request console 捕获，stdout/stderr 恒空串）；失败 500
//     {"ok":false,"error":"..."}；fetch 风格 → {ok,status,headers,
//     body_base64,truncated,...}（64KB 截断；hop-by-hop 与 content-length/
//     host/date/server 头过滤，黑名单对照 runner.js）；
//   - TW_MAX_REQUESTS（缺省 1000；显式 0/非法值 = 关闭）：每响应后计数
//     达标自退出（Flush 后 Exit，dispatcher 检测退出补位）；
//   - SIGTERM/SIGINT → 停止接新（新分发请求 503）+ 等在途完成 +
//     TW_DRAIN_TIMEOUT_MS（缺省 10000）兜底强退；health 在 drain 期仍 200
//     （runner 同款）。
//
// # 协议演进宪法
//
// 未知 x-tw-* 分发 header 一律忽略：本 SDK 只读取上列已知 header。未来
// 平台新增 x-tw-* 能力时，旧版 SDK 必须保持可用（新字段缺失按各自缺省
// 语义处理），不得因未知 header 拒绝请求。
//
// # 用法
//
// 单源（平台组合单位 = function 本身，invoke 与 cron 应为两个函数各自订阅）：
//
//	func main() {
//	    _ = functions.StartInvoke(func(ctx context.Context, req PingReq) (PongResp, error) {
//	        id := functions.FromContext(ctx) // 身份六件（token/baseURL/…）
//	        c := functions.NewClient(ctx)    // 平台 API 客户端（鉴权自动）
//	        _, _ = c.CreateDocument(ctx, "app", "logs", map[string]any{"ping": req.Ping})
//	        return PongResp{Pong: req.Ping}, nil
//	    })
//	}
//
// 多触发源显式组合（单二进制多源，复用 ServeMux/chi 心智）：
//
//	mux := functions.NewMux()
//	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) { ... })
//	mux.Cron(func(ctx context.Context, tick functions.CronTick) error { ... })
//	mux.Event(func(ctx context.Context, ch functions.DocumentChange) error { ... })
//	mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... }))
//	_ = functions.Listen(mux) // 阻塞，语义同 http.ListenAndServe
package functions
