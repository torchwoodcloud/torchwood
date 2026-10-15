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

// notificationTailLimit 是事件轴投递响应尾部存档的截断口径（D10 语义损失
// 入册：事件轴投递行不记录函数返回值，响应 body 尾部截断留档作审计替代）。
const notificationTailLimit = 512

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
// 送达失败（grpc status 上抛，供投递引擎计失败次数进梯度）。响应 body
// 尾部截断进 NotificationResult.ResponseTail（D10 审计存档通道）。
func (n *HTTPNotifier) Notify(ctx context.Context, notif functions.Notification) (functions.NotificationResult, error) {
	body, err := json.Marshal(notif.Event)
	if err != nil {
		return functions.NotificationResult{}, status.Errorf(codes.InvalidArgument, "notify: marshal cloud event: %v", err)
	}
	if notif.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(notif.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, notif.TargetURL, bytes.NewReader(body))
	if err != nil {
		return functions.NotificationResult{}, status.Errorf(codes.InvalidArgument, "notify: build request: %v", err)
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

	start := n.now()
	resp, err := n.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return functions.NotificationResult{}, status.Errorf(codes.DeadlineExceeded, "notify: %v", ctx.Err())
		}
		return functions.NotificationResult{}, status.Errorf(codes.Unavailable, "notify: dial %s: %v", redactURL(notif.TargetURL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	durationMS := time.Since(start).Milliseconds()
	// 响应 body 无应用层语义；尾部截断留档（D10）并限量读弃防连接滞留。
	tail := truncateTailN(readTail(resp.Body), notificationTailLimit)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return functions.NotificationResult{ResponseTail: tail, DurationMS: durationMS},
			status.Errorf(codes.Unavailable, "notify: endpoint status %d", resp.StatusCode)
	}
	return functions.NotificationResult{ResponseTail: tail, DurationMS: durationMS}, nil
}

// readTail 读取响应 body 的尾部截断文本（限量读弃防连接滞留）。
func readTail(r io.Reader) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	return b
}

// truncateTailN 按限长截断（末尾省略号）；notify 专用口径（D10 审计存档）。
func truncateTailN(b []byte, limit int) string {
	s := string(b)
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
