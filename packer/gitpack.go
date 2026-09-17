package packer

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gittransport "github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PackOptions 是单次打包的预算与安全开关，由进程装配（service.go）从
// config 派生。不变量：SSRF 口径（AllowInsecure）启动即固定、不随请求
// 变化——go-git 的协议表是进程级全局（见 installGuardedHTTPTransport）。
// 零值字段按缺省预算归一（normalizePackOptions）。
type PackOptions struct {
	MaxRepoBytes  int64         // worktree 磁盘侧预算（config max_repo_bytes）
	MaxZipBytes   int64         // 物化 zip 传输侧预算（config max_zip_bytes）
	MaxEntries    int           // 条目上限（缺省 MaxPackEntries；config 不暴露）
	FetchTimeout  time.Duration // 单次 fetch+物化整体超时（config fetch_timeout）
	AllowInsecure bool          // 放行 http:// 与私网/回环目标（config allow_insecure）
}

// normalizePackOptions 把零值/非法字段归一到缺省预算（与 config.proto
// 注释的默认值同源）。
func normalizePackOptions(opts PackOptions) PackOptions {
	if opts.MaxRepoBytes <= 0 {
		opts.MaxRepoBytes = DefaultMaxRepoBytes
	}
	if opts.MaxZipBytes <= 0 {
		opts.MaxZipBytes = DefaultMaxZipBytes
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = MaxPackEntries
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = DefaultFetchTimeout
	}
	return opts
}

// ——预算哨兵错误：walk 内部传递，PackGit 统一映射为 ResourceExhausted——
var (
	errRepoBudget    = errors.New("worktree exceeds max_repo_bytes")
	errZipBudget     = errors.New("materialized zip exceeds max_zip_bytes")
	errEntriesBudget = errors.New("worktree exceeds max entries")
)

// PackGit 把 req.URL@req.Ref 的 req.Directory 子目录物化为 zip：
// clone（浅/全量按 ref 形态）→ worktree 核算（两级预算 + 条目）→ 穿越
// 校验后的子目录流式写 zip（跳 .git/symlink、拒 node_modules，与 zip
// 通道同口径）。错误一律 grpc status：InvalidArgument（URL/directory/
// node_modules）、NotFound（仓库/ref 未命中）、ResourceExhausted（预算）、
// DeadlineExceeded（fetch_timeout，由服务层按封顶 ctx 判定归一）、
// Internal（其余）。所有中间态收敛在临时目录，任何失败路径 defer 清理。
func PackGit(ctx context.Context, req PackRequest, opts PackOptions) (*PackResponse, error) {
	if req.URL == "" {
		return nil, status.Error(codes.InvalidArgument, "url is required")
	}
	opts = normalizePackOptions(opts)
	// file:// 的放行语义复用 TORCHWOOD_ENV（development 专属，设计 §2）；
	// 进程环境在生产是静态的，逐请求读取只为测试可控。
	devEnv := config.CurrentRuntimeEnv() == config.EnvDevelopment
	srcURL, err := validateSourceURL(req.URL, devEnv, opts.AllowInsecure)
	if err != nil {
		return nil, err
	}
	tmpRoot, err := os.MkdirTemp("", "tw-pack-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()

	var (
		repoRoot  string
		commitSHA string
	)
	if isLocalSource(srcURL) {
		repoRoot, commitSHA, err = materializeLocalSource(srcURL, tmpRoot, req)
	} else {
		repoRoot, commitSHA, err = cloneAndCheckout(ctx, tmpRoot, srcURL, req)
	}
	if err != nil {
		return nil, err
	}
	if err := auditWorktree(repoRoot, opts); err != nil {
		return nil, budgetErr(err, opts)
	}
	ctxRoot, err := resolveContextRoot(repoRoot, req.Directory)
	if err != nil {
		return nil, err
	}
	zipBytes, err := materializeZip(ctxRoot, opts)
	if err != nil {
		return nil, budgetErr(err, opts)
	}
	sum := sha256.Sum256(zipBytes)
	return &PackResponse{
		CommitSHA: commitSHA,
		Checksum:  hex.EncodeToString(sum[:]),
		ZipBase64: base64.StdEncoding.EncodeToString(zipBytes),
	}, nil
}

// budgetErr 把 walk 物化过程中的预算哨兵映射为带限额值的
// ResourceExhausted，其余（如 node_modules 拒收的 InvalidArgument）原样
// 透传。
func budgetErr(err error, opts PackOptions) error {
	switch {
	case errors.Is(err, errRepoBudget):
		return status.Errorf(codes.ResourceExhausted, "repository worktree exceeds functions.packer.max_repo_bytes (%d bytes)", opts.MaxRepoBytes)
	case errors.Is(err, errZipBudget):
		return status.Errorf(codes.ResourceExhausted, "materialized zip exceeds functions.packer.max_zip_bytes (%d bytes)", opts.MaxZipBytes)
	case errors.Is(err, errEntriesBudget):
		return status.Errorf(codes.ResourceExhausted, "repository exceeds %d entries", opts.MaxEntries)
	default:
		return err
	}
}

// validateSourceURL 校验部署源 URL 并归一为可 clone 形态：仅 https（
// allow_insecure 放行 http）；拒绝 URL 内嵌 userinfo（凭证走独立字段，
// 防 URL 落日志泄密，设计 §2）；file:// 与裸本地路径仅 development
// （Windows 盘符 "D:\x" 会被 url.Parse 误判为 scheme="d"，故裸路径先于
// URL 解析判定）。file:// 归一为本地绝对路径（go-git file 传输两形态
// 等价，统一形态便于 isLocalSource 判定与测试断言）。
func validateSourceURL(raw string, devEnv, allowInsecure bool) (string, error) {
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, `\\`) {
		if !devEnv {
			return "", status.Error(codes.InvalidArgument, "local path git source is only allowed with TORCHWOOD_ENV=development")
		}
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "invalid git url: %v", err)
	}
	// URL 拒绝内嵌 userinfo：带凭证的 URL 会原样进入错误消息/日志，
	// 凭证必须走 username/token 独立字段。
	if u.User != nil {
		return "", status.Error(codes.InvalidArgument, "git url must not embed credentials; pass username/token fields instead")
	}
	switch u.Scheme {
	case "https":
		return raw, nil
	case "http":
		if !allowInsecure {
			return "", status.Error(codes.InvalidArgument, "http git url requires functions.packer.allow_insecure")
		}
		return raw, nil
	case "file":
		if !devEnv {
			return "", status.Error(codes.InvalidArgument, "file:// git url is only allowed with TORCHWOOD_ENV=development")
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", status.Errorf(codes.InvalidArgument, "file:// url must not reference a remote host %q", u.Host)
		}
		return localPathFromURL(u), nil
	case "":
		return "", status.Error(codes.InvalidArgument, "git url is required")
	default:
		return "", status.Errorf(codes.InvalidArgument, "unsupported git url scheme %q (https required; http/file with constraints)", u.Scheme)
	}
}

