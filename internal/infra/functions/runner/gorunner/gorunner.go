// Package gorunner 承载 Go runner 的平台侧资产（Go 一期，设计
// docs/design/functions-runtimes-and-sources.md §1）：AST 入口探测
// （DetectEntry）与 twmain bootstrap 渲染（RenderBootstrap）。
//
// 渲染产物写入用户构建上下文的保留目录 twmain/（无点前缀，规避 go build
// 点目录边角行为）：runtime.go = runner 协议实现（与 node runner.js 同契约
// 同版本，v5），main.go = 生成的引用用户根包的入口。模板资产命名 *.tmpl
// （不得命名为 .go）——包目录内的字面 .go 文件会进入平台构建（package main
// 的 main/serve 重复声明直接炸编译），go:embed 资产必须对平台编译器不可见。
//
// 硬约束（对抗审查 A3）：渲染产物的 import 白名单 = 标准库——vendor 模式
// 下 twmain 的依赖同样经 vendor 解析，任何第三方依赖都会让 vendor 用户
// 构建失败；render_test 对渲染产物做 stdlib-only 断言防回归。
//
// 本包保持叶子资产包形态：不 import infra/functions 根包与 runner 包
// （调用方为 dispatcher，探测产物由其映射进模板载体）。
package gorunner
