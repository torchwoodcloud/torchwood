//go:build !linux

package dispatcher

// setTestUmask 非 Linux 平台 no-op（umask 为 POSIX 语义；Windows 构建路径
// 的 mode 归一化由 tarDir 无条件保证，不依赖本助手生效）。返回空恢复函数
// 以保持调用点跨平台同构。
func setTestUmask(int) (restore func()) { return func() {} }