// localPathFromURL 把 file:///path 形态还原为本地路径（Windows 盘符
// file:///D:/x 的 Path 形如 "/D:/x"，剥前导斜杠）。
func localPathFromURL(u *url.URL) string {
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// isLocalSource 判定归一后的源是否本地路径（无 scheme）：file 传输无
// 凭证概念、不支持 shallow 协商，走独立分支。
func isLocalSource(srcURL string) bool {
	return !strings.Contains(srcURL, "://")
}

// ——ref 解析与克隆（设计 §2「clone 实现」）——

const (
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
)

// isFullSHA 判定 40 位 hex（git SHA-1 全量形态）。
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// cloneAndCheckout 克隆到 root 下的独立子目录并 checkout 钉死提交，返回
// (worktree 根, commit SHA)。ref 解析顺序（设计 §2）：空 = HEAD → 40 位
// hex 直取提交 → branch（refs/heads/）→ tag（refs/tags/）；命名 ref 均
// 未命中报 NotFound，不静默回落 HEAD——回落会把部署钉到非请求内容。
// 解析后经 ResolveRevision 剥离（tag object → commit）并显式 checkout，
// 保证 worktree 内容与返回的 commit SHA 一致。
func cloneAndCheckout(ctx context.Context, root, srcURL string, req PackRequest) (string, string, error) {
	base := &gogit.CloneOptions{
		URL:  srcURL,
		Auth: gitAuth(srcURL, req),
		// http(s) 走 Depth:1 + 单 ref refspec（SingleBranch，不取全量 refs）。
		Depth: 1,
	}

	var (
		repo *gogit.Repository
		rev  plumbing.Revision
		err  error
	)
	switch {
	case req.Ref == "":
		repo, err = doClone(ctx, root, base, "", 1)
		rev = "HEAD"
	case isFullSHA(req.Ref):
		// 40 位 hex：服务端 SHA-want 能力（allow-*-sha1-in-want）因 forge
		// 而异、浅克隆不可移植——按设计 §2 的回退路径统一全量克隆 +
		// revision 解析，由 fetch_timeout 封顶与 max_repo_bytes 预算兜底。
		full := *base
		full.Depth = 0
		repo, err = doClone(ctx, root, &full, "", 0)
		rev = plumbing.Revision(req.Ref)
	default:
		// branch → tag 依次浅克隆单 ref（每个尝试用独立子目录，失败的
		// 尝试不污染下一次 PlainClone 的空目录前提）。
		shallow := *base
		shallow.SingleBranch = true
		repo, err = doClone(ctx, root, &shallow, refsHeadsPrefix+req.Ref, 1)
		rev = plumbing.Revision(refsHeadsPrefix + req.Ref)
		if err != nil && isRefMiss(err) {
			repo, err = doClone(ctx, root, &shallow, refsTagsPrefix+req.Ref, 1)
			rev = plumbing.Revision(refsTagsPrefix + req.Ref)
		}
		// 两个形态都未命中：明确 NotFound（不静默回落 HEAD——回落会把
		// 部署钉到非请求内容）。
		if err != nil && isRefMiss(err) {
			return "", "", status.Errorf(codes.NotFound, "ref %q not found (tried refs/heads and refs/tags)", req.Ref)
		}
	}
	if err != nil {
		return "", "", cloneErr(err, req.Ref)
	}
	commit, err := repo.ResolveRevision(rev)
	if err != nil {
		return "", "", status.Errorf(codes.NotFound, "ref %q not found in repository: %v", req.Ref, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "worktree: %v", err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: *commit}); err != nil {
		return "", "", status.Errorf(codes.Internal, "checkout %s: %v", commit, err)
	}
	return wt.Filesystem.Root(), commit.String(), nil
}

// doClone 执行一次克隆尝试：root 下建独立子目录（PlainClone 要求空目录），
// refName 非空时设置单 ref ReferenceName。
func doClone(ctx context.Context, root string, o *gogit.CloneOptions, refName string, depth int) (*gogit.Repository, error) {
	dir, err := os.MkdirTemp(root, "clone-*")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create clone dir: %v", err)
	}
	opt := *o
	opt.ReferenceName = plumbing.ReferenceName(refName)
	opt.Depth = depth
	return gogit.PlainCloneContext(ctx, dir, false, &opt)
}

