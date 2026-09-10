package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

type mockExecutor struct {
	calls    []domainfunctions.Execution
	result   *domainfunctions.ExecutionResult
	err      error
	builds   int
	buildErr error
	removes  int
}

func (m *mockExecutor) Execute(_ context.Context, exec domainfunctions.Execution) (*domainfunctions.ExecutionResult, error) {
	m.calls = append(m.calls, exec)
	return m.result, m.err
}

func (m *mockExecutor) Build(_ context.Context, _, _, _ string) error {
	m.builds++
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
