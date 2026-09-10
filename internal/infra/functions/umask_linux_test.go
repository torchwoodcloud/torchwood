//go:build linux

package functions

import "syscall"

// setTestUmask 置进程 umask 并返回旧值（测试复现生产 umask 掩蔽形态用）。
func setTestUmask(mask int) int { return syscall.Umask(mask) }
