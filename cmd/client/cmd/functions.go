package cmd

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"

	"github.com/lynx-go/commands"
)

const (
	methodFunctionsRuntimes         = "/torchwood.server.v1.FunctionsService/ListRuntimes"
	methodFunctionsSpecifications   = "/torchwood.server.v1.FunctionsService/ListSpecifications"
	methodFunctionsCreate           = "/torchwood.server.v1.FunctionsService/CreateFunction"
	methodFunctionsList             = "/torchwood.server.v1.FunctionsService/ListFunctions"
	methodFunctionsGet              = "/torchwood.server.v1.FunctionsService/GetFunction"
	methodFunctionsUpdate           = "/torchwood.server.v1.FunctionsService/UpdateFunction"
	methodFunctionsDelete           = "/torchwood.server.v1.FunctionsService/DeleteFunction"
	methodFunctionsCreateDeployment = "/torchwood.server.v1.FunctionsService/CreateDeployment"
	methodFunctionsListDeployments  = "/torchwood.server.v1.FunctionsService/ListDeployments"
	methodFunctionsGetDeployment    = "/torchwood.server.v1.FunctionsService/GetDeployment"
	methodFunctionsDeleteDeployment = "/torchwood.server.v1.FunctionsService/DeleteDeployment"
	methodFunctionsSetVariables     = "/torchwood.server.v1.FunctionsService/SetVariables"
	methodFunctionsGetVariables     = "/torchwood.server.v1.FunctionsService/GetVariables"
	methodFunctionsCreateExecution  = "/torchwood.server.v1.FunctionsService/CreateExecution"
	methodFunctionsListExecutions   = "/torchwood.server.v1.FunctionsService/ListExecutions"
	methodFunctionsGetExecution     = "/torchwood.server.v1.FunctionsService/GetExecution"
)

// newFunctionsCmd 覆盖 FunctionsService 全部 16 个方法：
// runtimes/specifications、functions（create/list/get/update/delete）、
// deployments（create/list/get/delete）、variables（set/get）、
// executions（create/list/get）。
// deployments create 由 CLI 读取 zip 文件并 base64 编码后走 gRPC 纯消息
// （bytes code，≤1MiB 建议；gRPC 通道上限 8MiB，与服务端 MaxRecvMsgSize
// 对齐），更大的代码包走 multipart 上传（独立 HTTP handler，CLI 不提供）。
func newFunctionsCmd(g *globalFlags) *group {
	return newGroup(g, "functions", "函数管理（FunctionsService 全部方法）", func(sub *commands.App) {
		sub.Register(
			newFunctionsRuntimesCmd(g),
			newFunctionsSpecificationsCmd(g),
			newFunctionsListCmd(g),
			newFunctionsCreateCmd(g),
			newFunctionsGetCmd(g),
			newFunctionsUpdateCmd(g),
			newFunctionsDeleteCmd(g),
			newFunctionsDeploymentsCmd(g),
			newFunctionsVariablesCmd(g),
			newFunctionsExecutionsCmd(g),
		)
	})
}

func newFunctionsRuntimesCmd(g *globalFlags) *verb {
	return newVerb(g, "runtimes", "列出支持的运行时", "functions runtimes", nil,
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsRuntimes, nil)
		})
}

func newFunctionsSpecificationsCmd(g *globalFlags) *verb {
	return newVerb(g, "specifications", "列出支持的资源配置（spec）", "functions specifications", nil,
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsSpecifications, nil)
		})
}

func newFunctionsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "列出函数", "functions list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "每页条数（服务端默认 50，上限 1000）")
			fs.StringVar(&pageToken, "page-token", "", "上一页返回的 next_page_token")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsList, listJSON(pageSize, pageToken))
		})
}

func newFunctionsCreateCmd(g *globalFlags) *verb {
	var id, name, runtime, entrypoint string
	var timeoutSeconds int
	var spec string
	var enabled bool
	return newVerb(g, "create", "创建函数", "functions create --id <id> --name <name> --runtime <runtime>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "函数 ID（必填）")
			fs.StringVar(&name, "name", "", "函数名称（必填）")
			fs.StringVar(&runtime, "runtime", "", "运行时（必填，见 runtimes 命令）")
			fs.StringVar(&entrypoint, "entrypoint", "", "入口文件（缺省按运行时取默认值）")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "超时秒数（1-300，缺省服务端默认）")
			fs.StringVar(&spec, "spec", "", "资源配置（缺省 shared-1x，见 specifications 命令）")
			fs.BoolVar(&enabled, "enabled", false, "是否启用（显式传 --enabled=true/false 才生效）")
		},
		func(v *verb, env *commands.Environment, _ []string) error {
			req, err := buildCreateFunctionReq(v, id, name, runtime, entrypoint, timeoutSeconds, spec, enabled)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsCreate, req)
		})
}

func newFunctionsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取函数", "functions get <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsGet, map[string]any{"functionId": args[0]})
		})
}

func newFunctionsUpdateCmd(g *globalFlags) *verb {
	var name, entrypoint, spec string
	var timeoutSeconds int
	var enabled bool
	return newVerb(g, "update", "更新函数（仅更新显式传入的字段）", "functions update <function-id> [--name] [--entrypoint] [--timeout-seconds] [--spec] [--enabled]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "函数名称")
			fs.StringVar(&entrypoint, "entrypoint", "", "入口文件")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "超时秒数（1-300）")
			fs.StringVar(&spec, "spec", "", "资源配置")
			fs.BoolVar(&enabled, "enabled", false, "是否启用（显式传 --enabled=true/false 才生效）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildUpdateFunctionReq(v, args[0], name, entrypoint, timeoutSeconds, spec, enabled)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsUpdate, req)
		})
}

func newFunctionsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除函数", "functions delete <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDelete, map[string]any{"functionId": args[0]})
		})
}

// newFunctionsDeploymentsCmd: functions deployments create/list/get/delete。
func newFunctionsDeploymentsCmd(g *globalFlags) *group {
	return newGroup(g, "deployments", "函数部署管理（code 为 zip 代码包）", func(sub *commands.App) {
		sub.Register(
			newFunctionsDeploymentsCreateCmd(g),
			newFunctionsDeploymentsListCmd(g),
			newFunctionsDeploymentsGetCmd(g),
			newFunctionsDeploymentsDeleteCmd(g),
		)
	})
}

func newFunctionsDeploymentsCreateCmd(g *globalFlags) *verb {
	var code string
	return newVerb(g, "create", "上传 zip 代码包创建部署（gRPC 纯消息通道，上限 8MiB）", "functions deployments create <function-id> --code <zip-file>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&code, "code", "", "zip 代码包路径（必填；gRPC 消息通道上限 8MiB，建议单包 ≤1MiB；更大的代码包请走 multipart 上传接口，上限 50MiB）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildCreateDeploymentReq(args[0], code)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsCreateDeployment, req)
		})
}

func newFunctionsDeploymentsListCmd(g *globalFlags) *verb {
	return newVerb(g, "list", "列出函数部署", "functions deployments list <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsListDeployments, map[string]any{"functionId": args[0]})
		})
}

func newFunctionsDeploymentsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "按 ID 获取部署", "functions deployments get <function-id> <deployment-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsGetDeployment, map[string]any{"functionId": args[0], "deploymentId": args[1]})
		})
}

func newFunctionsDeploymentsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "删除部署", "functions deployments delete <function-id> <deployment-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDeleteDeployment, map[string]any{"functionId": args[0], "deploymentId": args[1]})
		})
}

// newFunctionsVariablesCmd: functions variables set/get。
func newFunctionsVariablesCmd(g *globalFlags) *group {
	return newGroup(g, "variables", "函数环境变量管理", func(sub *commands.App) {
		sub.Register(
			newFunctionsVariablesSetCmd(g),
			newVerb(g, "get", "获取函数环境变量", "functions variables get <function-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodFunctionsGetVariables, map[string]any{"functionId": args[0]})
				}),
		)
	})
}

func newFunctionsVariablesSetCmd(g *globalFlags) *verb {
	var vars string
	return newVerb(g, "set", "全量替换环境变量（--vars 为 JSON 对象）", "functions variables set <function-id> --vars '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&vars, "vars", "", "环境变量 JSON 对象（必填，如 '{\"FOO\":\"bar\"}'）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildSetVariablesReq(args[0], vars)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsSetVariables, req)
		})
}

// newFunctionsExecutionsCmd: functions executions create/list/get。
func newFunctionsExecutionsCmd(g *globalFlags) *group {
	return newGroup(g, "executions", "函数执行管理", func(sub *commands.App) {
		sub.Register(
			newFunctionsExecutionsCreateCmd(g),
			newVerb(g, "list", "列出执行记录（最近 100 条）", "functions executions list <function-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodFunctionsListExecutions, map[string]any{"functionId": args[0]})
				}),
			newVerb(g, "get", "按 ID 获取执行记录", "functions executions get <function-id> <execution-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 2); err != nil {
						return err
					}
					return call(g, env, methodFunctionsGetExecution, map[string]any{"functionId": args[0], "executionId": args[1]})
				}),
		)
	})
}