// materializeLocalSource 是本地源（file:// 或裸绝对路径，仅 development）
// 的等价物：不经传输层 clone——go-git 的 file 客户端会 shell-out 到
// git-upload-pack（Windows 上是 sh 脚本、无法直接 exec），而 dev 场景仓库
// 就在本机，只读打开 + 把目标提交的树物化到临时目录语义等价且零外部依赖。
// ref 解析与 cloneAndCheckout 同序（HEAD → 40hex → branch → tag）；symlink
// 条目不物化（其 blob 内容是 target 路径字符串——物化成常规文件会让 zip
// 携带错误内容，与 zip 写侧跳过 symlink 的口径一致。容器内 Linux 实跑
// 暴露：tree.Files() 并不天然只含常规 blob，Windows 宿主因 symlink 特权
// 缺失而测试假绿）；submodule 条目防御性跳过。
func materializeLocalSource(srcURL, root string, req PackRequest) (string, string, error) {
	repo, err := gogit.PlainOpen(srcURL)
	if err != nil {
		if errors.Is(err, gogit.ErrRepositoryNotExists) ||
			strings.Contains(err.Error(), "repository does not exist") {
			return "", "", status.Errorf(codes.NotFound, "git repository not found: %s", srcURL)
		}
		return "", "", status.Errorf(codes.InvalidArgument, "open local repository: %v", err)
	}
	rev := plumbing.Revision("HEAD")
	if req.Ref != "" {
		rev = plumbing.Revision(req.Ref)
		if !isFullSHA(req.Ref) {
			// 命名 ref：branch 优先，miss 再试 tag（与远端同序）。
			if _, err := repo.ResolveRevision(plumbing.Revision(refsHeadsPrefix + req.Ref)); err == nil {
				rev = plumbing.Revision(refsHeadsPrefix + req.Ref)
			} else if _, err := repo.ResolveRevision(plumbing.Revision(refsTagsPrefix + req.Ref)); err == nil {
				rev = plumbing.Revision(refsTagsPrefix + req.Ref)
			} else {
				return "", "", status.Errorf(codes.NotFound, "ref %q not found (tried refs/heads and refs/tags)", req.Ref)
			}
		}
	}
	hash, err := repo.ResolveRevision(rev)
	if err != nil {
		return "", "", status.Errorf(codes.NotFound, "resolve ref %q: %v", req.Ref, err)
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return "", "", status.Errorf(codes.InvalidArgument, "resolve commit %s: %v", hash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "commit tree: %v", err)
	}
	if err := tree.Files().ForEach(func(f *object.File) error {
		if f.Mode == filemode.Symlink || f.Mode == filemode.Submodule {
			return nil // 不物化：symlink blob 内容是 target 字符串，非文件内容
		}
		abs := filepath.Join(root, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = out.Close() }()
		in, err := f.Blob.Reader()
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		_, err = io.Copy(out, in)
		return err
	}); err != nil {
		return "", "", status.Errorf(codes.Internal, "materialize worktree: %v", err)
	}
	return root, commit.Hash.String(), nil
}

