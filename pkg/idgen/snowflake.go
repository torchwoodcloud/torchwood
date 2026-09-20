package idgen

import (
	"fmt"
	"sync"
	"time"
)

const (
	snowflakeEpochMs  = 1577836800000 // 2020-01-01 UTC
	snowflakeNodeBits = 10
	snowflakeSeqBits  = 12
	snowflakeMaxNode  = (1 << snowflakeNodeBits) - 1
	snowflakeMaxSeq   = (1 << snowflakeSeqBits) - 1
)

// Snowflake generates 64-bit time-sortable numeric string IDs.
type Snowflake struct {
	mu       sync.Mutex
	nodeID   int64
	lastMs   int64
	sequence int64
}

func NewSnowflake(nodeID int64) (*Snowflake, error) {
	if nodeID < 0 || nodeID > snowflakeMaxNode {
		return nil, fmt.Errorf("snowflake node_id must be between 0 and %d", snowflakeMaxNode)
	}
	return &Snowflake{nodeID: nodeID}, nil
}

func (s *Snowflake) NextString() string {
	return fmt.Sprintf("%d", s.Next())
}

func (s *Snowflake) Next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	// 时钟回拨防护：now 落后于上次发号毫秒（NTP 步进/手工校时）时自旋等待
	// 系统时钟追平，期间不发号。否则回拨后与回拨前同毫秒同节点会发出完全
	// 相同的 ID（主键冲突或静默撞号）。不做追平硬超时：回拨幅度大到进程
	// 无法承受时，持锁阻塞优于发出重复 ID；NTP 步进通常毫秒级，等待代价
	// 轻微（调用方本就持锁串行，单次自旋只多等回拨幅度）。
	// 自旋带 100µs sleep 而非纯忙等：回拨窗口内每毫秒至多 ~10 次 time.Now
	// 调用，不空烧 CPU；唤醒粒度只影响追平后的放行时机，不影响发号语义
	// （追平后落入 now==lastMs 分支正常续号）。
	for now < s.lastMs {
		time.Sleep(100 * time.Microsecond)
		now = time.Now().UnixMilli()
	}
	if now == s.lastMs {
		s.sequence = (s.sequence + 1) & snowflakeMaxSeq
		if s.sequence == 0 {
			for now <= s.lastMs {
				now = time.Now().UnixMilli()
			}
		}
	} else {
		s.sequence = 0
	}
	s.lastMs = now

	id := ((now - snowflakeEpochMs) << (snowflakeNodeBits + snowflakeSeqBits)) |
		(s.nodeID << snowflakeSeqBits) |
		s.sequence
	return id
}
