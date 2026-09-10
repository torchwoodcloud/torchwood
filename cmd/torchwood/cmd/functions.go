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
	return newGroup(g, "functions", "function management (all FunctionsService methods)", func(sub *commands.App) {
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
	return newVerb(g, "runtimes", "list supported runtimes", "functions runtimes", nil,
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsRuntimes, nil)
		})
}

func newFunctionsSpecificationsCmd(g *globalFlags) *verb {
	return newVerb(g, "specifications", "list supported resource specifications (spec)", "functions specifications", nil,
		func(v *verb, env *commands.Environment, _ []string) error {
			return call(g, env, methodFunctionsSpecifications, nil)
		})
}

func newFunctionsListCmd(g *globalFlags) *verb {
	var pageSize int
	var pageToken string
	return newVerb(g, "list", "list functions", "functions list",
		func(fs *flag.FlagSet) {
			fs.IntVar(&pageSize, "page-size", 0, "page size (server default 50, max 1000)")
			fs.StringVar(&pageToken, "page-token", "", "next_page_token returned by the previous page")
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
	return newVerb(g, "create", "create a function", "functions create --id <id> --name <name> --runtime <runtime>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "function ID (required)")
			fs.StringVar(&name, "name", "", "function name (required)")
			fs.StringVar(&runtime, "runtime", "", "runtime (required, see the runtimes command)")
			fs.StringVar(&entrypoint, "entrypoint", "", "entrypoint file (defaults to the runtime default)")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "timeout in seconds (1-300, server default when omitted)")
			fs.StringVar(&spec, "spec", "", "resource specification (defaults to shared-1x, see the specifications command)")
			fs.BoolVar(&enabled, "enabled", false, "whether enabled (pass --enabled=true/false explicitly to take effect)")
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
	return newVerb(g, "get", "get a function by ID", "functions get <function-id>", nil,
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
	return newVerb(g, "update", "update a function (only explicitly passed fields)", "functions update <function-id> [--name] [--entrypoint] [--timeout-seconds] [--spec] [--enabled]",
		func(fs *flag.FlagSet) {
			fs.StringVar(&name, "name", "", "function name")
			fs.StringVar(&entrypoint, "entrypoint", "", "entrypoint file")
			fs.IntVar(&timeoutSeconds, "timeout-seconds", 0, "timeout in seconds (1-300)")
			fs.StringVar(&spec, "spec", "", "resource specification")
			fs.BoolVar(&enabled, "enabled", false, "whether enabled (pass --enabled=true/false explicitly to take effect)")
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
	return newVerb(g, "delete", "delete a function", "functions delete <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDelete, map[string]any{"functionId": args[0]})
		})
}

// newFunctionsDeploymentsCmd: functions deployments create/list/get/delete。
func newFunctionsDeploymentsCmd(g *globalFlags) *group {
	return newGroup(g, "deployments", "function deployment management (code is a zip archive)", func(sub *commands.App) {
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
	return newVerb(g, "create", "create a deployment by uploading a zip archive (gRPC message channel, 8MiB max)", "functions deployments create <function-id> --code <zip-file>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&code, "code", "", "path to the zip archive (required; gRPC message channel caps at 8MiB, single package ≤1MiB recommended; for larger packages use the multipart upload API, 50MiB max)")
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
	return newVerb(g, "list", "list function deployments", "functions deployments list <function-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 1); err != nil {
				return err
			}
			return call(g, env, methodFunctionsListDeployments, map[string]any{"functionId": args[0]})
		})
}

func newFunctionsDeploymentsGetCmd(g *globalFlags) *verb {
	return newVerb(g, "get", "get a deployment by ID", "functions deployments get <function-id> <deployment-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsGetDeployment, map[string]any{"functionId": args[0], "deploymentId": args[1]})
		})
}

func newFunctionsDeploymentsDeleteCmd(g *globalFlags) *verb {
	return newVerb(g, "delete", "delete a deployment", "functions deployments delete <function-id> <deployment-id>", nil,
		func(v *verb, env *commands.Environment, args []string) error {
			if err := exactArgs(v, args, 2); err != nil {
				return err
			}
			return call(g, env, methodFunctionsDeleteDeployment, map[string]any{"functionId": args[0], "deploymentId": args[1]})
		})
}

// newFunctionsVariablesCmd: functions variables set/get。
func newFunctionsVariablesCmd(g *globalFlags) *group {
	return newGroup(g, "variables", "function environment variable management", func(sub *commands.App) {
		sub.Register(
			newFunctionsVariablesSetCmd(g),
			newVerb(g, "get", "get function environment variables", "functions variables get <function-id>", nil,
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
	return newVerb(g, "set", "replace environment variables (--vars is a JSON object)", "functions variables set <function-id> --vars '{...}'",
		func(fs *flag.FlagSet) {
			fs.StringVar(&vars, "vars", "", "environment variables JSON object (required, e.g. '{\"FOO\":\"bar\"}')")
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
	return newGroup(g, "executions", "function execution management", func(sub *commands.App) {
		sub.Register(
			newFunctionsExecutionsCreateCmd(g),
			newVerb(g, "list", "list execution records (latest 100)", "functions executions list <function-id>", nil,
				func(v *verb, env *commands.Environment, args []string) error {
					if err := exactArgs(v, args, 1); err != nil {
						return err
					}
					return call(g, env, methodFunctionsListExecutions, map[string]any{"functionId": args[0]})
				}),
			newVerb(g, "get", "get an execution record by ID", "functions executions get <function-id> <execution-id>", nil,
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
	return newVerb(g, "create", "create an execution (latest ready deployment by default)", "functions executions create <function-id> --input <json>",
		func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "", "execution input (required, must be a valid JSON string, ≤64KB)")
			fs.StringVar(&deploymentID, "deployment-id", "", "target deployment (latest ready deployment by default)")
			fs.BoolVar(&async, "async", false, "run asynchronously (only takes effect when --async=true is passed explicitly)")
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
		return nil, fmt.Errorf("--id is required")
	}
	if name == "" {
		return nil, fmt.Errorf("--name is required")
	}
	if runtime == "" {
		return nil, fmt.Errorf("--runtime is required (see the runtimes command)")
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
		return nil, fmt.Errorf("missing function-id")
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
		return nil, fmt.Errorf("missing function-id")
	}
	if codePath == "" {
		return nil, fmt.Errorf("--code is required (path to the zip archive)")
	}
	code, err := os.ReadFile(codePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read --code: %v", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("--code is an empty file")
	}
	if len(code) > 8<<20 {
		return nil, fmt.Errorf("--code exceeds 8MiB (gRPC message channel limit, aligned with the server MaxRecvMsgSize); for larger packages use the multipart upload API (50MiB max)")
	}
	return map[string]any{"functionId": functionID, "code": base64.StdEncoding.EncodeToString(code)}, nil
}

// buildSetVariablesReq 构造 SetVariablesRequest（--vars 为 JSON 对象）。
func buildSetVariablesReq(functionID, vars string) (map[string]any, error) {
	if functionID == "" {
		return nil, fmt.Errorf("missing function-id")
	}
	kv, err := jsonStringMap(vars, "--vars")
	if err != nil {
		return nil, err
	}
	if len(kv) == 0 {
		return nil, fmt.Errorf("--vars is required (environment variables JSON object)")
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
		return nil, fmt.Errorf("missing function-id")
	}
	if input == "" {
		return nil, fmt.Errorf("--input is required (execution input JSON string)")
	}
	req := map[string]any{"functionId": functionID, "data": input}
	setChanged(v, "deployment-id", req, "deploymentId", deploymentID)
	setChanged(v, "async", req, "async", async)
	return req, nil
}
