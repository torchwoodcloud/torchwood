package queue

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
)

// redisQueue 是 shared.Queue 的 Redis Stream 实现（至少一次，XADD/XREADGROUP/XACK）。
type redisQueue struct {
	client   *redis.Client
	consumer string
}

const (
	queueGroupSuffix = "-group"
)

// claimMinIdle 是 PEL 认领的最小 idle，必须显著大于消息最长在途处理时长
// （补构建 5min 超时 + 执行超时），否则并发消费 goroutine 会把仍在处理中的
// 消息从 PEL 重新认领、并发重复执行。15min 只服务崩溃恢复语义；单元测试
// 需要快速重投时直接覆写本变量。
var claimMinIdle = 15 * time.Minute

// NewRedisQueue creates a Redis-backed queue adapter.
func NewRedisQueue(client *redis.Client) domainshared.Queue {
	hostname, _ := os.Hostname()
	consumer := fmt.Sprintf("%s:%d", hostname, os.Getpid())
	return &redisQueue{client: client, consumer: consumer}
}

func queueGroup(queue string) string {
	return queue + queueGroupSuffix
}

func (q *redisQueue) Enqueue(ctx context.Context, name string, payload []byte) error {
	// 不设 MaxLen 裁剪：至少一次语义下未投递消息不得被近似裁剪丢弃，
	// 积压治理交给消息消费（重试超限标 failed）。
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return q.client.XAdd(ctx2, &redis.XAddArgs{
		Stream: name,
		Values: map[string]any{"payload": string(payload)},
	}).Err()
}

func (q *redisQueue) ensureGroup(ctx context.Context, name string) error {
	// 保持 "0-0" 起始：组首次创建前已入队的消息（server 先于 worker 启动的
	// 部署顺序）必须可投递，"$" 会静默跳过它们。组被误删重建时的全量重放
	// 会产生重复投递，但 ProcessExecution 的 CAS 领取闸门
	// （TransitionExecutionStatus queued→building）把重复投递收敛为幂等跳过，
	// 重放无害（2026-08 评审 P1-4 定案：闸门兜底优于起始位取舍）。
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := q.client.XGroupCreateMkStream(ctx2, name, queueGroup(name), "0-0").Err()
	if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (q *redisQueue) Dequeue(ctx context.Context, name string, timeout time.Duration) ([]byte, string, error) {
	if err := q.ensureGroup(ctx, name); err != nil {
		return nil, "", err
	}
	group := queueGroup(name)
	// 先认领 PEL 中超过 claimMinIdle 的消息（崩溃/未 Ack 重投）。
	claimCtx, cancelClaim := context.WithTimeout(ctx, 5*time.Second)
	claimed, _, err := q.client.XAutoClaim(claimCtx, &redis.XAutoClaimArgs{
		Stream:   name,
		Group:    group,
		Consumer: q.consumer,
		MinIdle:  claimMinIdle,
		Start:    "0-0",
		Count:    1,
	}).Result()
	cancelClaim()
	if err == nil && len(claimed) > 0 {
		raw, ok := claimed[0].Values["payload"]
		if !ok {
			// 坏条目直接 ACK 丢弃。
			ackCtx, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
			_ = q.client.XAck(ackCtx, name, group, claimed[0].ID).Err()
			cancelAck()
			return nil, "", fmt.Errorf("queue entry missing payload")
		}
		s, ok := raw.(string)
		if !ok {
			ackCtx, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
			_ = q.client.XAck(ackCtx, name, group, claimed[0].ID).Err()
			cancelAck()
			return nil, "", fmt.Errorf("queue payload not string")
		}
		return []byte(s), claimed[0].ID, nil
	}
	block := timeout
	if timeout <= 0 {
		block = 0
	}
	streams, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: q.consumer,
		Streams:  []string{name, ">"},
		Count:    1,
		Block:    block,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, "", nil
		}
		return nil, "", err
	}
	if len(streams) == 0 || len(streams[0].Messages) == 0 {
		return nil, "", nil
	}
	msg := streams[0].Messages[0]
	raw, ok := msg.Values["payload"]
	if !ok {
		ackCtx, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
		_ = q.client.XAck(ackCtx, name, group, msg.ID).Err()
		cancelAck()
		return nil, "", fmt.Errorf("queue entry missing payload")
	}
	s, ok := raw.(string)
	if !ok {
		ackCtx, cancelAck := context.WithTimeout(context.Background(), 5*time.Second)
		_ = q.client.XAck(ackCtx, name, group, msg.ID).Err()
		cancelAck()
		return nil, "", fmt.Errorf("queue payload not string")
	}
	return []byte(s), msg.ID, nil
}

func (q *redisQueue) Trim(ctx context.Context, queue string, maxLen int64) error {
	if maxLen <= 0 {
		return nil
	}
	// APPROX：单次 O(被裁剪部分)，不精准到 maxLen——PEL 未 Ack 消息理论上
	// 可能被裁掉，但 claimMinIdle(15min) 崩溃恢复窗口远小于裁剪水位差，
	// 风险可接受（与 Enqueue 不设 MaxLen 的权衡一致，见其注释）。
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return q.client.XTrimMaxLenApprox(ctx2, queue, maxLen, 0).Err()
}

func (q *redisQueue) Ack(ctx context.Context, queue string, ack string) error {
	if ack == "" {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return q.client.XAck(ctx2, queue, queueGroup(queue), ack).Err()
}
