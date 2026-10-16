// Package functions 是 Torchwood Go 函数的官方运行时 SDK（Functions v4）。
//
// v4（docs/design/functions-v4.md）执行面整体外包：函数 = 常驻服务，平台经
// 「签名 HTTP + 双轴协议」调用它——RPC 轴 JSON-RPC 2.0（invoke /
// http.request，送达成功恒 HTTP 200、函数成败在 result/error 对象）与事件
// 轴 CloudEvents 1.0 structured mode（cron.tick / 文档与系统事件，2xx =
// 送达）。本包是该协议的自包含参考实现：仅依赖标准库、不 import 任何
// internal 包，外部用户可经 Go proxy 独立拉取构建。
//
// # 统一入口（NewServer / Listen）
//
// NewServer(mux, secrets...) 组装「验签（X-Tw-Signature：时间窗 + 常数时间
// 比较 + 双密钥轮换任一命中）→ body 形态探测（有 jsonrpc → RPC 轴、有
// specversion → 事件轴）→ Mux 分发」的 http.Handler，可直接挂任意
// http.Server / 云平台 HTTP 入口（fleetly app / k8s Service / 裸机）。内置：
// 可选探针 GET /_tw/health（部署平台探活用，不走验签）、请求体 4MB 上限、
// handler panic 捕获（RPC 轴 → error 对象、事件轴 → 400 投递失败信号），
// 不杀进程。
//
// Listen(mux, secrets...) 是阻塞便捷形态（语义同 http.ListenAndServe）：
// 绑定 TW_RUNNER_PORT（缺省 18080；v4 语义 = 常驻服务监听端口），SIGTERM/
// SIGINT → 优雅排水（停止接新 + 等在途完成 + TW_DRAIN_TIMEOUT_MS 兜底强
// 退）。StartInvoke / StartCron / StartEvent / StartHTTP 是单源便捷封装
// （= NewMux 注册 + Listen，验签密钥必传）。
//
// # 执行身份与平台回访
//
// 每次调用的身份六件自动注入 handler ctx（FromContext 读取）：token /
// source / invoking-user / project 四件走签名请求的身份头（source 缺省回落
// "server"）；executionId 是本次执行的关联键（RPC 轴 = JSON-RPC id 回显键、
// 事件轴 = CloudEvent id）；apiBaseUrl 是平台 API base——由部署侧注入的 env
// TW_API_BASE_URL 供给（NewServer 构建期读入每请求身份；v4 下 env 归部署侧
// 下发，如 fleetly app env / compose）。NewClient(ctx) 据此零配置回访平台
// API（未注入 env 时构造即报错，指明部署需注入）。
//
// # 协议演进宪法
//
// 未知 x-tw-* 头与未知 JSON 字段一律忽略：本 SDK 只读取已知项。未来平台
// 新增能力时旧版 SDK 必须保持可用（新字段缺失按各自缺省语义处理），不得因
// 未知字段拒绝请求。
//
// # 用法
//
// 多触发源显式组合（单端点多轴，分发维度 = 协议轴而非触发器）：
//
//	mux := functions.NewMux()
//	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) {
//	    id := functions.FromContext(ctx) // 身份六件（token/baseURL/…）
//	    _ = id
//	    c, err := functions.NewClient(ctx) // 平台 API 客户端（鉴权自动）
//	    if err != nil {
//	        return nil, err
//	    }
//	    _, _ = c.CreateDocument(ctx, "app", "logs", map[string]any{"ping": "hi"})
//	    return map[string]any{"pong": "hi"}, nil
//	})
//	mux.Cron(func(ctx context.Context, tick functions.CronTick) error { ... })
//	mux.Event(func(ctx context.Context, ch functions.DocumentChange) error { ... })
//	mux.Fetch(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ... }))
//	_ = functions.Listen(mux, secret) // 阻塞 + 排水
//	// 或自持 http.Server：http.ListenAndServe(":8080", functions.NewServer(mux, secret))
//
// 单源便捷封装（单源 = 平台组合单位一个 function，invoke 与 cron 应为两个
// 函数各自部署）：
//
//	func main() {
//	    _ = functions.StartInvoke(func(ctx context.Context, req PingReq) (PongResp, error) {
//	        return PongResp{Pong: req.Ping}, nil
//	    }, secret)
//	}
package functions