func newFunctionsExecutionsCreateCmd(g *globalFlags) *verb {
	var input, deploymentID string
	var async bool
	return newVerb(g, "create", "创建执行（缺省用最新 ready 部署）", "functions executions create <function-id> --input <json>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "", "执行输入（必填，须为合法 JSON 字符串，≤64KB）")
			fs.StringVar(&deploymentID, "deployment-id", "", "指定部署（缺省用最新 ready 部署）")
			fs.BoolVar(&async, "async", false, "异步执行（显式传 --async=true 才生效）")
		},
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			req, err := buildCreateExecutionReq(v, args[0], input, deploymentID, async)
			if err != nil {
				return err
			}
			return call(g, env, methodFunctionsCreateExecution, req)
		})
}

// buildCreateFunctionReq 构造 CreateFunctionRequest（id/name/runtime 必填）。
func buildCreateFunctionReq(v *verb, id, name, runtime, entrypoint string, timeoutSeconds int, spec string, enabled bool) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("--id 必填")
	}
	if name == "" {
		return nil, fmt.Errorf("--name 必填")
	}
	if runtime == "" {
		return nil, fmt.Errorf("--runtime 必填（可用 runtimes 命令查看）")
	}
	req := map[string]any{"id": id, "name": name, "runtime": runtime}
	if entrypoint != "" {
		req["entrypoint"] = entrypoint
	}
	setChanged(v, "timeout-seconds", req, "timeoutSeconds", timeoutSeconds)
	setChanged(v, "spec", req, "spec", spec)
	setChanged(v, "enabled", req, "enabled", enabled)
	return req, nil
}

// buildUpdateFunctionReq 构造 UpdateFunctionRequest：仅设置显式传入的字段。
func buildUpdateFunctionReq(v *verb, functionID string, name, entrypoint string, timeoutSeconds int, spec string, enabled bool) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("缺少 function-id")
	}
	req := map[string]any{"functionId": functionID}
	setChanged(v, "name", req, "name", name)
	setChanged(v, "entrypoint", req, "entrypoint", entrypoint)
	setChanged(v, "timeout-seconds", req, "timeoutSeconds", timeoutSeconds)
	setChanged(v, "spec", req, "spec", spec)
	setChanged(v, "enabled", req, "enabled", enabled)
	return req, nil
}

// buildCreateDeploymentReq 读取 zip 文件并构造 CreateDeploymentRequest
// （code 为 bytes 字段，CLI 负责读文件后 base64 编码，不让用户手写）。
func buildCreateDeploymentReq(functionID, codePath string) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("缺少 function-id")
	}
	if codePath == "" {
		return nil, fmt.Errorf("--code 必填（zip 代码包路径）")
	}
	code, err := os.ReadFile(codePath)
	if err != nil {
		return nil, fmt.Errorf("读取 --code 失败：%v", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("--code 为空文件")
	}
	if len(code) > 8<<20 {
		return nil, fmt.Errorf("--code 超过 8MiB（gRPC 消息通道上限，与服务端 MaxRecvMsgSize 对齐）；更大的代码包请走 multipart 上传接口（上限 50MiB）")
	}
	return map[string]any{"functionId": functionID, "code": base64.StdEncoding.EncodeToString(code)}, nil
}

// buildSetVariablesReq 构造 SetVariablesRequest（--vars 为 JSON 对象）。
func buildSetVariablesReq(functionID, vars string) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("缺少 function-id")
	}
	kv, err := jsonStringMap(vars, "--vars")
	if err != nil {
		return nil, err
	}
	if len(kv) == 0 {
		return nil, fmt.Errorf("--vars 必填（环境变量 JSON 对象）")
	}
	list := make([]map[string]string, 0, len(kv))
	for k, v := range kv {
		list = append(list, map[string]string{"key": k, "value": v})
	}
	return map[string]any{"functionId": functionID, "variables": list}, nil
}

// buildCreateExecutionReq 构造 CreateExecutionRequest（--input 必填，与服务端
// 校验一致）。
func buildCreateExecutionReq(v *verb, functionID, input, deploymentID string, async bool) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("缺少 function-id")
	}
	if input == "" {
		return nil, fmt.Errorf("--input 必填（执行输入 JSON 字符串）")
	}
	req := map[string]any{"functionId": functionID, "data": input}
	setChanged(v, "deployment-id", req, "deploymentId", deploymentID)
	setChanged(v, "async", req, "async", async)
	return req, nil
}