// isRefMiss 判定「远端无此 ref」类错误（branch 尝试失败后允许改试 tag；
// 仓库整体不存在不在此列，走 cloneErr → NotFound）。go-git 对缺失
// ReferenceName 的报错形态随传输层不同（local: plumbing.ErrReferenceNotFound
// 链；各传输: NoMatchingRefSpecError "couldn't find remote ref"），按错误
// 链 + 消息双口径匹配。
func isRefMiss(err error) bool {
	if errors.Is(err, plumbing.ErrReferenceNotFound) ||
		errors.Is(err, gogit.ErrBranchNotFound) ||
		errors.Is(err, gogit.ErrTagNotFound) ||
		errors.Is(err, gogit.NoMatchingRefSpecError{}) {
		return true
	}
	return strings.Contains(err.Error(), "couldn't find remote ref")
}

// cloneErr 把克隆失败映射为明确错误：仓库不存在 → NotFound（404 明确
// 报错，设计 §2）；认证失败 → InvalidArgument（调用方凭证问题，非服务端
// 故障）；ctx 超时 → DeadlineExceeded；其余 Internal（阶段 3 的 server
// 侧适配器按码还原 grpc status）。
func cloneErr(err error, ref string) error {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "git fetch exceeded functions.packer.fetch_timeout")
	case errors.Is(err, gittransport.ErrRepositoryNotFound) ||
		strings.Contains(msg, "repository does not exist") ||
		strings.Contains(msg, "does not appear to be a git repository"):
		return status.Errorf(codes.NotFound, "git repository not found: %s", msg)
	case errors.Is(err, gittransport.ErrAuthenticationRequired) ||
		errors.Is(err, gittransport.ErrAuthorizationFailed):
		return status.Errorf(codes.InvalidArgument, "git authentication failed: %s", msg)
	case strings.Contains(msg, "shallow not supported"):
		return status.Errorf(codes.Internal, "git server does not support shallow fetch: %s", msg)
	default:
		return status.Errorf(codes.Internal, "git clone ref %q: %s", ref, msg)
	}
}

// gitAuth 构造 http(s) Basic 凭证：username 空回落字面量 "git"（GitHub/
// GitLab PAT 通用形态，设计 §2）；本地路径源无凭证概念。两者皆空 = 匿名
// 拉取公共仓库。
func gitAuth(srcURL string, req PackRequest) gittransport.AuthMethod {
	if isLocalSource(srcURL) || (req.Token == "" && req.Username == "") {
		return nil
	}
	username := req.Username
	if username == "" {
		username = "git"
	}
	return &githttp.BasicAuth{Username: username, Password: req.Token}
}

// ——SSRF 防护（拨号点，设计 §2）——

// installGuardedHTTPTransport 把带 SSRF 防护的 *http.Client 注册为 go-git
// 的 http/https 传输。不变量：go-git 协议表（client.InstallProtocol）是
// 进程级全局、缺省安装发生在 NewService 一次——packer 进程的 SSRF 口径
// 启动即固定（allow_insecure 来自 config、不随请求变化），全局安装与
// 进程策略天然一致；测试如需切换口径必须串行执行（不 t.Parallel）。
func installGuardedHTTPTransport(allowInsecure bool) {
	c := newGuardedHTTPClient(allowInsecure)
	gitclient.InstallProtocol("https", githttp.NewClient(c))
	gitclient.InstallProtocol("http", githttp.NewClient(c))
}

// newGuardedHTTPClient 构造拨号点校验的 HTTP 客户端：基于 DefaultTransport
// 克隆（保留代理/TLS 缺省），仅替换 DialContext 为带 Control 钩子的
// dialer。不设 DialTLSContext——TLS 握手叠在受控拨号之上，Control 才能拦截。
// 代理场景下 Control 看到的是代理地址（拨号终点即代理，代理本身必须
// 可信，与出网代理的既有信任模型一致）。
func newGuardedHTTPClient(allowInsecure bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control 在 connect 系统调用时拿到将拨号的真实地址（TCP 下
		// host 已是解析后的 IP 字面量，多 A 记录每连接触发一次）——校验
		// 实施在拨号点而非请求前预解析，解析与连接之间换 DNS 记录的
		// rebinding 因此失效（设计 §2）。
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddress(address, allowInsecure)
		},
	}
	tr.DialContext = dialer.DialContext
	return &http.Client{Transport: tr}
}

