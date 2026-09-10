package testutil

import (
	"testing"
	"time"
)

// Eventually 以 50ms 间隔轮询 check，通过即返回；超时后 t.Fatal 并附最后
// 状态。用于替换测试中的固定 sleep（J6-4 flaky 治理）：等待条件成立而非
// 猜测时长，负载下不再假阴。
func Eventually(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := check()
	for !last {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s (last state: %v)", timeout, last)
		}
		time.Sleep(50 * time.Millisecond)
		last = check()
	}
}
