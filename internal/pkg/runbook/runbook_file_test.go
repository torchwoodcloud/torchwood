package runbook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeRunbookDir 在临时目录写入一组文件并返回目录路径（专供 arrange 阶段
// 构造磁盘状态；内容原样落盘，不做归一化）。
func writeRunbookDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

// runbookFixtureValid 是设计文档 §2.1 的示例文件（合法双段，含嵌套
// attributes/indexes 与多动作 down）。集合 ID 字段按 CreateCollectionRequest
// 的 proto 字段名写 `id`（引擎 schema 校验的字段面 = proto 字段集；delete
// 侧 GetCollectionRequest 才是 `collection_id`）。
const runbookFixtureValid = `up:
  - create_collection:
      database_id: app
      id: configs
      name: configs
      document_security: true
      permissions: ['read:users']
      attributes:
        - { key: key, type: string, size: 64, required: true }
        - { key: value, type: json }
      indexes:
        - { id: key, type: unique, attributes: [key] }
down:
  - delete_collection:
      database_id: app
      collection_id: configs
`

func TestParseRunbookFileName(t *testing.T) {
	cases := []struct {
		fileName string
		version  int64
		name     string
		ok       bool
	}{
		{"000001_hello.yaml", 1, "hello", true},
		{"000042_create_configs_collection.yaml", 42, "create_configs_collection", true},
		{"999999_x.yaml", 999999, "x", true},
		{"000001_hello.yml", 0, "", false},                            // 仅认 .yaml 后缀
		{"00001_hello.yaml", 0, "", false},                            // 非 6 位（5 位）
		{"0000001_hello.yaml", 0, "", false},                          // 非 6 位（7 位）
		{"000001_Hello.yaml", 0, "", false},                           // 大写 name
		{"000001_hello.YAML", 0, "", false},                           // 大写后缀
		{"000001-hello.yaml", 0, "", false},                           // 连字符
		{"000001.yaml", 0, "", false},                                 // 缺 name 段
		{"000001_.yaml", 0, "", false},                                // 空 name
		{"hello.yaml", 0, "", false},                                  // 缺序号
		{"000001_he%llo.yaml", 0, "", false},                          // 非法字符
		{"000001_" + strings.Repeat("a", 65) + ".yaml", 0, "", false}, // name 超 64
		{"000001_" + strings.Repeat("a", 64) + ".yaml", 1, strings.Repeat("a", 64), true},
	}
	for _, tc := range cases {
		version, name, ok := parseRunbookFileName(tc.fileName)
		require.Equal(t, tc.ok, ok, tc.fileName)
		if tc.ok {
			require.Equal(t, tc.version, version, tc.fileName)
			require.Equal(t, tc.name, name, tc.fileName)
		}
	}
}

func TestParseRunbookContent(t *testing.T) {
	t.Run("合法双段保留动作与嵌套体", func(t *testing.T) {
		up, down, err := parseRunbookContent([]byte(runbookFixtureValid), "000001_create_configs_collection.yaml")
		require.NoError(t, err)
		require.Len(t, up, 1)
		require.Equal(t, "create_collection", up[0].Verb)
		body := up[0].Body
		require.Equal(t, "app", body["database_id"])
		require.Equal(t, "configs", body["id"])
		require.True(t, body["document_security"].(bool))
		require.Equal(t, []any{"read:users"}, body["permissions"])
		attrs := body["attributes"].([]any)
		require.Len(t, attrs, 2)
		attr := attrs[0].(map[string]any)
		require.Equal(t, "key", attr["key"])
		require.Equal(t, 64, attr["size"])
		require.Equal(t, true, attr["required"])
		idx := body["indexes"].([]any)[0].(map[string]any)
		require.Equal(t, "unique", idx["type"])
		require.Equal(t, []any{"key"}, idx["attributes"])
		require.Len(t, down, 1)
		require.Equal(t, "delete_collection", down[0].Verb)
		require.Equal(t, "configs", down[0].Body["collection_id"])
	})

	t.Run("down 缺失合法（不可逆语义）", func(t *testing.T) {
		up, down, err := parseRunbookContent([]byte("up:\n  - delete_bucket:\n      name: gold\n"), "000001_x.yaml")
		require.NoError(t, err)
		require.Len(t, up, 1)
		require.Empty(t, down)
	})

	t.Run("down 空列表合法", func(t *testing.T) {
		_, down, err := parseRunbookContent([]byte("up: []\ndown: []\n"), "000001_x.yaml")
		require.NoError(t, err)
		require.Empty(t, down)
	})

	t.Run("未知顶层键报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up: []\ndown: []\nsideways: true\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "000001_x.yaml")
		require.Contains(t, err.Error(), "sideways")
	})

	t.Run("动作 0 个动词键报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up:\n  - {}\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no verb key")
	})

	t.Run("null 动作项报错（无动词键）", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up:\n  -\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no verb key")
	})

	t.Run("动作 2 个动词键报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up:\n  - create_collection:\n      collection_id: a\n    delete_collection:\n      collection_id: a\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "2 verb keys")
		require.Contains(t, err.Error(), "create_collection, delete_collection")
	})

	t.Run("动词键 null 值报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up:\n  - create_collection:\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), `verb "create_collection" body is null`)
	})

	t.Run("动词体非 map 报错", func(t *testing.T) {
		for _, body := range []string{"create_collection: oops", "create_collection:\n      - a\n      - b\n"} {
			_, _, err := parseRunbookContent([]byte("up:\n  - "+body), "000001_x.yaml")
			require.Error(t, err, body)
			require.Contains(t, err.Error(), "must be a mapping", body)
		}
	})

	t.Run("嵌套非字符串键报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up:\n  - create_collection:\n      1: bad\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "is not a string")
	})

	t.Run("空文件报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("# 只有注释\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty runbook file")
	})

	t.Run("多 YAML 文档报错", func(t *testing.T) {
		_, _, err := parseRunbookContent([]byte("up: []\n---\ndown: []\n"), "000001_x.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "single YAML document")
	})
}

