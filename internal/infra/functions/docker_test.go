package functions

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/packer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReadBuildOutput_SuccessStream(t *testing.T) {
	stream := "{\"stream\":\"Step 1/2 : FROM node:18-alpine\\n\"}\n" +
		"{\"stream\":\"Successfully built abc123\\n\"}\n"
	log, err := readBuildOutput(strings.NewReader(stream))
	require.NoError(t, err)
	require.Contains(t, log, "Step 1/2")
	require.Contains(t, log, "Successfully built")
}

func TestReadBuildOutput_ErrorJSON(t *testing.T) {
	stream := "{\"stream\":\"Step 2/2 : RUN nope\\n\"}\n" +
		"{\"errorDetail\":{\"message\":\"The command '/bin/sh -c nope' returned a non-zero code: 127\"},\"error\":\"The command '/bin/sh -c nope' returned a non-zero code: 127\"}\n"
	log, err := readBuildOutput(strings.NewReader(stream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-zero code")
	require.Contains(t, log, "The command '/bin/sh -c nope'")
}

func TestReadBuildOutput_ErrorDetailOnly(t *testing.T) {
	stream := "{\"errorDetail\":{\"message\":\"failed to solve: no matching manifest\"}}\n"
	_, err := readBuildOutput(strings.NewReader(stream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to solve")
}

func TestReadBuildOutput_LongStreamKeepsErrorTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < (maxBuildLogBytes/16)+100; i++ {
		sb.WriteString("{\"stream\":\"some build progress line content\\n\"}\n")
	}
	sb.WriteString("{\"error\":\"the final failure\"}\n")
	log, err := readBuildOutput(strings.NewReader(sb.String()))
	require.Error(t, err)
	require.Contains(t, err.Error(), "the final failure")
	require.Len(t, log, maxBuildLogBytes, "日志保留尾部 64KB")
	require.True(t, strings.HasSuffix(log, "{\"error\":\"the final failure\"}\n"), "错误行位于日志末尾")
}

func TestReadBuildOutput_PlainTextNoError(t *testing.T) {
	log, err := readBuildOutput(strings.NewReader("plain text line, not json\n"))
	require.NoError(t, err)
	require.Contains(t, log, "plain text line")
}

func TestTailBuffer_KeepsTail(t *testing.T) {
	var b tailBuffer
	for i := 0; i < maxBuildLogBytes/1024+1; i++ {
		_, _ = b.Write([]byte(strings.Repeat(string(rune('a'+i%26)), 1024)))
	}
	_, _ = b.Write([]byte(strings.Repeat("z", 1024)))
	got := b.String()
	require.Len(t, got, maxBuildLogBytes)
	require.True(t, strings.HasSuffix(got, strings.Repeat("z", 1024)))
	require.False(t, strings.HasPrefix(got, strings.Repeat("a", 1024)), "头部已被丢弃")
}

// partialWriter 每次只落盘前 3 字节（模拟底层 writer 部分写入）。
type partialWriter struct{}

func (partialWriter) Write(p []byte) (int, error) {
	if len(p) > 3 {
		return 3, nil
	}
	return len(p), nil
}

// TestBudgetWriter_EnforcesActualByteBudget 按实际写入字节计数（G6-1/R07-P1-1）：
// 预算精确到字节、超限整段拒绝、计数跟随底层实际写入（不信任声明大小）。
func TestBudgetWriter_EnforcesActualByteBudget(t *testing.T) {
	var buf bytes.Buffer
	w := &budgetWriter{dst: &buf, limit: 100}
	n, err := w.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Equal(t, 10, n)
	n, err = w.Write(make([]byte, 90))
	require.NoError(t, err)
	require.Equal(t, 90, n)
	require.Equal(t, int64(100), w.written, "恰好打满预算")

	_, err = w.Write([]byte("x"))
	require.ErrorIs(t, err, errZipBudgetExceeded)
	require.Equal(t, 100, buf.Len(), "超限字节不得写入目标 writer")

	// 底层部分写入时，计数按实际写入字节而非请求字节。
	bw := &budgetWriter{dst: partialWriter{}, limit: 10}
	_, err = bw.Write(make([]byte, 5))
	require.NoError(t, err)
	require.Equal(t, int64(3), bw.written, "计数跟随底层实际写入")
}

// le16/le32 小端序列化（手工构造 zip 用）；掩码截断为有意为之。
func le16(v uint16) []byte {
	return []byte{byte(v & 0xFF), byte((v >> 8) & 0xFF)}
}

func le32(v uint32) []byte {
	return []byte{byte(v & 0xFF), byte((v >> 8) & 0xFF), byte((v >> 16) & 0xFF), byte((v >> 24) & 0xFF)}
}

// craftLyingZip 手工构造"声明 200 字节、实际数据区仅 10 字节"的伪造 zip
// （stored 条目 index.js；声明大小与实际内容不符，模拟 zip bomb 的声明侧欺骗）。
func craftLyingZip() []byte {
	const name = "index.js"
	var buf bytes.Buffer
	// local file header（30 + nameLen）。
	buf.Write(le32(0x04034b50))
	buf.Write(le16(20))  // version needed
	buf.Write(le16(0))   // flags
	buf.Write(le16(0))   // method: stored
	buf.Write(le16(0))   // mod time
	buf.Write(le16(0))   // mod date
	buf.Write(le32(0))   // crc32（读取端在大小不符时先报错，不校验 CRC）
	buf.Write(le32(200)) // compressed size（声明）
	buf.Write(le32(200)) // uncompressed size（声明）
	buf.Write(le16(uint16(len(name))))
	buf.Write(le16(0)) // extra len
	buf.Write([]byte(name))
	// 实际数据区只有 10 字节（声明 200，严重不符）。
	buf.Write([]byte("0123456789"))
	// central directory（46 + nameLen）。
	cdOffset := buf.Len()
	buf.Write(le32(0x02014b50))
	buf.Write(le16(20)) // version made by
	buf.Write(le16(20)) // version needed
	buf.Write(le16(0))  // flags
	buf.Write(le16(0))  // method
	buf.Write(le16(0))  // mod time
	buf.Write(le16(0))  // mod date
	buf.Write(le32(0))  // crc32
	buf.Write(le32(200))
	buf.Write(le32(200))
	buf.Write(le16(uint16(len(name))))
	buf.Write(le16(0)) // extra len
	buf.Write(le16(0)) // comment len
	buf.Write(le16(0)) // disk number start
	buf.Write(le16(0)) // internal attrs
	buf.Write(le32(0)) // external attrs
	buf.Write(le32(0)) // local header offset
	buf.Write([]byte(name))
	// end of central directory（22）。
	cdSize := buf.Len() - cdOffset
	buf.Write(le32(0x06054b50))
	buf.Write(le16(0))                        // disk number
	buf.Write(le16(0))                        // cd start disk
	buf.Write(le16(1))                        // entries on disk
	buf.Write(le16(1))                        // total entries
	buf.Write(le32(uint32(uint64(cdSize))))   // #nosec G115 -- 测试构造值远小于 2^32
	buf.Write(le32(uint32(uint64(cdOffset)))) // #nosec G115 -- 测试构造值远小于 2^32
	buf.Write(le16(0))                        // comment len
	return buf.Bytes()
}

// TestExtractZip_LyingDeclaredSize_ErrorsAndCleansPartial 声明 200 字节但实际
// 数据区只有 10 字节的伪造 zip：解压中途报错（实际写入与声明不符），且
// 整个解压目标目录必须被清理（G6-1/R07-P1-1 + G11-3 不留半成品要求）。
func TestExtractZip_LyingDeclaredSize_ErrorsAndCleansPartial(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "lying.zip")
	require.NoError(t, os.WriteFile(zipPath, craftLyingZip(), 0o600))
	destDir := filepath.Join(t.TempDir(), "out")

	_, err := extractZipWithLimits(zipPath, destDir, zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 1 << 20,
	})
	require.Error(t, err, "声明与实际内容不符必须报错")

	requireExtractDirCleared(t, destDir)
}

// requireExtractDirCleared 断言解压目标目录被完整清理（目录本身已不存在）。
func requireExtractDirCleared(t *testing.T, destDir string) {
	t.Helper()
	_, err := os.Stat(destDir)
	require.Error(t, err, "解压目标目录必须被整体清理")
	require.True(t, os.IsNotExist(err), "目录不应残留（期望已删除）")
}

// TestExtractZip_TotalBudgetExceeded_CleansWholeDir（G11-3）：前序条目已成功
// 解压后，后续条目声明大小使总预算超限 → 报错且整个目标目录被清理，
// 已解压的前序条目不得残留。
func TestExtractZip_TotalBudgetExceeded_CleansWholeDir(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f1, err := zw.Create("pre.js")
	require.NoError(t, err)
	_, err = f1.Write([]byte("exports.hook = () => {};"))
	require.NoError(t, err)
	f2, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f2.Write(bytes.Repeat([]byte("b"), 100))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "big.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))
	destDir := filepath.Join(t.TempDir(), "out")

	_, err = extractZipWithLimits(zipPath, destDir, zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 50,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "total uncompressed size exceeds")

	requireExtractDirCleared(t, destDir)
}

// TestExtractZip_EntryBudgetExceeded_CleansWholeDir（G11-3）：前序条目已成功
// 解压后，后续条目声明大小超过单条目预算 → 报错且整个目标目录被清理。
func TestExtractZip_EntryBudgetExceeded_CleansWholeDir(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f1, err := zw.Create("pre.js")
	require.NoError(t, err)
	_, err = f1.Write([]byte("exports.hook = () => {};"))
	require.NoError(t, err)
	f2, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f2.Write(bytes.Repeat([]byte("c"), 100))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "big-entry.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))
	destDir := filepath.Join(t.TempDir(), "out")

	_, err = extractZipWithLimits(zipPath, destDir, zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 50,
		maxTotalBytes: 1 << 20,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, `exceeds 50 bytes`)

	requireExtractDirCleared(t, destDir)
}

// TestExtractZipWithLimits_DeclaredOverBudgetRejected 声明大小超过注入预算时
// 快速拒绝（声明侧预检 + 预算参数注入生效）。
func TestExtractZipWithLimits_DeclaredOverBudgetRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f.Write(bytes.Repeat([]byte("a"), 200))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "big.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

	_, err = extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 100,
		maxTotalBytes: 1 << 20,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "exceeds 100 bytes")
}

