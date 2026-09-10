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
		"generic call: invoke any Server API method by full gRPC method name (escape hatch)",
		`rpc <full-method> [--data '<json>']

Invoke any unary Server API method by its full gRPC method name (except
APIKeysService — API key credentials are forbidden by the server). --data
is the request protojson (camelCase field names, fields optional).

Examples:
  torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{"pageSize": 10}'
  torchwood rpc /torchwood.server.v1.HealthService/Check`,
		func(fs *flag.FlagSet) {
			fs.StringVar(&data, "data", "", "request JSON (protojson, camelCase field names)")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, args[0], data)
		})
}