func TestLoadRunbookDir(t *testing.T) {
	t.Run("合法目录按版本升序加载并计算 checksum", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000002_create_gold_asset.yaml":         "up:\n  - create_asset_def:\n      code: gold\ndown: []\n",
			"000001_create_configs_collection.yaml": runbookFixtureValid,
		})
		files, err := loadRunbookDir(dir)
		require.NoError(t, err)
		require.Len(t, files, 2)
		require.Equal(t, int64(1), files[0].Version)
		require.Equal(t, "create_configs_collection", files[0].Name)
		require.Equal(t, int64(2), files[1].Version)
		require.Equal(t, "create_gold_asset", files[1].Name)
		require.Equal(t, runbookChecksum([]byte(runbookFixtureValid)), files[0].Checksum)
		require.Equal(t, runbookChecksum([]byte("up:\n  - create_asset_def:\n      code: gold\ndown: []\n")), files[1].Checksum)
		require.NotEqual(t, files[0].Checksum, files[1].Checksum)
	})

	t.Run("目录不存在报错（D13）", func(t *testing.T) {
		_, err := loadRunbookDir(filepath.Join(t.TempDir(), "missing"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not exist")
	})

	t.Run("空目录与无合法文件目录报错（D13）", func(t *testing.T) {
		for name, files := range map[string]map[string]string{
			"完全为空":   {},
			"只有无关文件": {"README.md": "notes"},
		} {
			dir := writeRunbookDir(t, files)
			_, err := loadRunbookDir(dir)
			require.Error(t, err, name)
			require.Contains(t, err.Error(), "contains no migration files", name)
		}
	})

	t.Run(".yml 后缀拒收", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{"000001_hello.yml": "up: []\n"})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid runbook file name")
		require.Contains(t, err.Error(), "000001_hello.yml")
	})

	t.Run("非 6 位序号的 .yaml 报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"00001_short.yaml": "up: []\n",
			"000001_ok.yaml":   "up: []\n",
		})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "00001_short.yaml")
	})

	t.Run("大写 name 的 .yaml 报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_Oops.yaml": "up: []\n",
			"000001_ok.yaml":   "up: []\n",
		})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "000001_Oops.yaml")
	})

	t.Run("无关文件忽略、子目录跳过", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_ok.yaml": "up: []\n",
			"README.md":      "notes",
			"notes.txt":      "scratch",
		})
		require.NoError(t, os.Mkdir(filepath.Join(dir, "000002_sub.yaml"), 0o750))
		files, err := loadRunbookDir(dir)
		require.NoError(t, err)
		require.Len(t, files, 1)
		require.Equal(t, int64(1), files[0].Version)
	})

	t.Run("重号报错并带文件名", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml": "up: []\n",
			"000001_b.yaml": "up: []\n",
		})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate runbook version 000001")
		require.Contains(t, err.Error(), "000001_a.yaml")
		require.Contains(t, err.Error(), "000001_b.yaml")
	})

	t.Run("跳号报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml": "up: []\n",
			"000003_c.yaml": "up: []\n",
		})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "broken runbook version chain")
		require.Contains(t, err.Error(), "000003_c.yaml")
		require.Contains(t, err.Error(), "expected version 000002")
	})

	t.Run("不从 1 起报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{"000002_b.yaml": "up: []\n"})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected version 000001")
	})

	t.Run("内容解析错误穿透并带文件名", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_bad.yaml": "up: []\nunknown_key: 1\n",
		})
		_, err := loadRunbookDir(dir)
		require.Error(t, err)
		require.Contains(t, err.Error(), "000001_bad.yaml")
	})
}

