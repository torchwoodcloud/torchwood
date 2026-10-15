// 投递调度引擎与 outbox 事件入口（functions-v4.md §6，D5）：
//
//	事件入口（worker webhooks 消费组）：Envelope → 订阅快照匹配 → 逐命中
//	webhook 建投递行（payload = CloudEvents 1.0 structured mode 信封整体）。
//	到期扫描（worker 1min loop）：ClaimDue CAS 领取 → HTTPNotifier 单次
//	尝试 → 2xx 标 delivered / 非 2xx 已由领取按梯度推进 next_attempt_at，
//	梯度耗尽或越窗的行在领取期直接终态 failed（DLQ）。
//
// 计量只进 metrics 不进 billing（D5 拍板——本包零 billing 依赖）。
package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 投递指标（D5：只进 metrics；前缀 torchwood_，包内自注册）。
var (
	// webhookDeliveryAttemptsTotal 是投递尝试计数：result ∈
	// delivered（2xx）/ retry（非终态失败，梯度推进）/ failed（终态 DLQ）/
	// error（领取/收口写失败等内部错误）。
	webhookDeliveryAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_webhook_delivery_attempts_total",
		Help: "Webhook delivery attempts by project and result (delivered|retry|failed|error).",
	}, []string{"project", "result"})
	// webhookDeliveryDurationSeconds 是单次投递尝试时延直方图（出站 HTTP
	// 往返；D5 计量面）。
	webhookDeliveryDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "torchwood_webhook_delivery_duration_seconds",
		Help:    "Webhook delivery attempt duration, by project and result.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 16), // 5ms .. ~164s
	}, []string{"project", "result"})
	// webhookDLQTotal 是终态失败（DLQ 入队）计数：非零即「订阅端持续不可
	// 达」告警锚点。
	webhookDLQTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_webhook_dlq_total",
		Help: "Webhook deliveries reaching terminal failed state (DLQ), by project.",
	}, []string{"project"})
)

func init() {
	prometheus.MustRegister(webhookDeliveryAttemptsTotal, webhookDeliveryDurationSeconds, webhookDLQTotal)
}

// ——投递行浏览/重投（管理面）——

// GetDelivery 读取投递行（不存在归一 NotFound；载荷不回显——投影面在
// handler）。
func (s *Service) GetDelivery(ctx context.Context, projectID, webhookID, deliveryID string) (*domainwebhooks.Delivery, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	d, err := s.deliveries.GetDelivery(ctx, projectID, webhookID, deliveryID)
	if err != nil {
		return nil, s.mapRepoErr(err)
	}
	return d, nil
}

// ListDeliveries 分页浏览投递行（DLQ 浏览面；status 非空过滤）。
func (s *Service) ListDeliveries(ctx context.Context, projectID, webhookID, status string, limit, offset int) (domainwebhooks.DeliveryListPage, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return domainwebhooks.DeliveryListPage{}, err
	}
	return s.deliveries.ListDeliveries(ctx, projectID, webhookID, status, limit, offset)
}

// RedeliverDelivery 手动重投：终态行（delivered/failed）重置为立即到期
// （pending、attempts=0），由 worker 到期扫描领取重投。pending 行拒绝
// （已在投递队列中）。返回重置后的投递行。
func (s *Service) RedeliverDelivery(ctx context.Context, projectID, webhookID, deliveryID string) (*domainwebhooks.Delivery, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := s.deliveries.GetDelivery(ctx, projectID, webhookID, deliveryID); err != nil {
		return nil, s.mapRepoErr(err)
	}
	ok, err := s.deliveries.ResetForRedeliver(ctx, projectID, webhookID, deliveryID, s.now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"delivery %s is pending (already in delivery queue); redeliver rejected", deliveryID)
	}
	return s.deliveries.GetDelivery(ctx, projectID, webhookID, deliveryID)
}

// ——事件入口——

// CreateDeliveriesCommand 是一次事件命中的建行命令（worker 匹配路径产出：
// 投影 data 已由调用方按 domainfunctions 事件投影预算构建）。
type CreateDeliveriesCommand struct {
	ProjectID string
	// WebhookIDs 是命中的订阅（去重后；空 = 无投递）。
	WebhookIDs []string
	// Envelope 是 outbox 事件信封（CloudEvent 属性的事实源）。
	Envelope *domainevents.Envelope
	// Data 是事件 data 投影 JSON（≤32KB，domainfunctions 投影预算）。
	Data string
}

