package functions

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件承载函数构建/执行路径的共享 docker 约定（zip 解压校验与依赖探测、
// 镜像名/执行网络名解析、构建日志解析）。v1 docker 执行器（每请求一容器、
// 进程内 docker.sock）已移除——执行统一经 functions-dispatcher 分发
// （DispatcherExecutor），docker.sock 收敛到 dispatcher 进程。

// zip 解压与构建日志限制（§5.4 防 zip 炸弹；构建日志保留尾部 64KB）。
const (
	maxZipEntries    = 1000
	maxZipEntryBytes = 100 << 20 // 单条 ≤ 100 MiB
	maxZipTotalBytes = 200 << 20 // 总解压 ≤ 200 MiB
	maxBuildLogBytes = 64 << 10  // 构建日志截断 64KB
	maxBuildLogLine  = 4 << 20   // 单行构建日志上限（Scanner 缓冲，超长即报错）
	// maxPackageJSONBytes 是 package.json 依赖探测的读取上限（v3 §3.1）。
	// 合法 package.json 远小于此，防御恶意巨型条目撑探测内存。
	maxPackageJSONBytes = 4 << 20
)

// zipExtractLimits 是 extractZip 的解压预算（防 zip 炸弹）。
// 校验分两层：声明侧（UncompressedSize64，快速拒绝）+ 写入侧（按实际写入
// 字节计数，防声明大小与实际内容不符的伪造 zip）。
type zipExtractLimits struct {
	maxEntries    int
	maxEntryBytes int64
	maxTotalBytes int64
}

var defaultZipExtractLimits = zipExtractLimits{
	maxEntries:    maxZipEntries,
	maxEntryBytes: maxZipEntryBytes,
	maxTotalBytes: maxZipTotalBytes,
}

// errZipBudgetExceeded 表示按实际字节计数的解压预算超限（写入侧兜底）。
var errZipBudgetExceeded = errors.New("extracted size exceeds budget")

// budgetWriter 包装目标 writer，按实际写入字节计数；超预算拒绝写入并返回
// errZipBudgetExceeded（调用方负责清理半成品文件）。
type budgetWriter struct {
	dst     io.Writer
	limit   int64
	written int64
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if b.limit-b.written < int64(len(p)) {
		return 0, errZipBudgetExceeded
	}
	n, err := b.dst.Write(p)
	b.written += int64(n)
	return n, err
}

// specResources 是资源规格 → 容器配额映射（与 app 层 runtimes 表一致，
// infra 不依赖 app 包，自持一份兜底）。
var specResources = map[string]struct {
	cpu    float64
	memory int64
}{
	"shared-1x": {cpu: 0.5, memory: 256 << 20},
	"shared-2x": {cpu: 1.0, memory: 512 << 20},
}

// ResourceSpec 是资源规格的配额投影（functions-dispatcher 复用同一映射）。
type ResourceSpec struct {
	Memory   int64
	NanoCPUs int64
}

// SpecResources 把规格名映射为容器配额；未知规格回落 shared-1x。
func SpecResources(spec string) ResourceSpec {
	res := specResources[spec]
	if res.cpu <= 0 {
		res = specResources["shared-1x"]
	}
	return ResourceSpec{Memory: res.memory, NanoCPUs: int64(res.cpu * 1e9)}
}

// perProjectNetworkPrefix 是默认 per-project 函数执行网络的前缀
// （Round4 J5-4）：完整网络名为 tw-func-<project.id>。project.id 已过
// ident 白名单（^[a-z][a-z0-9]{0,27}$），可直接用作网络名后缀。
const perProjectNetworkPrefix = "tw-func-"

// perProjectInternalNetworkSuffix 是 internal 变体网络的后缀（P2 egress
// 默认 deny，设计 Security #6）：tw-func-<project.id>-int，docker
// internal: true——阻断外网出口、网内互通保留（dispatcher/平台回访地址
// 仍可达）。不可信函数（client_callable 或存在 http/cron 触发器）容器
// attach 该网络而非常规网络。
const perProjectInternalNetworkSuffix = "-int"

