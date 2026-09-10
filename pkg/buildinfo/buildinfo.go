// Package buildinfo 承载编译期注入的版本信息（ldflags -X main.version 等）。
package buildinfo

// BuildInfo 是编译期注入的版本元数据，经 /v1/server/health/version 暴露。
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}
