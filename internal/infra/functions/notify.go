package functions

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HTTPNotifier 是 domain Notifier 端口的 v4 生产实现（v4 §4.2 事件轴，
// D10）：CloudEvents 1.0 structured mode over 签名 HTTP——cron / 事件触发
// / webhook 投递共用（重试节奏/DLQ 由投递引擎在调用方调度，本适配器只做
// 单次投递尝试）。
type HTTPNotifier struct {
	client *http.Client
	now    func() time.Time
}

// NewHTTPNotifier 以注入的 client 构造（生产经 NewGuardedInvokerClient
// 同款守卫 client；测试注入 httptest client 绕守卫回环拦截）。
func NewHTTPNotifier(client *http.Client) *HTTPNotifier {
	return &HTTPNotifier{client: client, now: time.Now}
}

// Notify 实现 Notifier 端口：2xx = 送达（nil）；非 2xx / 超时 / 连不通 =
// 送达失败（grpc status 上抛，供投递引擎计失败次数进梯度）。
func (n *HTTPNotifier) Notify(ctx context.Context, notif functions.Notification) error {
	body, err := json.Marshal(notif.Event)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "notify: marshal cloud event: %v", err)
	}
	if notif.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(notif.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, notif.TargetURL, bytes.NewReader(body))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "notify: build request: %v", err)
	}
	ts := n.now().Unix()
	entries := [][2]string{{"v1", functions.ComputeSignature(notif.TargetSecret, ts, body)}}
	if notif.TargetPreviousSecret != "" {
		entries = append(entries, [2]string{"v2", functions.ComputeSignature(notif.TargetPreviousSecret, ts, body)})
	}
	req.Header.Set(functions.HeaderSignature, functions.BuildSignatureHeader(ts, entries))
	req.Header.Set(functions.HeaderProtocolVersion, functions.ProtocolVersion)
	req.Header.Set("Content-Type", "application/json")
	if notif.ExecutionToken != "" {
		req.Header.Set(functions.HeaderExecutionToken, notif.ExecutionToken)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return status.Errorf(codes.DeadlineExceeded, "notify: %v", ctx.Err())
		}
		return status.Errorf(codes.Unavailable, "notify: dial %s: %v", redactURL(notif.TargetURL), err)
	}
	defer resp.Body.Close()
	// 通知轴不消费响应 body（无应用层结果）；读弃并限量防连接滞留。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return status.Errorf(codes.Unavailable, "notify: endpoint status %d", resp.StatusCode)
	}
	return nil
}