// TestExtractZipWithLimits_ValidZipWithinBudget 正常 zip 在注入预算内解压成功。
func TestExtractZipWithLimits_ValidZipWithinBudget(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f.Write([]byte("exports.main = () => ({ ok: true });"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "ok.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

	contents, err := extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 1 << 20,
	})
	require.NoError(t, err)
	require.Equal(t, "node", contents.Family)
	require.False(t, contents.NodeDeps)
	require.False(t, contents.HasLockfile)
}

// TestReadBuildOutput_LongLineWithinLimit 单行超过旧 512KB 上限（< 4MB）不再
// 丢日志（G6-7/R07-P2-6）。
func TestReadBuildOutput_LongLineWithinLimit(t *testing.T) {
	line := `{"stream":"` + strings.Repeat("a", 600*1024) + `"}` + "\n"
	log, err := readBuildOutput(strings.NewReader(line))
	require.NoError(t, err)
	require.True(t, strings.Contains(log, strings.Repeat("a", 1024)), "长行日志保留")
}

// TestReadBuildOutput_LineOverMaxFails 超过 4MB 的单行明确报错（防内存耗尽）。
func TestReadBuildOutput_LineOverMaxFails(t *testing.T) {
	line := strings.Repeat("a", maxBuildLogLine+1) + "\n"
	_, err := readBuildOutput(strings.NewReader(line))
	require.Error(t, err)
	require.ErrorIs(t, err, bufio.ErrTooLong)
}