func TestRunbookChecksum(t *testing.T) {
	lf := []byte("up:\n  - create_collection:\n      collection_id: a\ndown: []\n")
	crlf := []byte("up:\r\n  - create_collection:\r\n      collection_id: a\r\ndown: []\r\n")
	bom := append([]byte{0xEF, 0xBB, 0xBF}, lf...)
	bomCRLF := append([]byte{0xEF, 0xBB, 0xBF}, crlf...)

	t.Run("CRLF 与 LF 等价（D9）", func(t *testing.T) {
		require.Equal(t, runbookChecksum(lf), runbookChecksum(crlf))
	})

	t.Run("去 BOM 后等价（D9）", func(t *testing.T) {
		require.Equal(t, runbookChecksum(lf), runbookChecksum(bom))
		require.Equal(t, runbookChecksum(lf), runbookChecksum(bomCRLF))
	})

	t.Run("内容不同则不等", func(t *testing.T) {
		require.NotEqual(t, runbookChecksum(lf), runbookChecksum([]byte("up: []\n")))
	})

	t.Run("形状为 64 位小写 hex", func(t *testing.T) {
		sum := runbookChecksum(lf)
		require.Len(t, sum, 64)
		require.Equal(t, sum, strings.ToLower(sum))
	})
}

func TestRunbookMaxVersion(t *testing.T) {
	t.Run("目录不存在返回 0", func(t *testing.T) {
		v, err := RunbookMaxVersion(filepath.Join(t.TempDir(), "missing"))
		require.NoError(t, err)
		require.Zero(t, v)
	})

	t.Run("空目录返回 0", func(t *testing.T) {
		v, err := RunbookMaxVersion(t.TempDir())
		require.NoError(t, err)
		require.Zero(t, v)
	})

	t.Run("取最大序号且不校验链（D17：new 只看最大值）", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{
			"000001_a.yaml": "up: []\n",
			"000005_e.yaml": "up: []\n",
		})
		v, err := RunbookMaxVersion(dir)
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
		v, err := RunbookMaxVersion(dir)
		require.NoError(t, err)
		require.Equal(t, int64(1), v)
	})
}

func TestScaffold(t *testing.T) {
	t.Run("空目录引导：创建目录并从 000001 起（D17）", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "runbooks")
		path, version, err := Scaffold(dir, "hello")
		require.NoError(t, err)
		require.Equal(t, int64(1), version)
		require.Equal(t, filepath.Join(dir, "000001_hello.yaml"), path)
		content, err := os.ReadFile(path) //nolint:gosec // path 为 Scaffold 在 t.TempDir 下生成，非外部输入
		require.NoError(t, err)
		require.Contains(t, string(content), "up: []")
		require.Contains(t, string(content), "down: []")
		require.Contains(t, string(content), "IRREVERSIBLE")
		require.Contains(t, string(content), "create_collection")
	})

	t.Run("序号递增", func(t *testing.T) {
		dir := t.TempDir()
		_, v1, err := Scaffold(dir, "first")
		require.NoError(t, err)
		path2, v2, err := Scaffold(dir, "second")
		require.NoError(t, err)
		require.Equal(t, int64(1), v1)
		require.Equal(t, int64(2), v2)
		require.Equal(t, filepath.Join(dir, "000002_second.yaml"), path2)
	})

	t.Run("骨架可通过严格加载（自洽）", func(t *testing.T) {
		dir := t.TempDir()
		_, _, err := Scaffold(dir, "a")
		require.NoError(t, err)
		_, _, err = Scaffold(dir, "b")
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
		_, version, err := Scaffold(dir, "next")
		require.NoError(t, err)
		require.Equal(t, int64(6), version)
	})

	t.Run("非法 name 报错", func(t *testing.T) {
		for _, name := range []string{"", "Hello", "has-dash", "has space", strings.Repeat("a", 65)} {
			_, _, err := Scaffold(t.TempDir(), name)
			require.Error(t, err, name)
			require.Contains(t, err.Error(), "invalid runbook name", name)
		}
	})

	t.Run("999999 封顶后报错", func(t *testing.T) {
		dir := writeRunbookDir(t, map[string]string{"999999_last.yaml": "up: []\n"})
		_, _, err := Scaffold(dir, "overflow")
		require.Error(t, err)
		require.Contains(t, err.Error(), "999999")
	})
}
