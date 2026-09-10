//go:build !linux

package functions

// setTestUmask 非 Linux 平台 no-op（umask 为 POSIX 语义；Windows 构建路径
// 的 mode 归一化由 tarDir 无条件保证，不依赖本助手生效）。
func setTestUmask(int) int { return 0o022 }
