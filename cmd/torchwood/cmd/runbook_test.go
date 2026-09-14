package cmd

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynx-go/commands"
	"github.com/stretchr/testify/require"
)

func TestRunbookMaxVersion(t *testing.T) {
	t.Run("目录不存在返回 0", func(t *testing.T) {
		v, err := runbookMaxVersion(filepath.Join(t.TempDir(), "missing"))
		require.NoError(t, err)
		require.Zero(t, v)
	})

	t.Run("空目录返回 0", func(t *testing.T) {
		v, err := runbookMaxVersion(t.TempDir())
		require.NoError(t, err)
		require.Zero(t, v)
	})

	t.Run("取最大序号且不校验链（D17：new 只看最大值）", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml": "up: []\n",
			"000005_e.yaml": "up: []\n",
		})
		v, err := runbookMaxVersion(dir)
		require.NoError(t, err)
		require.Equal(t, int64(5), v)
	})

	t.Run("忽略非法文件名与子目录", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml":   "up: []\n",
			"000002_b.yml":    "up: []\n", // 仅认 .yaml，不计入
			"0003_short.yaml": "up: []\n", // 非 6 位，不计入
			"README.md":       "notes",
		})
		require.NoError(t, os.Mkdir(filepath.Join(dir, "000009_sub.yaml"), 0o750))
		v, err := runbookMaxVersion(dir)
		require.NoError(t, err)
		require.Equal(t, int64(1), v)
	})
}

func TestWriteRunbookSkeleton(t *testing.T) {
	t.Run("空目录引导：创建目录并从 000001 起（D17）", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "runbooks")
		path, version, err := writeRunbookSkeleton(dir, "hello")
		require.NoError(t, err)
		require.Equal(t, int64(1), version)
		require.Equal(t, filepath.Join(dir, "000001_hello.yaml"), path)
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(content), "up: []")
		require.Contains(t, string(content), "down: []")
		require.Contains(t, string(content), "IRREVERSIBLE")
		require.Contains(t, string(content), "create_collection")
	})

	t.Run("序号递增", func(t *testing.T) {
		dir := t.TempDir()
		_, v1, err := writeRunbookSkeleton(dir, "first")
		require.NoError(t, err)
		path2, v2, err := writeRunbookSkeleton(dir, "second")
		require.NoError(t, err)
		require.Equal(t, int64(1), v1)
		require.Equal(t, int64(2), v2)
		require.Equal(t, filepath.Join(dir, "000002_second.yaml"), path2)
	})

	t.Run("骨架可通过严格加载（自洽）", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := writeRunbookSkeleton(dir, "a")
		require.NoError(t, err)
		_, _, err = writeRunbookSkeleton(dir, "b")
		require.NoError(t, err)
		files, err := loadRunbookDir(dir)
		require.NoError(t, err)
		require.Len(t, files, 2)
		require.Equal(t, int64(1), files[0].Version)
		require.Equal(t, int64(2), files[1].Version)
		require.Empty(t, files[0].Up)
		require.Empty(t, files[0].Down)
		require.NotEqual(t, files[0].Checksum, files[1].Checksum)
	})

	t.Run("断链目录仍取最大值 +1", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml": "up: []\n",
			"000005_e.yaml": "up: []\n",
		})
		_, version, err := writeRunbookSkeleton(dir, "next")
		require.NoError(t, err)
		require.Equal(t, int64(6), version)
	})

	t.Run("非法 name 报错", func(t *testing.T) {
		for _, name := range []string{"", "Hello", "has-dash", "has space", strings.Repeat("a", 65)} {
			_, _, err := writeRunbookSkeleton(t.TempDir(), name)
			require.Error(t, err, name)
			require.Contains(t, err.Error(), "invalid runbook name", name)
		}
	})

	t.Run("999999 封顶后报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{"999999_last.yaml": "up: []\n"})
		_, _, err := writeRunbookSkeleton(dir, "overflow")
		require.Error(t, err)
		require.Contains(t, err.Error(), "999999")
	})
}

func TestRunbookNewCmd(t *testing.T) {
	isolateConfig(t)

	t.Run("注册形态：runbook 分组下可见 new", func(t *testing.T) {
		grp := newRunbookCmd(&globalFlags{})
		_, found := grp.sub.Lookup("new")
		require.True(t, found, "torchwood runbook new 应已注册")
	})

	t.Run("new 是免 key 动词且落盘成功、输出 JSON", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "runbooks")
		v := newRunbookNewCmd(&globalFlags{timeout: "30s", output: "json"})
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
		v := newRunbookNewCmd(&globalFlags{timeout: "30s", output: "json"})
		fs := flag.NewFlagSet(v.Name(), flag.ContinueOnError)
		v.SetFlags(fs)
		require.NoError(t, fs.Parse(nil))
		err := v.Run(context.Background(), &commands.Environment{}, fs.Args())
		require.Error(t, err)
		var usage *commands.UsageError
		require.ErrorAs(t, err, &usage)
	})
}