// CreateDeliveries 为每个命中 webhook 建投递行（status=pending、
// next_attempt_at=now 即时首投）：payload = CloudEvents 1.0 structured mode
// 信封（§4.2——id = 投递行 id、source = projects/{p}、type = 事件目录名、
// twproject/twseq 扩展属性）。(webhook_id, event_id) 唯一索引吸收
// at-least-once 重复。返回实际新建行数（去重命中 = 0）。
func (s *Service) CreateDeliveries(ctx context.Context, cmd CreateDeliveriesCommand) (int, error) {
	if len(cmd.WebhookIDs) == 0 || cmd.Envelope == nil {
		return 0, nil
	}
	now := s.now()
	items := make([]*domainwebhooks.Delivery, 0, len(cmd.WebhookIDs))
	for _, whID := range cmd.WebhookIDs {
		id := "whd-" + newUUID()
		items = append(items, &domainwebhooks.Delivery{
			ID:            id,
			ProjectID:     cmd.ProjectID,
			WebhookID:     whID,
			EventID:       cmd.Envelope.EventID,
			EventType:     cmd.Envelope.Event,
			Seq:           cmd.Envelope.Seq,
			Status:        domainwebhooks.DeliveryStatusPending,
			NextAttemptAt: now,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
		ev := buildCloudEvent(id, cmd.Envelope, cmd.Data)
		payload, err := json.Marshal(ev)
		if err != nil {
			return 0, fmt.Errorf("marshal cloud event: %w", err)
		}
		items[len(items)-1].Payload = payload
	}
	return s.deliveries.CreateDeliveries(ctx, items)
}

// buildCloudEvent 组装事件轴信封（v4 §4.2：文档/系统事件 source =
// projects/{p}；time = outbox 产生时刻；twseq = outbox 全局序）。
func buildCloudEvent(deliveryID string, ev *domainevents.Envelope, data string) domainfunctions.CloudEvent {
	return domainfunctions.CloudEvent{
		Specversion:     domainfunctions.CloudEventsSpecVersion,
		ID:              deliveryID,
		Source:          "projects/" + ev.ProjectID,
		Type:            ev.Event,
		Time:            ev.CreatedAt.UTC().Format(time.RFC3339),
		DataContentType: "application/json",
		Data:            json.RawMessage(data),
		TwProject:       ev.ProjectID,
		TwSeq:           ev.Seq,
	}
}

// newUUID 生成投递行 id 尾段（与 functions 侧 "trg-"+uuid 同款直接取值）。
func newUUID() string { return idgen.UUID().String() }

// ——订阅快照——

// RefreshSubscriptionIndex 拉取全量订阅快照并构建匹配器（worker 15s 周期
// 驱动；对齐 functions.RefreshEventTriggerIndex 语义）：任一项目扫描失败时
// 返回已收集部分 + 首个错误——调用方**不换入**（残缺快照会把「匹配不到」
// 变成永久丢事件），保留上一轮完整快照。
func (s *Service) RefreshSubscriptionIndex(ctx context.Context, budget int) (*domainwebhooks.SubscriptionIndex, error) {
	if s.projects == nil || s.repo == nil {
		// 未装配项目目录/仓储（最小测试装配）：空快照 + 无错误——与
		// 「无订阅」等价。
		return domainwebhooks.NewSubscriptionIndex(nil), nil
	}
	all, err := s.projects.ListProjects(ctx)
	if err != nil {
		return domainwebhooks.NewSubscriptionIndex(nil), err
	}
	var (
		collected []domainwebhooks.Webhook
		firstErr  error
	)
	for i := range all {
		if all[i].Status != "active" {
			continue
		}
		if budget > 0 && len(collected) >= budget {
			break
		}
		ws, err := s.repo.ListEnabledWebhooks(ctx, all[i].ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.log.Warn("webhooks: list enabled subscriptions failed", "project_id", all[i].ID, "error", err)
			continue
		}
		if budget > 0 && len(collected)+len(ws) > budget {
			ws = ws[:budget-len(collected)]
		}
		collected = append(collected, ws...)
	}
	idx := domainwebhooks.NewSubscriptionIndex(collected)
	if firstErr != nil {
		return idx, firstErr
	}
	s.log.Debug("webhooks subscription index refreshed", "subscriptions", idx.Len())
	return idx, nil
}

// ——到期扫描（DeliveryScheduler 端口实现）——

// RunDueDeliveries 领取并投递全部 active 项目内到期的投递行（worker 每分钟
// ticker 驱动）。项目遍历带轮转游标 + 全局预算（镜像 DispatchDueCronTriggers
// 模式——webhook_deliveries 在项目 schema 内）。先 CAS 后投递（repo
// ClaimDue 保证）；批内按 concurrency 有界并发执行投递尝试。返回本次执行
// 的投递尝试条数。
func (s *Service) RunDueDeliveries(ctx context.Context, now time.Time, budget int) (int, error) {
	if s.deliveries == nil || s.notifier == nil {
		return 0, nil
	}
	if s.projects == nil {
		return 0, nil
	}
	all, err := s.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := s.scanCursor.Start(n)
	remaining := budget
	attempted := 0
	stopped := -1
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if remaining <= 0 {
			stopped = idx
			break
		}
		if all[idx].Status != "active" {
			continue
		}
		claims, err := s.deliveries.ClaimDue(ctx, all[idx].ID, now, remaining, func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan {
			return s.policy.PlanAttempt(d, now)
		})
		if err != nil {
			s.log.Warn("webhooks: claim due deliveries failed", "project_id", all[idx].ID, "error", err)
			continue
		}
		if len(claims) > 0 {
			s.runClaims(ctx, all[idx].ID, claims, now)
		}
		remaining -= len(claims)
		attempted += len(claims)
	}
	if stopped >= 0 {
		s.scanCursor.ResumeAt(stopped)
	} else {
		s.scanCursor.Complete()
	}
	return attempted, nil
}

// runClaims 执行一批已领取的投递（有界并发；每条 claim 的 next_attempt_at
// 已由领取 CAS 推进——投递中崩溃的行按梯度退避后自愈）。
func (s *Service) runClaims(ctx context.Context, projectID string, claims []domainwebhooks.DeliveryClaim, now time.Time) {
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	for i := range claims {
		claim := &claims[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.deliverOne(ctx, projectID, claim, now)
		}()
	}
	wg.Wait()
}

// deliverOne 执行单次投递尝试并收口：2xx → delivered；非 2xx → 审计面
// 补记（next_attempt_at 已由领取按梯度推进；终态判定在领取期已完成）。
func (s *Service) deliverOne(ctx context.Context, projectID string, claim *domainwebhooks.DeliveryClaim, now time.Time) {
	w, err := s.repo.GetWebhook(ctx, projectID, claim.WebhookID)
	if err != nil {
		// 订阅已删除（外键级联只清投递行，在途 claim 仍持有 id）：放弃本次，
		// 行留 pending 下轮领取时自然不再有 webhook——记审计面避免静默。
		s.log.Warn("webhooks: subscription missing for claim; skipping", "project_id", projectID,
			"webhook_id", claim.WebhookID, "delivery_id", claim.ID, "error", err)
		webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "error").Inc()
		return
	}
	current, previous, err := s.effectiveSecrets(ctx, w)
	if err != nil {
		// 密钥解密失败（密钥材料配置错误等）：行留 pending、下轮重试；
		// next_attempt_at 已推进，不热循环。
		s.log.Error("webhooks: decrypt secret failed; attempt skipped", "project_id", projectID,
			"webhook_id", w.ID, "delivery_id", claim.ID, "error", err)
		if rerr := s.deliveries.RecordAttemptError(ctx, projectID, claim.ID, now, 0, truncateErr(err)); rerr != nil {
			s.log.Warn("webhooks: record attempt error failed", "delivery_id", claim.ID, "error", rerr)
		}
		webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "error").Inc()
		return
	}

	var ev domainfunctions.CloudEvent
	if err := json.Unmarshal(claim.Payload, &ev); err != nil {
		// 载荷损坏（理论不可达：行内 payload 由本服务写入）：终态化防
		// 毒行无限重投。
		s.log.Error("webhooks: corrupt delivery payload; marking failed", "delivery_id", claim.ID, "error", err)
		_ = s.deliveries.MarkFailed(ctx, projectID, claim.ID, now, 0, truncateErr(fmt.Errorf("corrupt payload: %w", err)))
		webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "failed").Inc()
		webhookDLQTotal.WithLabelValues(projectID).Inc()
		return
	}

	start := time.Now()
	code := 0
	nres, nerr := s.notifier.Notify(ctx, domainfunctions.Notification{
		TargetURL:            w.URL,
		TargetSecret:         current,
		TargetPreviousSecret: previous,
		Event:                ev,
		TimeoutSeconds:       s.timeoutSeconds,
	})
	duration := time.Since(start)
	_ = nres // 响应尾部存档（D10）为函数面 invocation 行服务；webhook 投递行沿用 response_code + last_error 审计面
	if nerr == nil {
		code = 200 // 事件轴 2xx = 送达（HTTPNotifier 折叠具体 2xx 细节）
		if err := s.deliveries.MarkDelivered(ctx, projectID, claim.ID, time.Now(), code); err != nil {
			s.log.Warn("webhooks: mark delivered failed", "delivery_id", claim.ID, "error", err)
			webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "error").Inc()
			return
		}
		webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "delivered").Inc()
		webhookDeliveryDurationSeconds.WithLabelValues(projectID, "delivered").Observe(duration.Seconds())
		return
	}
	// 非终态失败：梯度已由领取推进，补记审计面。
	code = deliveryResponseCode(nerr)
	if err := s.deliveries.RecordAttemptError(ctx, projectID, claim.ID, time.Now(), code, truncateErr(nerr)); err != nil {
		s.log.Warn("webhooks: record attempt error failed", "delivery_id", claim.ID, "error", err)
	}
	webhookDeliveryAttemptsTotal.WithLabelValues(projectID, "retry").Inc()
	webhookDeliveryDurationSeconds.WithLabelValues(projectID, "retry").Observe(duration.Seconds())
	s.log.Warn("webhooks: delivery attempt failed",
		"project_id", projectID, "webhook_id", claim.WebhookID, "delivery_id", claim.ID,
		"attempt", claim.Attempts, "response_code", code, "error", nerr)
}

