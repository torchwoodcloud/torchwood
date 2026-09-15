package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/pkg/runbook"
)

func TestRunbookNewCmd(t *testing.T) {
	isolateConfig(t)

	t.Run("注册形态：runbook 分组下可见 new", func(t *testing.T) {
		grp := newRunbookCmd(&GlobalFlags{})
		_, found := grp.sub.Lookup("new")
		require.True(t, found, "torchwood runbook new 应已注册")
	})

	t.Run("new 是免 key 动词且落盘成功、输出 JSON", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "runbooks")
		v := newRunbookNewCmd(&GlobalFlags{timeout: "30s", output: "json"})
		require.True(t, v.noKey, "runbook new 应豁免 api-key（noKey 机制）")

		fs := flag.NewFlagSet(v.Name(), flag.ContinueOnError)
		v.SetFlags(fs)
		require.NoError(t, fs.Parse([]string{"--dir", dir, "hello"}))

		var out strings.Builder
		env := &commands.Environment{Stdout: &out, Stderr: &out}
		require.NoError(t, v.Run(context.Background(), env, fs.Args()))

		var res struct {
			File    string `json:"file"`
			Version int64  `json:"version"`
		}
		require.NoError(t, decodeJSON(out.String(), &res))
		require.Equal(t, int64(1), res.Version)
		require.Equal(t, filepath.Join(dir, "000001_hello.yaml"), res.File)
		require.FileExists(t, res.File)
	})

	t.Run("位置参数个数校验", func(t *testing.T) {
		v := newRunbookNewCmd(&GlobalFlags{timeout: "30s", output: "json"})
		fs := flag.NewFlagSet(v.Name(), flag.ContinueOnError)
		v.SetFlags(fs)
		require.NoError(t, fs.Parse(nil))
		err := v.Run(context.Background(), &commands.Environment{}, fs.Args())
		require.Error(t, err)
		var usage *commands.UsageError
		require.ErrorAs(t, err, &usage)
	})
}

// ---------------------------------------------------------------------------
// 命令注册形态（引擎本体测试在 internal/pkg/runbook；这里只守卫 CLI 面）
// ---------------------------------------------------------------------------

func TestRunbookEngineCmdRegistration(t *testing.T) {
	grp := newRunbookCmd(&GlobalFlags{})
	for _, name := range []string{"new", "up", "down", "status", "forgive"} {
		_, found := grp.sub.Lookup(name)
		require.True(t, found, "torchwood runbook %s 应已注册", name)
	}
}

// TestRunbookStatusEmptyDirE2E：D13 实证——空目录走命令面（verb.Run 全链路）
// 必须报「无合法迁移文件」错误（加载在首个 RPC 之前，无需真实服务端）。
func TestRunbookStatusEmptyDirE2E(t *testing.T) {
	isolateConfig(t)
	emptyDir := filepath.Join(t.TempDir(), "rbtest2")
	require.NoError(t, os.MkdirAll(emptyDir, 0o750))

	v := newRunbookStatusCmd(&GlobalFlags{timeout: "30s", output: "json", apiKey: "dummy"})
	fs := flag.NewFlagSet(v.Name(), flag.ContinueOnError)
	v.SetFlags(fs)
	require.NoError(t, fs.Parse([]string{"--dir", emptyDir}))

	err := v.Run(context.Background(), &commands.Environment{Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}, fs.Args())
	require.Error(t, err)
	require.Contains(t, err.Error(), "contains no migration files")
}

// TestRunbookUpCmdUsageErrorSurface：D14 越界错误经命令层转 UsageError。
func TestRunbookUpCmdUsageErrorSurface(t *testing.T) {
	isolateConfig(t)
	v := newRunbookUpCmd(&GlobalFlags{timeout: "30s", output: "json"})
	err := wrapRunbookUsageError(v, &runbook.UsageError{Err: errors.New("boom")})
	var ue *commands.UsageError
	require.ErrorAs(t, err, &ue)
}
