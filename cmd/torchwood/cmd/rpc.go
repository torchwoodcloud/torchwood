package cmd

import (
	"flag"

	"github.com/lynx-go/commands"
)

// newRPCCmd 是逃生舱命令：按完整 gRPC 方法名调用任意 Server API 方法，
// --data 以 protojson 填充请求体（动态分发见 sdk/go/server.InvokeJSON，
// 完整性由 SDK 测试保证）。
func newRPCCmd(g *globalFlags) *verb {
	var data string
	return newVerb(g, "rpc",
		"通用调用：按完整 gRPC 方法名调用任意 Server API 方法（逃生舱）",
		`rpc <full-method> [--data '<json>']

按完整 gRPC 方法名调用 Server API 的任意 unary 方法（APIKeysService 除外——
API Key 凭证被服务端禁止调用）。--data 为请求的 protojson（camelCase 字段名，
可省略字段）。

示例：
  torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{"pageSize": 10}'
  torchwood rpc /torchwood.server.v1.HealthService/Check`,
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "请求 JSON（protojson，camelCase 字段名）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, args[0], data)
		})
}
