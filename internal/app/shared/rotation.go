package shared

import "sync"

// ProjectRotation 是预算型 worker 的项目轮转游标（队尾饥饿防护，设计
// project-data-plane-schema §7）：每 tick 全局预算（K22）从头部的固定顺序
// 扣减会让持续到期的头部项目永远挤占队尾项目；轮转让每 tick 从上一轮
// 提前结束的下一个项目开始环形遍历，完整遍历一轮后回到头部。
//
// 并发安全（互斥锁保护游标）：同一 Functions 用例实例被 worker 的多个
// tick 循环共享（recoverLoop / cronLoop / 事件匹配器扫描各自独立 goroutine
// 驱动），v3 事件触发器引入的全生命周期并发测试暴露了原先「单循环串行」
// 假设的失效——游标读写必须互斥。
type ProjectRotation struct {
	mu   sync.Mutex
	next int
}

// Start 返回本轮起始下标（0 <= start < n）；n <= 0 时归零返回 0。
// 项目数变化（删项目）导致游标越界时自动回到头部。
func (r *ProjectRotation) Start(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 {
		r.next = 0
		return 0
	}
	if r.next >= n {
		r.next = 0
	}
	return r.next
}

// ResumeAt 在全局预算用尽、提前结束于下标 idx（该项目本轮尚未处理）时
// 记录：下一 tick 从 idx 开始。
func (r *ProjectRotation) ResumeAt(idx int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 {
		idx = 0
	}
	r.next = idx
}

// Complete 结束一轮完整遍历：游标回到头部。
func (r *ProjectRotation) Complete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = 0
}