// TestExtractZip_RejectsSymlink 符号链接条目拒绝（补 G6-1 同路径覆盖）。
func TestExtractZip_RejectsSymlink(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "link", Method: zip.Store}
	hdr.SetMode(os.ModeSymlink | 0o777)
	f, err := zw.CreateHeader(hdr)
	require.NoError(t, err)
	_, err = f.Write([]byte("index.js"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "symlink.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

	_, err = extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 1 << 20,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "symlink")
}

// ---- 平台代装依赖（v3 §3.1/D11，functions-v3.md）：zip 校验层探测与拒收 ----

// TestExtractZip_RejectsNodeModulesEntry node_modules 文件条目拒收（v3 §3.1）：
// 任意条目路径第一段为 node_modules 即拒绝，错误信息对齐设计文案。
func TestExtractZip_RejectsNodeModulesEntry(t *testing.T) {
	for _, name := range []string{
		"node_modules/ms/index.js",
		"node_modules",           // node_modules 自身（文件条目形态）
		"./node_modules/ms/x.js", // 前导 ./ 归一化后第一段命中
	} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte("exports.x = 1;"))
		require.NoError(t, err)
		f2, err := zw.Create("index.js")
		require.NoError(t, err)
		_, err = f2.Write([]byte("exports.main = () => ({});"))
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		zipPath := filepath.Join(t.TempDir(), "nm.zip")
		require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

		_, err = extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
			maxEntries:    1000,
			maxEntryBytes: 4096,
			maxTotalBytes: 1 << 20,
		})
		require.Error(t, err, "entry %q must be rejected", name)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.ErrorContains(t, err, "请勿在代码包中携带 node_modules")
	}
}

