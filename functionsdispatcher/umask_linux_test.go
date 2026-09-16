//go:build linux

package functionsdispatcher

import "syscall"

// setTestUmask 置进程 umask 并返回恢复函数（测试复现生产 umask 掩蔽形态用；
// 见 daemon_integration_test.go 镜像权限坏档回归，恢复须等测试结束再执行）。
func setTestUmask(mask int) (restore func()) {
	old := syscall.Umask(mask)
	return func() { syscall.Umask(old) }
}
