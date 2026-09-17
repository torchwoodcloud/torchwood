package functions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/packer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PackerClient 是 SourcePacker 端口（二期阶段 3，设计 §2）的 HTTP 适配实现：
// 经 packer 服务的内网 API（POST /v1/pack/git）把 GitSource 物化
// 为 zip 代码包。zip 流向反转：packer 打好 zip 交回 server 落既有 zipPath——
// 本客户端只做协议适配，不落盘不落库。凭证（Username/Token）只在调用栈
// 内存随请求体送达 packer，本实现不持久化、不写日志、不回显（D8）。
type PackerClient struct {
	baseURL     string
	sharedToken string
	// maxZipBytes 是 packer 侧物化 zip 预算的 config 投影（响应读取上限的
	// 派生输入，见 packResponseLimit；0 = 缺省 50MiB）。
	maxZipBytes int64
	hc          *http.Client
}

// NewPackerClient 构造 packer HTTP 客户端（SourcePacker 装配；url 未配置不
// 是构造错误——git 源是增量启用能力，PackGit 调用时才报 FailedPrecondition，
// zip/node 完全不受影响，wire 装配保持恒可）。
func NewPackerClient(cfg *config.AppConfig) *PackerClient {
	p := cfg.GetFunctions().GetPacker()
	// 客户端总超时 = packer 侧 fetch_timeout 封顶 + 传输余量：packer 对单次
	// 打包整体封顶（clone+核算+物化共享同一预算，服务端到点回 504），客户端
	// 只需防连接级挂起（对齐 DispatcherExecutor 的「不设响应头超时」 vs
	// 「重资源操作要有界」折中——pack 在部署请求关键路径上，必须有界）。
	timeout := packer.DefaultFetchTimeout
	if d, err := time.ParseDuration(p.GetFetchTimeout()); err == nil && d > 0 {
		timeout = d
	}
	return &PackerClient{
		baseURL:     p.GetUrl(),
		sharedToken: p.GetSharedToken(),
		maxZipBytes: p.GetMaxZipBytes(),
		hc: &http.Client{
			Timeout: timeout + 30*time.Second,
			Transport: &http.Transport{
				// 内网同宿主回环/桥网络：短超时拨号 + 不走代理。
				Proxy:           nil,
				MaxIdleConns:    8,
				IdleConnTimeout: 90 * time.Second,
				DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
		},
	}
}

// packResponseLimit 计算 /v1/pack/git 响应读取上限：合法响应上界 =
// max_zip_bytes（缺省 50MiB）的 zip 经 base64 膨胀（×4/3 ≈ 66.7MiB）加
// JSON 包装与 checksum 字段，取 +1MiB 余量。上限只防失配/恶意响应的无限
// 读取（不构成协议约束——packer 侧 max_zip_bytes 物化预算才是权威；配置
// 调大 max_zip_bytes 时本上限随之放大，合法响应不会被截断）。
func packResponseLimit(maxZipBytes int64) int64 {
	if maxZipBytes <= 0 {
		maxZipBytes = packer.DefaultMaxZipBytes
	}
	return maxZipBytes/3*4 + 1<<20
}

// PackGit 调 packer 把 GitSource 物化为 zip 代码包。错误映射与
// packer 服务端状态码口径互逆（429 → ResourceExhausted、504 →
// DeadlineExceeded、400 → InvalidArgument、401 → FailedPrecondition、
// 其余 → Internal）。
func (c *PackerClient) PackGit(ctx context.Context, src domainfunctions.GitSource) (string, string, []byte, error) {
	if c.baseURL == "" {
		return "", "", nil, status.Error(codes.FailedPrecondition, "functions.packer.url is not configured (git deployment source disabled)")
	}
	payload, err := json.Marshal(packer.PackRequest{
		URL:       src.URL,
		Ref:       src.Ref,
		Directory: src.Directory,
		Username:  src.Username,
		Token:     src.Token,
	})
	if err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "marshal packer request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/pack/git", bytes.NewReader(payload))
	if err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "build packer request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.sharedToken != "" {
		req.Header.Set("X-Tw-Packer-Token", c.sharedToken)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, packResponseLimit(c.maxZipBytes)))
	if err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "read packer response: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := errBody.Error
		if msg == "" {
			msg = truncateLog(string(body))
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			return "", "", nil, status.Error(codes.ResourceExhausted, msg)
		case http.StatusGatewayTimeout:
			return "", "", nil, status.Error(codes.DeadlineExceeded, msg)
		case http.StatusBadRequest:
			return "", "", nil, status.Error(codes.InvalidArgument, msg)
		case http.StatusUnauthorized:
			return "", "", nil, status.Error(codes.FailedPrecondition, "packer authentication failed")
		default:
			return "", "", nil, status.Errorf(codes.Internal, "packer error (http %d): %s", resp.StatusCode, msg)
		}
	}
	var out packer.PackResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "decode packer response: %v", err)
	}
	zip, err := base64.StdEncoding.DecodeString(out.ZipBase64)
	if err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "decode packer zip payload: %v", err)
	}
	return out.CommitSHA, out.Checksum, zip, nil
}