// TestExtractZip_RejectsNodeModulesDirEntry node_modules 目录条目（zip 显式
// 目录记录）同样拒收——目录条目在 IsDir continue 之前判定，不留漏网。
func TestExtractZip_RejectsNodeModulesDirEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_, err := zw.Create("node_modules/")
	require.NoError(t, err)
	f, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f.Write([]byte("exports.main = () => ({});"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "nm-dir.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

	_, err = extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 1 << 20,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "请勿在代码包中携带 node_modules")
}

// TestExtractZip_AllowsNestedNodeModulesDirName 拒收只针对 zip 根第一段：
// 子目录中的同名目录（如测试夹具）不拒（v3 §3.1 按条目路径第一段判定）。
func TestExtractZip_AllowsNestedNodeModulesDirName(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("test/fixtures/node_modules-helper.js")
	require.NoError(t, err)
	_, err = f.Write([]byte("// not a real node_modules entry"))
	require.NoError(t, err)
	f2, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f2.Write([]byte("exports.main = () => ({});"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "nested.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))

	contents, err := extractZipWithLimits(zipPath, filepath.Join(t.TempDir(), "out"), zipExtractLimits{
		maxEntries:    1000,
		maxEntryBytes: 4096,
		maxTotalBytes: 1 << 20,
	})
	require.NoError(t, err)
	require.Equal(t, "node", contents.Family)
}

// TestExtractZip_PackageJSONManifest 探测收集：dependencies 非空 → NodeDeps；
// 仅 devDependencies / 空依赖 → false；package-lock.json 存在 → HasLockfile。
func TestExtractZip_PackageJSONManifest(t *testing.T) {
	cases := []struct {
		name        string
		packageJSON string
		withLock    bool
		wantDeps    bool
	}{
		{"deps non-empty", `{"name":"fn","dependencies":{"ms":"2.1.3"}}`, true, true},
		{"devDependencies only", `{"name":"fn","devDependencies":{"tap":"21.0.0"}}`, false, false},
		{"empty deps", `{"name":"fn","dependencies":{}}`, false, false},
		{"null deps", `{"name":"fn","dependencies":null}`, false, false},
		{"no package.json", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"index.js": "exports.main = () => ({});"}
			if tc.packageJSON != "" {
				files["package.json"] = tc.packageJSON
			}
			if tc.withLock {
				files["package-lock.json"] = `{"name":"fn","lockfileVersion":3,"packages":{}}`
			}
			contents, err := extractZipFromFiles(t, files)
			require.NoError(t, err)
			require.Equal(t, tc.wantDeps, contents.NodeDeps)
			require.Equal(t, tc.withLock, contents.HasLockfile)
		})
	}
}