// ——DLQ 清理——

// PruneOldDeliveries 清理全部 active 项目内超保留窗的终态投递行（worker
// 低频 ticker 驱动；pending 行由重试窗终态化后自然进入下一轮清理口径）。
// 返回删除条数。
func (s *Service) PruneOldDeliveries(ctx context.Context, budget int) (int64, error) {
	if s.deliveries == nil || s.projects == nil {
		return 0, nil
	}
	all, err := s.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-s.retention)
	var pruned int64
	for i := range all {
		if all[i].Status != "active" {
			continue
		}
		if budget > 0 && pruned >= int64(budget) {
			break
		}
		n, err := s.deliveries.PruneTerminal(ctx, all[i].ID, cutoff, 500)
		if err != nil {
			s.log.Warn("webhooks: prune deliveries failed", "project_id", all[i].ID, "error", err)
			continue
		}
		pruned += n
	}
	return pruned, nil
}

// ——辅助——

// deliveryResponseCode 从 Notifier 错误中 best-effort 提取 HTTP 响应码
// （事件轴契约只区分 2xx/非 2xx，非 2xx 码折叠在错误文本 "status %d" 内；
// 连不通/超时未达 HTTP 层 → 0）。解析失败不影响投递状态机（审计面语义）。
func deliveryResponseCode(err error) int {
	if err == nil {
		return 0
	}
	msg := ""
	if st, ok := status.FromError(err); ok {
		msg = st.Message()
	} else {
		msg = err.Error()
	}
	idx := strings.LastIndex(msg, "status ")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("status "):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}

// truncateErr 收敛错误摘要（列宽护栏；对齐 invoker truncateTail 口径）。
func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 512 {
		return s[:512] + "…"
	}
	return s
}
