package functions

// v4 事件轴服务面（docs/design/functions-v4.md §4.2，D10 拍板）：cron /
// 事件触发以 CloudEvents 1.0 structured mode 通知送达（无应用层结果，
// 2xx = 送达）；与 RPC 轴（rpc.go）共用统一入口 NewServer——按 body 形态
// 探测分流（有 jsonrpc → RPC、有 specversion → 事件），同一签名方案罩两面。
//
// SDK 独立 module 不 import 平台 internal/：cloudEvent 结构与常量自持，
// 口径与 internal/domain/functions/protocol.go 一致。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

// NewServer 构建函数常驻服务的 v4 统一入口：Verify 验签 → body 形态探测
// → RPC 轴（jsonrpc：invoke / http.request）或事件轴（specversion：
// cron.tick → Cron、其余 type → Event）→ Mux 注册的 handler。可直接挂
// 任意 http.Server / 云平台 HTTP 入口（fleetly app / k8s Service / 裸机）。
//
// 用法：
//
//	mux := functions.NewMux()
//	mux.Invoke(func(ctx context.Context, data json.RawMessage) (any, error) { … })
//	mux.Cron(func(ctx context.Context, tick functions.CronTick) error { … })
//	http.ListenAndServe(":8080", functions.NewServer(mux, secret))
func NewServer(mux *Mux, secrets ...string) http.Handler {
	return Verify(secrets...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			serveRPCBody(w, r, body, mux)
		case probe["specversion"] != nil:
			serveEventBody(w, r, body, mux)
		default:
			http.Error(w, "functions: unrecognized protocol (expect jsonrpc or specversion)", http.StatusBadRequest)
		}
	}))
}

// serveEventBody 解析并分发 CloudEvents 通知：cron.tick → data 解析为
// CronTick → Cron handler；其余 type → data 解析为事件投影 → Event
// handler。成功 204 No Content（无应用层结果）；解析/处理失败 400——
// 对投递引擎即「本次投递失败」信号（进重试梯度）。
func serveEventBody(w http.ResponseWriter, r *http.Request, body []byte, mux *Mux) {
	var ce cloudEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		http.Error(w, "functions: malformed cloud event: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ce.Specversion != cloudEventsSpecVersion {
		http.Error(w, "functions: unsupported specversion "+ce.Specversion, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(identityContext(r), rpcRequestTimeout)
	defer cancel()

	if ce.Type == cloudEventTypeCronTick {
		mux.mu.RLock()
		cron := mux.cron
		mux.mu.RUnlock()
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

	mux.mu.RLock()
	event := mux.event
	mux.mu.RUnlock()
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
