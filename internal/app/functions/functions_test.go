package functions

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

type mockExecutor struct {
	// mu 保护可变计数与切片：镜像缺失重建在后台 goroutine 调 Build/
	// ImportImage，与测试断言（require.Eventually 轮询）并发。
	mu       sync.Mutex
	calls    []domainfunctions.Execution
	result   *domainfunctions.ExecutionResult
	err      error
	builds   int
	buildErr error
	removes  int
	// builds 是 Build 载荷断言面：specs 收集每次 BuildSpec（构建链一期
	// 定稿载荷，deployments 构建测试用）。
	specs []domainfunctions.BuildSpec
	// buildNodeID 是 Build 返回的构建亲和节点 ID（四期 4a-1：断言
	// dep.BuildNode 落库链路；空 = 旧形态无节点语义）。
	buildNodeID string
	// buildFn 非空时接管 Build 行为（ctx 形态/超时预算断言、可编程返回）。
	buildFn func(ctx context.Context, spec domainfunctions.BuildSpec) error
	// ——镜像导入（三期阶段 1）——imports 计数、importSpecs 收集每次
	// ImportImageSpec（digest 钉死/凭证/预期 digest 断言面）；importDigest
	// 非空时作为钉死 digest 返回（空则回落默认样例），importErr 可编程失败；
	// importFn 非空时接管 ImportImage 行为（首次成功/复检失败等分调用次序
	// 的可编程场景）。
	imports      int
	importSpecs  []domainfunctions.ImportImageSpec
	importDigest string
	importErr    error
	importFn     func(spec domainfunctions.ImportImageSpec) (string, error)
}

func (m *mockExecutor) Execute(_ context.Context, exec domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.mu.Lock()
	m.calls = append(m.calls, exec)
	m.mu.Unlock()
	return m.result, m.err
}

func (m *mockExecutor) Build(ctx context.Context, spec domainfunctions.BuildSpec) (string, error) {
	m.mu.Lock()
	m.specs = append(m.specs, spec)
	m.builds++
	m.mu.Unlock()
	if m.buildFn != nil {
		return m.buildNodeID, m.buildFn(ctx, spec)
	}
	return m.buildNodeID, m.buildErr
}

func (m *mockExecutor) ImportImage(_ context.Context, spec domainfunctions.ImportImageSpec) (string, error) {
	m.mu.Lock()
	m.importSpecs = append(m.importSpecs, spec)
	m.imports++
	m.mu.Unlock()
	if m.importFn != nil {
		return m.importFn(spec)
	}
	if m.importErr != nil {
		return "", m.importErr
	}
	if m.importDigest != "" {
		return m.importDigest, nil
	}
	return "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", nil
}

func (m *mockExecutor) RemoveImage(_ context.Context, _, _ string) error {
	m.mu.Lock()
	m.removes++
	m.mu.Unlock()
	return nil
}

// buildCount / importCount 是并发安全读取面：镜像缺失重建的计数由后台
// goroutine 写入，断言（require.Eventually 轮询）必须经此读取。
func (m *mockExecutor) buildCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.builds
}

func (m *mockExecutor) importCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.imports
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