// checkDialAddress 是拨号点 SSRF 校验：默认拒绝 loopback / private /
// link-local / 未指定地址（169.254.169.254 云元数据端点等），出现主机名
// 说明走错钩子、fail-closed。allow_insecure（自托管内网 git 服务）时
// 整体放行——给显式开关而非逼出危险旁路（设计 §2）。
func checkDialAddress(address string, allowInsecure bool) error {
	if allowInsecure {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf guard: non-ip dial address %q", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("ssrf guard: dial to %q is not allowed (loopback/private/link-local); set functions.packer.allow_insecure for self-hosted git", host)
	}
	return nil
}

// ——worktree 核算与物化（设计 §2「物化」）——

// auditWorktree 在 checkout 后核算 worktree（磁盘侧预算）：常规文件总
// 字节与条目数，超限即清理报错。跳过 .git 与非常规条目（symlink 等）——
// 预算只对将被物化的内容计，与 zip 写侧口径一致。
func auditWorktree(root string, opts PackOptions) error {
	var totalBytes int64
	var entries int
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlink/fifo 等不进 zip，不进预算
		}
		entries++
		if entries > opts.MaxEntries {
			return errEntriesBudget
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		totalBytes += info.Size()
		if totalBytes > opts.MaxRepoBytes {
			return errRepoBudget
		}
		return nil
	})
	return walkErr
}

// resolveContextRoot 校验 directory 并返回物化上下文根：Clean 后拒绝
// `..` 逃逸与绝对路径（穿越校验，设计 §2），再用 filepath.Rel 复核仍在
// worktree 内（双保险）；目录不存在明确报错（空 zip 比报错更难排查）。
func resolveContextRoot(worktreeRoot, directory string) (string, error) {
	if directory == "" {
		return worktreeRoot, nil
	}
	clean := path.Clean(filepath.ToSlash(directory))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", status.Errorf(codes.InvalidArgument, "directory %q escapes the repository", directory)
	}
	root := filepath.Join(worktreeRoot, filepath.FromSlash(clean))
	rel, err := filepath.Rel(worktreeRoot, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", status.Errorf(codes.InvalidArgument, "directory %q escapes the repository", directory)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", status.Errorf(codes.InvalidArgument, "directory %q not found in repository", directory)
	}
	return root, nil
}

// materializeZip 把上下文根流式写为 zip：跳过 .git 与 symlink 条目；拒绝
// node_modules——与 zip 通道同口径（路径首段，相对物化上下文根，目录与
// 文件一并拒绝；internal/infra/functions 的 firstPathSegment 判定）。zip
// 条目不写目录项（读取方按文件路径推目录）、Modified 固定零值 + walk
// 字典序 → 同一 commit 产出字节级一致的 zip（checksum 稳定可审计）。
// 写出侧经 limitWriter 计数，超 max_zip_bytes 立即中止（内存占用封顶在
// 预算附近）。
func materializeZip(ctxRoot string, opts PackOptions) ([]byte, error) {
	buf := &bytes.Buffer{}
	lim := &limitWriter{w: buf, limit: opts.MaxZipBytes}
	zw := zip.NewWriter(lim)
	walkErr := filepath.WalkDir(ctxRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlink 跳过（防逃逸；与预算口径一致）
		}
		rel, err := filepath.Rel(ctxRoot, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if firstPathSegment(name) == "node_modules" {
			return status.Error(codes.InvalidArgument,
				"请勿在代码包中携带 node_modules——平台将在构建期代装依赖（跨平台二进制不兼容）")
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, f); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if lim.exceeded {
		return nil, errZipBudget
	}
	return buf.Bytes(), nil
}

// firstPathSegment 返回（zip 口径）条目名的第一段（归一反斜杠与前导
// "./"）。与 internal/infra/functions 的同名判定逻辑保持一致（本地复制，
// 不为 6 行工具函数拉入 docker 依赖链）。
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

// limitWriter 是带硬上限的计数写出器：zip.Writer 的压缩字节全部经此落
// buffer，超限置位并返回哨兵错误（zip.Writer 会吞后续写入，最终以
// exceeded 标记判定，不依赖中途错误传播）。
type limitWriter struct {
	w        *bytes.Buffer
	limit    int64
	n        int64
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.limit {
		l.exceeded = true
		return 0, errZipBudget
	}
	l.n += int64(len(p))
	return l.w.Write(p)
}