// TestExtractZip_InvalidPackageJSON 坏 package.json 是明确错误（不静默按
// 无依赖处理），错误信息携带解析错误（v3 §3.1）。
func TestExtractZip_InvalidPackageJSON(t *testing.T) {
	_, err := extractZipFromFiles(t, map[string]string{
		"index.js":     "exports.main = () => ({});",
		"package.json": `{"name":"fn","dependencies":`,
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "invalid package.json")

	// 依赖值非法 JSON 形状（string 而非 object）同样报错。
	_, err = extractZipFromFiles(t, map[string]string{
		"index.js":     "exports.main = () => ({});",
		"package.json": `{"dependencies":"ms"}`,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid package.json")
}

// extractZipFromFiles 测试辅助：files 打进内存 zip 并走 extractZip 全链路。
func extractZipFromFiles(t *testing.T, files map[string]string) (SourceContents, error) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	zipPath := filepath.Join(t.TempDir(), "code.zip")
	require.NoError(t, os.WriteFile(zipPath, buf.Bytes(), 0o600))
	return extractZip(zipPath, filepath.Join(t.TempDir(), "out"))
}

// TestExtractZip_DetectsPythonEntrypoint main.py zip 仍探测为 python-3.11
// （探测保留、报错前移到构建期 runner.DockerfileFor——错误可指认 runtime）。
func TestExtractZip_DetectsPythonEntrypoint(t *testing.T) {
	contents, err := extractZipFromFiles(t, map[string]string{
		"main.py": "def main(data):\n    return {}\n",
	})
	require.NoError(t, err)
	require.Equal(t, "python", contents.Family)
}

// TestExtractZip_MissingEntrypoint 既无 index.js 也无 go.mod / main.py →
// 明确报错。
func TestExtractZip_MissingEntrypoint(t *testing.T) {
	_, err := extractZipFromFiles(t, map[string]string{
		"README.md": "not code",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "missing entrypoint file")
	require.ErrorContains(t, err, "index.js")
	require.NotContains(t, err.Error(), "main.py")
}

// ---- Go 一期探测（设计 functions-runtimes-and-sources.md §1）----

// TestExtractZip_DetectsGoMod go.mod zip 探测为 go-1.26：module 行解析
// （引号与行尾注释形态剥壳）、require 非空（单行与块形态）、go.sum/vendor
// 标记（五期 5b：twmain 保留目录概念已删，携带 twmain/ 目录的用户 zip
// 不再被标记/拒收）。
func TestExtractZip_DetectsGoMod(t *testing.T) {
	contents, err := extractZipFromFiles(t, map[string]string{
		"go.mod":  "module example.com/fn\n\ngo 1.26\n\nrequire github.com/x/y v1.2.3\n",
		"go.sum":  "github.com/x/y v1.2.3 h1:abc=\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	require.NoError(t, err)
	require.Equal(t, "go", contents.Family)
	require.Equal(t, "example.com/fn", contents.GoModulePath)
	require.True(t, contents.GoHasRequires)
	require.True(t, contents.GoHasSum)
	require.False(t, contents.HasVendor)
}

// TestExtractZip_GoModParseForms module 行与 require 判定的形态矩阵：
// 引号 module、行尾注释、require 块、// indirect 注释剥离、纯 stdlib
// （require 缺省 → GoHasRequires=false）。
func TestExtractZip_GoModParseForms(t *testing.T) {
	cases := []struct {
		name         string
		goMod        string
		wantModule   string
		wantRequires bool
	}{
		{
			"quoted module",
			"module \"example.com/quoted\"\n",
			"example.com/quoted", false,
		},
		{
			"trailing comment",
			"module example.com/x // production module\n",
			"example.com/x", false,
		},
		{
			"quoted module with comment",
			"module \"example.com/q2\" // prod\n",
			"example.com/q2", false,
		},
		{
			"require block with indirect comments",
			"module example.com/x\n\nrequire (\n\tgithub.com/x/y v1.2.3 // indirect\n)\n",
			"example.com/x", true,
		},
		{
			"single line require",
			"module example.com/x\n\nrequire github.com/x/y v1.2.3\n",
			"example.com/x", true,
		},
		{
			"stdlib only",
			"module example.com/x\n\ngo 1.26\n",
			"example.com/x", false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contents, err := extractZipFromFiles(t, map[string]string{
				"go.mod":  tc.goMod,
				"main.go": "package main\n",
			})
			require.NoError(t, err)
			require.Equal(t, "go", contents.Family)
			require.Equal(t, tc.wantModule, contents.GoModulePath)
			require.Equal(t, tc.wantRequires, contents.GoHasRequires)
		})
	}
}

// TestExtractZip_GoModInvalid 坏 go.mod（module 行缺失/空路径）是明确错误，
// 不静默按零值处理（五期 5b 起该校验前置坏 go.mod 的失败点：InvalidArgument
// 比 go build 的构建日志更可指认）。合法引号形态由 TestExtractZip_GoModParseForms
// 覆盖，不在此列。
func TestExtractZip_GoModInvalid(t *testing.T) {
	for _, goMod := range []string{
		"go 1.26\n",     // 无 module 行
		"module\n",      // 空路径
		"module \"\"\n", // 引号空路径
	} {
		_, err := extractZipFromFiles(t, map[string]string{
			"go.mod":  goMod,
			"main.go": "package main\n",
		})
		require.Error(t, err, "go.mod %q must be rejected", goMod)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.ErrorContains(t, err, "invalid go.mod")
	}
}

// TestExtractZip_GoModTooLarge go.mod 超 4MiB 读取上限 → 明确报错（对齐
// package.json 探测的防巨型条目口径）。
func TestExtractZip_GoModTooLarge(t *testing.T) {
	_, err := extractZipFromFiles(t, map[string]string{
		"go.mod":  "module example.com/x\n\n// " + strings.Repeat("pad", maxGoModBytes) + "\n",
		"main.go": "package main\n",
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "go.mod exceeds")
}

// TestExtractZip_GoDetectionMarkers vendor/ 标记：任一根下 vendor 前缀条目
// 命中 HasVendor。五期 5b：twmain 保留目录概念已删——用户 zip 携带 twmain/
// 目录不再被标记/拒收（与平台无关的普通用户目录）。
func TestExtractZip_GoDetectionMarkers(t *testing.T) {
	// vendor/ 目录（zip 显式目录条目 + 目录下文件条目均命中）。
	contents, err := extractZipFromFiles(t, map[string]string{
		"go.mod":                     "module example.com/x\n\nrequire github.com/x/y v1.2.3\n",
		"vendor/":                    "",
		"vendor/github.com/x/y/y.go": "package y\n",
		"main.go":                    "package main\n",
	})
	require.NoError(t, err)
	require.True(t, contents.HasVendor)

	// twmain/ 目录条目形态：不再是保留目录，照常解压且无任何标记语义。
	contents, err = extractZipFromFiles(t, map[string]string{
		"go.mod":      "module example.com/x\n",
		"twmain/":     "",
		"twmain/x.go": "package main\n",
		"main.go":     "package main\n",
	})
	require.NoError(t, err, "twmain/ 不再是平台保留目录，携带同名目录合法")
	require.False(t, contents.HasVendor)

	// 子目录中的 twmain 同名目录同样无关（判定口径 = 路径第一段，历史上
	// 也只对根 twmain 标记）。
	contents, err = extractZipFromFiles(t, map[string]string{
		"go.mod":                "module example.com/x\n",
		"test/twmain-helper.go": "package test\n",
		"main.go":               "package main\n",
	})
	require.NoError(t, err)
}

// TestExtractZip_PriorityIndexJSOverGoMod 混装探测优先级：index.js > go.mod
// （混装按 node，不报冲突——与「探测即入口」现状一致；Go 字段不投影）。
func TestExtractZip_PriorityIndexJSOverGoMod(t *testing.T) {
	contents, err := extractZipFromFiles(t, map[string]string{
		"index.js": "exports.main = () => ({});",
		"go.mod":   "module example.com/mixed\n",
		"main.go":  "package main\n",
	})
	require.NoError(t, err)
	require.Equal(t, "node", contents.Family)
	require.Empty(t, contents.GoModulePath, "混装按 node：Go 探测字段不投影")
}

// craftEntryZipN 构造 n 条目 zip（根含 index.js 保证探测通过；其余为分散
// 子目录的小文件）。
func craftEntryZipN(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < n-1; i++ {
		f, err := zw.Create(fmt.Sprintf("pkg/dir%d/file%04d.txt", i%37, i))
		require.NoError(t, err)
		_, err = f.Write([]byte("x"))
		require.NoError(t, err)
	}
	f, err := zw.Create("index.js")
	require.NoError(t, err)
	_, err = f.Write([]byte("exports.main = () => ({});"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// writeCraftedZip 把构造的 zip 字节落盘并返回路径。
func writeCraftedZip(t *testing.T, data []byte) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "code.zip")
	require.NoError(t, os.WriteFile(zipPath, data, 0o600))
	return zipPath
}

// TestExtractZipRelaxed_EntryBudgetWidened 二期阶段 3（设计 §2 条目维链条）：
// git 源物化 zip 条目上限对齐 packer 物化口径（5000）——默认预算（1000）
// 拒绝的 1001 条目 zip 经 ExtractZipRelaxed 正常解压探测；5001 条目仍按
// 放宽上限拒绝；单条/总量预算与默认一致（字面断言防漂移）。
func TestExtractZipRelaxed_EntryBudgetWidened(t *testing.T) {
	zip1001 := writeCraftedZip(t, craftEntryZipN(t, 1001))

	// 默认预算击毙 1001 条目（现行 zip 上传通道口径不变）。
	_, err := ExtractZip(zip1001, filepath.Join(t.TempDir(), "out-default"))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "too many entries")

	// 放宽预算放行同一 zip（git 源构建路径）。
	contents, err := ExtractZipRelaxed(zip1001, filepath.Join(t.TempDir(), "out-relaxed"))
	require.NoError(t, err)
	require.Equal(t, "node", contents.Family)

	// 放宽上限 = packer.MaxPackEntries（5000）：5001 条目仍拒绝。
	zip5001 := writeCraftedZip(t, craftEntryZipN(t, packer.MaxPackEntries+1))
	_, err = ExtractZipRelaxed(zip5001, filepath.Join(t.TempDir(), "out-5001"))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, fmt.Sprintf("zip contains too many entries (max %d)", packer.MaxPackEntries))

	// 单条与总量预算维持默认口径（100MiB / 200MiB）。
	require.Equal(t, int64(maxZipEntryBytes), gitPackZipExtractLimits.maxEntryBytes)
	require.Equal(t, int64(maxZipTotalBytes), gitPackZipExtractLimits.maxTotalBytes)
}