// ImageName 返回函数部署镜像名：{registry}/func-{functionID}-{deploymentID}
// （registry 取 functions.docker.registry，默认 torchwood-funcs）。
func ImageName(cfg *config.AppConfig, functionID, deploymentID string) string {
	registry := cfg.GetFunctions().GetDocker().GetRegistry()
	if registry == "" {
		registry = "torchwood-funcs"
	}
	// 兜底兼容历史大写 functionID（Docker 镜像仓库/标签名只允许小写，G6-3）。
	return fmt.Sprintf("%s/func-%s-%s", registry, strings.ToLower(functionID), deploymentID)
}

// ResolveNetworkName 解析函数执行容器网络名（Round4 J5-4；导出供
// functions-dispatcher 保持约定）：
//   - 显式配置 functions.docker.network 时使用该全局网络（opt-in；跨项目
//     函数容器同网互通，存在横向访问风险，见 config.yaml.template 警告）；
//   - 未配置（默认）时使用 per-project 网络 tw-func-<project.id>，项目间
//     容器互不可达，实现租户网络隔离。
//
// projectID 为空且未配置全局网络时返回错误（fail-closed，不回落共享网络）。
func ResolveNetworkName(cfg *config.AppConfig, projectID string) (string, error) {
	if name := cfg.GetFunctions().GetDocker().GetNetwork(); name != "" {
		return name, nil
	}
	if projectID == "" {
		return "", status.Error(codes.InvalidArgument, "project id is required for function execution")
	}
	// 纵深防御：即便上游漏校验，也不让非法字符进入 docker 网络名。
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "invalid project id for function execution: %v", err)
	}
	return perProjectNetworkPrefix + projectID, nil
}

// ResolveInternalNetworkName 解析 internal 变体网络名（P2 egress 默认 deny；
// 导出供 functions-dispatcher 保持约定）：常规网络名 + "-int" 后缀
// （tw-func-<project>-int；显式全局网络配置同样加后缀）。
// projectID 校验与 ResolveNetworkName 同源。
func ResolveInternalNetworkName(cfg *config.AppConfig, projectID string) (string, error) {
	base, err := ResolveNetworkName(cfg, projectID)
	if err != nil {
		return "", err
	}
	return base + perProjectInternalNetworkSuffix, nil
}

// ZipContents 是 zip 解压校验的产出：runtime 判定 + 平台代装依赖的探测
// 结果（v3 §3.1，functions-v3.md），供 runner.DockerfileFor 做模板分层与
// lockfile 强制决策。
type ZipContents struct {
	// Runtime 是运行时 ID（node-18.0；python-3.11 仅探测保留，构建期报错）。
	Runtime string
	// NodeDeps 表示 zip 根 package.json 的 dependencies 键非空（探测条件；
	// devDependencies 不触发代装）。
	NodeDeps bool
	// HasLockfile 表示 zip 根含 package-lock.json。
	HasLockfile bool
}

// extractZip 解压 zip 到 destDir（防 zip 炸弹与路径穿越），返回内容探测结果。
func extractZip(zipPath, destDir string) (ZipContents, error) {
	return extractZipWithLimits(zipPath, destDir, defaultZipExtractLimits)
}

// ExtractZip 是 extractZip 的导出版（functions-dispatcher 的构建复用
// 同一防 zip 炸弹/路径穿越预算与依赖探测）。
func ExtractZip(zipPath, destDir string) (ZipContents, error) {
	return extractZip(zipPath, destDir)
}

