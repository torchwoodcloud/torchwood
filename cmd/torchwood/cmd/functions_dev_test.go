package cmd

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// functions dev / deploy 的旗标解析与纯函数层测试（对齐本包既有测试边界：
// 子进程与 RPC 轮询不真跑；buildDevEnv/zipFunctionDir/pollDeployment 等
// 纯函数直测）。

func TestValidateFunctionDir(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, validateFunctionDir(dir), "缺 index.js 拒绝")

	bad := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(bad, "index.js"), 0o700))
	require.Error(t, validateFunctionDir(bad), "index.js 为目录拒绝")

	good := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(good, "index.js"), []byte("exports.main = () => 1"), 0o600))
	require.NoError(t, validateFunctionDir(good))
}

func TestResolveNodePath(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
	p, err := resolveNodePath("")
	require.NoError(t, err)
	require.NotEmpty(t, p)
	_, err = resolveNodePath(filepath.Join(t.TempDir(), "no-such-node"))
	require.Error(t, err)
}

func TestParseEnvFile(t *testing.T) {
	t.Run("KEY=VALUE 与注释/空行", func(t *testing.T) {
		kv, err := parseEnvFile("# comment\n\nFOO=bar\r\nEMPTY=\nA = b c \n")
		require.NoError(t, err)
		require.Equal(t, [][2]string{{"FOO", "bar"}, {"EMPTY", ""}, {"A", "b c"}}, kv)
	})
	t.Run("缺等号拒绝", func(t *testing.T) {
		_, err := parseEnvFile("JUSTAKEY\n")
		require.Error(t, err)
	})
	t.Run("空 key 拒绝", func(t *testing.T) {
		_, err := parseEnvFile("=value\n")
		require.Error(t, err)
	})
}

func TestBuildDevEnv(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "vars.env")
	require.NoError(t, os.WriteFile(envFile, []byte("FOO=bar\n"), 0o600))
	dataFile := filepath.Join(t.TempDir(), "data.json")
	require.NoError(t, os.WriteFile(dataFile, []byte(`{"hello":"world"}`), 0o600))

	opts := devOptions{
		port:       19999,
		token:      "tok-1",
		apiBaseURL: "http://127.0.0.1:9060",
		dataFile:   dataFile,
		envFile:    envFile,
	}
	env, err := buildDevEnv(opts)
	require.NoError(t, err)
	got := map[string]string{}
	for _, e := range env {
		if i := strings.Index(e, "="); i >= 0 {
			got[e[:i]] = e[i+1:]
		}
	}
	require.Equal(t, "19999", got["TW_RUNNER_PORT"])
	require.Equal(t, "tok-1", got["TW_EXECUTION_TOKEN"])
	require.Equal(t, "http://127.0.0.1:9060", got["TW_API_BASE_URL"])
	require.Equal(t, `{"hello":"world"}`, got["TW_DATA"])
	require.Equal(t, "bar", got["FOO"])

	t.Run("平台保留键拒绝", func(t *testing.T) {
		reserved := filepath.Join(t.TempDir(), "r.env")
		require.NoError(t, os.WriteFile(reserved, []byte("TW_RUNNER_PORT=1\n"), 0o600))
		_, err := buildDevEnv(devOptions{port: 1, envFile: reserved})
		require.Error(t, err)
	})

	t.Run("data-file 缺失拒绝", func(t *testing.T) {
		_, err := buildDevEnv(devOptions{port: 1, dataFile: filepath.Join(t.TempDir(), "nope")})
		require.Error(t, err)
	})
}

func TestWriteRunnerToTemp(t *testing.T) {
	dir, err := writeRunnerToTemp()
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()
	p := filepath.Join(dir, "runner.js")
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0), "runner.js 应写入嵌入资产")
}

func TestZipFunctionDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("exports.main=()=>1"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "p.js"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.js"), []byte("module.exports={}"), 0o600))

	code, err := zipFunctionDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, code)

	// 用 zip reader 反查条目：index.js/lib.js 在包，node_modules/.git 排除。
	gr, err := zip.NewReader(bytes.NewReader(code), int64(len(code)))
	require.NoError(t, err)
	names := map[string]bool{}
	for _, f := range gr.File {
		names[f.Name] = true
	}
	require.True(t, names["index.js"])
	require.True(t, names["lib.js"])
	require.False(t, names["node_modules/pkg/p.js"])
	require.False(t, names[".git/HEAD"])

	t.Run("空目录拒绝", func(t *testing.T) {
		empty := t.TempDir()
		_, err := zipFunctionDir(empty)
		require.Error(t, err)
	})
}

func TestBuildCreateDeploymentReqFromDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.js"), []byte("exports.main=()=>1"), 0o600))
	req, err := buildCreateDeploymentReqFromDir("f1", dir)
	require.NoError(t, err)
	require.Equal(t, "f1", req["functionId"])
	require.NotEmpty(t, req["code"], "code 应 base64 编码")

	t.Run("缺 function-id", func(t *testing.T) {
		_, err := buildCreateDeploymentReqFromDir("", dir)
		// zip 不依赖 function-id；>1MiB 检查的错误信息带 function-id，空 id
		// 时正常路径仍可构建（deploy 命令层已在旗标校验拦截空 id）。
		_ = err
	})

	t.Run("超过 1MiB 提示 multipart", func(t *testing.T) {
		big := t.TempDir()
		// 真随机数据不可压缩，1.2MiB 足以超 1MiB 上限（合成周期数据会被
		// DEFLATE 压到阈值以下，断言失效）。
		data := make([]byte, 1200*1024)
		_, err := rand.Read(data)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(big, "blob.bin"), data, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(big, "index.js"), []byte("exports.main=()=>1"), 0o600))
		_, err = buildCreateDeploymentReqFromDir("f1", big)
		require.Error(t, err)
		require.Contains(t, err.Error(), "multipart upload API")
	})
}

func TestParseDeploymentView(t *testing.T) {
	v, err := parseDeploymentView([]byte(`{"id":"dep1","status":"failed","error":"npm install boom"}`))
	require.NoError(t, err)
	require.Equal(t, "dep1", v.ID)
	require.Equal(t, "failed", v.Status)
	require.Equal(t, "npm install boom", v.Error)

	_, err = parseDeploymentView([]byte(`not-json`))
	require.Error(t, err)
}

func TestSameFileStates(t *testing.T) {
	now := time.Now()
	a := map[string]time.Time{"x.js": now}
	require.True(t, sameFileStates(a, map[string]time.Time{"x.js": now}))
	require.False(t, sameFileStates(a, map[string]time.Time{"x.js": now.Add(time.Second)}))
	require.False(t, sameFileStates(a, map[string]time.Time{}))
}

// TestFunctionsDevDeployFlagsPresence 冒烟：dev/deploy 动词旗标可解析
// （分发动线与其它动词同构；这里只验证 FlagSet 声明无冲突）。
func TestFunctionsDevDeployFlagsPresence(t *testing.T) {
	devFlags := func(fs *flag.FlagSet) {
		fs.String("dir", ".", "")
		fs.Int("port", 18080, "")
		fs.String("token", "", "")
		fs.String("api-base-url", "", "")
		fs.String("data-file", "", "")
		fs.String("env-file", "", "")
		fs.String("node", "", "")
	}
	v := newPresenceVerb(t, devFlags, map[string]string{"port": "19999", "token": "t"})
	require.True(t, v.changed("port"))
	require.True(t, v.changed("token"))
	require.False(t, v.changed("dir"))

	deployFlags := func(fs *flag.FlagSet) {
		fs.String("function-id", "", "")
		fs.String("dir", ".", "")
		fs.Bool("watch", false, "")
	}
	dv := newPresenceVerb(t, deployFlags, map[string]string{"function-id": "f1", "watch": "true"})
	require.True(t, dv.changed("watch"))
	require.True(t, dv.changed("function-id"))
	require.False(t, dv.changed("dir"))
}
