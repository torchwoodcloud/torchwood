package console

import "embed"

// Dist contains the built Admin Console SPA.
// Run `mise run console:build` before building the Go binary.
//
// dist 构建产物全部 gitignore；入库的 dist/.gitkeep 仅保证新克隆与 CI 的
// `go build` 有可嵌入文件（未构建时 /console/ 由 runtime 返回提示页）。
// all: 前缀使点文件可被嵌入。
//
//go:embed all:dist
var Dist embed.FS
