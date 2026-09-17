package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

type mockExecutor struct {
	calls    []domainfunctions.Execution
	result   *domainfunctions.ExecutionResult
	err      error
	builds   int
	buildErr error
	removes  int
	// builds 是 Build 载荷断言面：specs 收集每次 BuildSpec（构建链一期
	// 定稿载荷，deployments 构建测试用）。
	specs []domainfunctions.BuildSpec
	// buildFn 非空时接管 Build 行为（ctx 形态/超时预算断言、可编程返回）。
	buildFn func(ctx context.Context, spec domainfunctions.BuildSpec) error
}

func (m *mockExecutor) Execute(_ context.Context, exec domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.calls = append(m.calls, exec)
	return m.result, m.err
}

func (m *mockExecutor) Build(ctx context.Context, spec domainfunctions.BuildSpec) error {
	m.specs = append(m.specs, spec)
	m.builds++
	if m.buildFn != nil {
		return m.buildFn(ctx, spec)
	}
	return m.buildErr
}

func (m *mockExecutor) RemoveImage(_ context.Context, _, _ string) error {
	m.removes++
	return nil
}

func newMockExecutor(result *domainfunctions.ExecutionResult, err error) *mockExecutor {
	return &mockExecutor{result: result, err: err}
}

func TestSanitizeEnv(t *testing.T) {
	env := map[string]string{
		"OK":     "fine",
		"a\nb":   "newline in key",
		"a\rb":   "carriage return in key",
		"a\x00b": "nul in key",
		"SPACE":  "a b",
		"EMPTY":  "",
	}
	got := sanitizeEnv(env)
	require.Equal(t, map[string]string{"OK": "fine", "SPACE": "a b", "EMPTY": ""}, got)
}

func TestRuntimeImage(t *testing.T) {
	cfg := &config.AppConfig{}
	uc := NewFunctions(cfg, newMockExecutor(nil, nil), nil, nil)
	require.Equal(t, "torchwood-funcs/runtime-node-18.0:latest", uc.RuntimeImage("node-18.0"), "default registry is torchwood-funcs")

	cfg.Functions = &config.Functions{
		Docker: &config.Functions_Docker{Registry: "ghcr.io/torchwood"},
	}
	require.Equal(t, "ghcr.io/torchwood/runtime-node-18.0:latest", uc.RuntimeImage("node-18.0"))
}