// extractZipWithLimits 是 extractZip 的可注入预算版本（测试用）：除声明侧
// UncompressedSize64 预检外，写入侧按实际字节计数强制预算，超限清理半成品。
// 同处逐条收集平台代装依赖探测信息（v3 §3.1）：node_modules 拒收、zip 根
// package.json 的 dependencies 非空判定、package-lock.json 存在性。
func extractZipWithLimits(zipPath, destDir string, limits zipExtractLimits) (ZipContents, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return ZipContents{}, status.Error(codes.InvalidArgument, "invalid zip file")
	}
	defer func() { _ = zr.Close() }()

	if len(zr.File) > limits.maxEntries {
		return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip contains too many entries (max %d)", limits.maxEntries)
	}
	// declaredTotal 基于声明大小快速预检（低成本拒绝明显超限）；
	// actualTotal 按实际写入字节累计（防御声明大小与实际不符的伪造 zip）。
	var declaredTotal uint64
	var actualTotal int64
	hasIndexJS := false
	hasMainPy := false
	hasLockfile := false
	nodeDeps := false
	root := filepath.Clean(destDir)

	for _, f := range zr.File {
		if f.Mode()&os.ModeSymlink != 0 {
			return ZipContents{}, status.Error(codes.InvalidArgument, "zip entry is a symlink")
		}
		// node_modules 拒收（v3 §3.1/D11）：用户 zip 携带
		// node_modules 存在跨平台二进制不兼容与包体膨胀问题，依赖改由平台
		// 构建期代装（CLI deploy 侧 B 切片已同步剔除）。判定条目路径第一段
		// （含 node_modules 自身与 node_modules/...，目录与文件条目一并拒绝，
		// 逐条判定即可，无需等解压完成）；子目录中的同名目录不受影响。
		if firstPathSegment(f.Name) == "node_modules" {
			return ZipContents{}, status.Error(codes.InvalidArgument, "请勿在代码包中携带 node_modules——平台将在构建期代装依赖（跨平台二进制不兼容）")
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if f.UncompressedSize64 > uint64(limits.maxEntryBytes) {
			_ = os.RemoveAll(destDir)
			return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip entry %q exceeds %d bytes", f.Name, limits.maxEntryBytes)
		}
		declaredTotal += f.UncompressedSize64
		if declaredTotal > uint64(limits.maxTotalBytes) {
			_ = os.RemoveAll(destDir)
			return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip total uncompressed size exceeds %d bytes", limits.maxTotalBytes)
		}

		// zip slip：Clean 后必须仍位于解压根目录内。
		name := filepath.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip entry %q escapes root", f.Name)
		}
		target := filepath.Join(root, name)
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip entry %q escapes root", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return ZipContents{}, fmt.Errorf("create zip entry dir: %w", err)
		}
		src, err := f.Open()
		if err != nil {
			return ZipContents{}, fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			_ = src.Close()
			return ZipContents{}, fmt.Errorf("write zip entry %q: %w", f.Name, err)
		}
		// 写入侧按实际字节计数（不再仅信任 UncompressedSize64 声明值），
		// 预算（单条目或总预算）超限报错并清理整个解压目标目录——RemoveAll
		// 仅作用于本函数持有的 destDir 子树（zip-slip 校验保证条目写入始终
		// 位于其内），不会误删目录外内容，也不残留已解压的前序条目。
		n, copyErr := io.Copy(&budgetWriter{dst: dst, limit: limits.maxEntryBytes}, src)
		actualTotal += n
		_ = src.Close()
		closeErr := dst.Close()
		if copyErr != nil {
			_ = os.RemoveAll(destDir)
			if errors.Is(copyErr, errZipBudgetExceeded) {
				return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip entry %q exceeds %d byte extraction budget", f.Name, limits.maxEntryBytes)
			}
			return ZipContents{}, fmt.Errorf("extract zip entry %q: %w", f.Name, copyErr)
		}
		if actualTotal > limits.maxTotalBytes {
			_ = os.RemoveAll(destDir)
			return ZipContents{}, status.Errorf(codes.InvalidArgument, "zip total uncompressed size exceeds %d bytes", limits.maxTotalBytes)
		}
		if closeErr != nil {
			return ZipContents{}, fmt.Errorf("close zip entry %q: %w", f.Name, closeErr)
		}

		switch name {
		case "index.js":
			hasIndexJS = true
		case "main.py":
			hasMainPy = true
		case "package-lock.json":
			hasLockfile = true
		case "package.json":
			// 依赖探测（v3 §3.1）：只读根 package.json 的 dependencies 键。
			deps, parseErr := packageJSONHasDeps(f)
			if parseErr != nil {
				return ZipContents{}, parseErr
			}
			nodeDeps = deps
		}
	}

	switch {
	case hasIndexJS:
		return ZipContents{Runtime: "node-18.0", NodeDeps: nodeDeps, HasLockfile: hasLockfile}, nil
	case hasMainPy:
		// python 探测保留（报错信息可指认根因），构建期由 runner.DockerfileFor
		// 明确拒绝——常驻执行器仅支持 node，python runner 未实现。
		return ZipContents{Runtime: "python-3.11", NodeDeps: nodeDeps, HasLockfile: hasLockfile}, nil
	default:
		return ZipContents{}, status.Error(codes.InvalidArgument, "missing entrypoint file: expected index.js (node)")
	}
}

