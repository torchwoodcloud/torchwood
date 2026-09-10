package cmd

import (
	"fmt"

	"github.com/lynx-go/commands"

	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// newUUIDCmd 生成本地 UUID v4（与服务端 idgen.UUID 同源），无需 API key。
// 输出纯文本 ID（一行一个），便于 shell 捕获后传给 --id 等客户端指定 ID 的命令。
func newUUIDCmd(g *globalFlags) *verb {
	return newPublicVerb(g, "uuid", "生成本地 UUID（无需 API key）", "uuid", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := noArgs(v, args); err != nil {
				return err
			}
			_, err := fmt.Fprintln(env.Stdout, idgen.UUID().String())
			return err
		})
}
