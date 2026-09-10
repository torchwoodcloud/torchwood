//go:build linux

package functionsdispatcher

import "syscall"

// setTestUmask 置进程 umask 并返回旧值（测试复现生产 umask 掩蔽形态用；
// 见 daemon_integration_test.go 镜像权限坏档回归）。
func setTestUmask(mask int) int { return syscall.Umask(mask) }