// packageJSONHasDeps 只读 zip 条目（根 package.json）的 dependencies 键并
// 判定非空（v3 §3.1 探测条件；devDependencies 不触发代装）。坏 JSON 是
// 用户代码包的明确错误——报错并携带解析错误，不静默按无依赖处理。
func packageJSONHasDeps(f *zip.File) (bool, error) {
	src, err := f.Open()
	if err != nil {
		return false, status.Errorf(codes.InvalidArgument, "open package.json: %v", err)
	}
	defer func() { _ = src.Close() }()
	raw, err := io.ReadAll(io.LimitReader(src, maxPackageJSONBytes+1))
	if err != nil {
		return false, status.Errorf(codes.InvalidArgument, "read package.json: %v", err)
	}
	if len(raw) > maxPackageJSONBytes {
		return false, status.Errorf(codes.InvalidArgument, "package.json exceeds %d bytes", maxPackageJSONBytes)
	}
	// 值用 RawMessage 承接（官方依赖值为 string，但对象简写等历史形态合法）
	// ——只判键非空，不做 schema 校验。
	var pkg struct {
		Dependencies map[string]json.RawMessage `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return false, status.Errorf(codes.InvalidArgument, "invalid package.json: %v", err)
	}
	return len(pkg.Dependencies) > 0, nil
}

// firstPathSegment 返回 zip 条目名的第一段（归一化反斜杠与前导 "./"）。
func firstPathSegment(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[:i]
	}
	return name
}

// readBuildOutput 逐行读取 docker build 输出流，保留尾部 maxBuildLogBytes 字节，
// 并扫描 `{"error":...}` / `{"errorDetail":{"message":...}}` JSON（BuildKit 失败
// 消息位于流末尾，不产生 Go error）。返回 (日志尾部, 构建错误)。
func readBuildOutput(r io.Reader) (string, error) {
	var log tailBuffer
	var buildErr error
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxBuildLogLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = log.Write(append(line, '\n'))
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Error != "" {
			buildErr = errors.New(msg.Error)
			break
		}
		if msg.ErrorDetail.Message != "" {
			buildErr = errors.New(msg.ErrorDetail.Message)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return log.String(), err
	}
	return log.String(), buildErr
}

// ReadBuildOutput 是 readBuildOutput 的导出版（functions-dispatcher 的构建
// 复用同一 BuildKit error 流解析）。
func ReadBuildOutput(r io.Reader) (string, error) {
	return readBuildOutput(r)
}

func truncateLog(s string) string {
	if len(s) <= maxBuildLogBytes {
		return s
	}
	return s[:maxBuildLogBytes]
}

// TruncateBuildLog 是 truncateLog 的导出版（构建日志裁剪口径共用）。
func TruncateBuildLog(s string) string { return truncateLog(s) }

// tailBuffer 仅保留最后 maxBuildLogBytes 字节（构建失败原因通常在输出末尾）。
type tailBuffer struct {
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > maxBuildLogBytes {
		drop := len(b.buf) - maxBuildLogBytes
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:maxBuildLogBytes]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
