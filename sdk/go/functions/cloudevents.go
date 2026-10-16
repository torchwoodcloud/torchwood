package functions

// v4 事件轴服务面 + 统一入口（docs/design/functions-v4.md §4.2，D10 拍板）：
// cron / 事件触发以 CloudEvents 1.0 structured mode 通知送达（无应用层结果，
// 2xx = 送达）；与 RPC 轴（rpc.go）共用统一入口 NewServer——按 body 形态探
// 测分流（有 jsonrpc → RPC、有 specversion → 事件），同一签名方案罩两面。
//
// SDK 独立 module 不 import 平台 internal/：cloudEvent 结构与常量自持，
// 口径与 internal/domain/functions/protocol.go 一致。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// 事件轴常量（与平台 domain protocol.go 同值）。
const (
	cloudEventsSpecVersion = "1.0"
	cloudEventTypeCronTick = "cron.tick"
)

// cloudEvent 是 structured mode 信封（SDK 侧只需解析面；twproject/twseq
// 扩展属性对函数经 envelope 直接可见，不单独投影）。
type cloudEvent struct {
	Specversion string          `json:"specversion"`
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Time        string          `json:"time,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

// server 是统一入口的运行时形态：mux + 构建期读定的 API base（部署侧注入
// 的 TW_API_BASE_URL，每请求身份恒定）。
type server struct {
	mux        *Mux
	apiBaseURL string
}

// NewServer 构建函数常驻服务的 v4 统一入口：可选探针 GET /_tw/health（部署
// 平台用，不走验签）→ Verify 验签 → body 形态探测 → RPC 轴（jsonrpc：
// invoke / http.request）或事件轴（specversion：cron.tick → Cron、其余
// type → Event）→ Mux 注册的 handler。构建期读一次部署注入的 TW_API_BASE_URL
// 注入每请求身份。可直接挂任意 http.Server / 云平台 HTTP 入口（fleetly app
// / k8s Service / 裸机）。
//
// 用法：
//
//	mux := functions.NewMux()
//	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) { … })
//	mux.Cron(func(ctx context.Context, tick functions.CronTick) error { … })
//	http.ListenAndServe(":8080", functions.NewServer(mux, secret))
func NewServer(mux *Mux, secrets ...string) http.Handler {
	s := &server{mux: mux, apiBaseURL: os.Getenv(envAPIBaseURL)}
	protocol := Verify(secrets...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			http.Error(w, "functions: read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxBodyBytes {
			http.Error(w, "functions: body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err != nil {
			http.Error(w, "functions: malformed json body", http.StatusBadRequest)
			return
		}
		switch {
		case probe["jsonrpc"] != nil:
			s.serveRPC(w, r, body)
		case probe["specversion"] != nil:
			s.serveEvent(w, r, body)
		default:
			http.Error(w, "functions: unrecognized protocol (expect jsonrpc or specversion)", http.StatusBadRequest)
		}
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			s.serveHealth(w, r)
			return
		}
		protocol.ServeHTTP(w, r)
	})
}

// serveHealth 是可选探针：GET → 200 JSON（进程活着即就绪）；其余方法 405。
func (s *server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, &healthEnvelope{OK: false})
		return
	}
	writeJSON(w, http.StatusOK, &healthEnvelope{OK: true, Ready: true})
}

// serveEventBody 解析并分发 CloudEvents 通知：cron.tick → data 解析为
// CronTick → Cron handler；其余 type → data 解析为事件投影 → Event
// handler。成功 204 No Content（无应用层结果）；解析/处理失败/panic →
// 400——对投递引擎即「本次投递失败」信号（进重试梯度）。
func (s *server) serveEvent(w http.ResponseWriter, r *http.Request, body []byte) {
	var ce cloudEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		http.Error(w, "functions: malformed cloud event: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ce.Specversion != cloudEventsSpecVersion {
		http.Error(w, "functions: unsupported specversion "+ce.Specversion, http.StatusBadRequest)
		return
	}

	// 执行身份：token 头 + CloudEvent id（事件轴的执行关联键）+ 部署 env 的
	// API base；source 头缺省回落 "server"。
	ctx, cancel := context.WithTimeout(identityContext(r, ce.ID, s.apiBaseURL), rpcRequestTimeout)
	defer cancel()

	// panic 捕获 → 400 投递失败信号（不杀进程；投递引擎按梯度重投）。
	defer func() {
		if rec := recover(); rec != nil {
			http.Error(w, fmt.Sprintf("functions: handler panic: %v", rec), http.StatusBadRequest)
		}
	}()

	if ce.Type == cloudEventTypeCronTick {
		cron := s.mux.snapshot().cron
		if cron == nil {
			http.Error(w, "functions: no Cron handler registered", http.StatusBadRequest)
			return
		}
		tick, err := parseCronTick(ce.Data)
		if err != nil {
			http.Error(w, "functions: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := cron(ctx, tick); err != nil {
			http.Error(w, "functions: cron handler: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	event := s.mux.snapshot().event
	if event == nil {
		http.Error(w, "functions: no Event handler registered", http.StatusBadRequest)
		return
	}
	ch, err := parseDocumentChange(ce.Data)
	if err != nil {
		http.Error(w, "functions: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := event(ctx, ch); err != nil {
		http.Error(w, "functions: event handler: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
