// Package server 是 Torchwood Server API 的 Go 客户端（x-api-key 认证）。
package server

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/internal/conn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Config 汇总 Server API 客户端配置，由 Option 修改。
type Config struct {
	// APIKey 用于 x-api-key 头认证。
	APIKey string
	// ProjectID 非空时以 x-torchwood-project 头发送（admin session 项目覆盖）。
	ProjectID string
	// DatabaseID 是文档 API 使用的默认 database_id。
	DatabaseID string

	timeout       time.Duration // <=0 视为 conn.DefaultTimeout
	retryDisabled bool
	// dialOptions 透传给底层拨号。
	dialOptions []grpc.DialOption
}

// Option 以函数选项模式修改 Config。
type Option func(*Config)

// WithAPIKey 设置 Server API 认证用的 API Key。
func WithAPIKey(key string) Option { return func(c *Config) { c.APIKey = key } }

// WithProjectID 设置 x-torchwood-project 头。
func WithProjectID(projectID string) Option { return func(c *Config) { c.ProjectID = projectID } }

// WithDatabaseID 设置文档 API 的默认 database_id。
func WithDatabaseID(databaseID string) Option {
	return func(c *Config) { c.DatabaseID = databaseID }
}

// WithTimeout 设置单次调用的默认超时（默认 30s，<=0 回落默认值）。仅对
// 未携带 deadline 的 ctx 生效；调用方已有 deadline 时原样尊重。
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.timeout = d } }

// WithRetryDisabled 关闭默认的 UNAVAILABLE 自动重试 service config
// （默认开启；非幂等写敏感的调用方可显式关闭）。
func WithRetryDisabled() Option { return func(c *Config) { c.retryDisabled = true } }

// WithDialOptions 附加底层 gRPC 拨号选项。
func WithDialOptions(opts ...grpc.DialOption) Option {
	return func(c *Config) { c.dialOptions = append(c.dialOptions, opts...) }
}

// Client 封装 Torchwood Server API（API Key 认证）。
type Client struct {
	cfg  Config
	conn *grpc.ClientConn

	health    serverv1.HealthServiceClient
	users     serverv1.UsersServiceClient
	groups    serverv1.GroupsServiceClient
	databases serverv1.DatabasesServiceClient

	// Health 提供服务健康检查（ACCESS_PUBLIC，可不配置 API Key）。
	Health *HealthService
	// Users 提供用户管理（含 CreateUserToken 模拟登录）。
	Users *UsersService
	// Groups 提供服务端用户组管理。
	Groups *GroupsService
	// Databases 提供库/集合/属性/索引/文档管理，绑定默认 DatabaseID。
	Databases *DatabasesService
	// Projects 提供项目管理。
	Projects *ProjectsService
	// Storage 提供存储元数据管理（上传/下载走独立 HTTP handler）。
	Storage *StorageService
	// Functions 提供函数管理（运行时/部署/变量/执行）。
	Functions *FunctionsService
	// OAuthProviders 提供 OAuth 提供商管理。
	OAuthProviders *OAuthProvidersService
	// Payments 提供订单查询、退款与人工履约。
	Payments *PaymentsService
	// Assets 提供资产定义 CRUD 与五动词（终端用户无写入口）。
	Assets *AssetsService
	// Subscriptions 提供计划 CRUD 与订阅强制取消/过期。
	Subscriptions *SubscriptionsService
	// Billing 提供用量合计、小时 rollup 与月账单文档（只读）。
	Billing *BillingService
	// Outbox 提供死信查询与重放。
	Outbox *OutboxService
}

// New 建立 Server API 连接。target 为 gRPC 目标地址，不能为空。
func New(target string, opts ...Option) (*Client, error) {
	var cfg Config
	for _, o := range opts {
		o(&cfg)
	}
	c := &Client{cfg: cfg}
	dialOpts := append([]grpc.DialOption{
		// 超时兜底在最外层（含 auth 头注入的开销），auth 在内层。
		grpc.WithChainUnaryInterceptor(conn.TimeoutUnaryInterceptor(cfg.timeout), c.authInterceptor()),
	}, cfg.dialOptions...)
	if !cfg.retryDisabled {
		dialOpts = append(dialOpts, conn.RetryDialOption())
	}
	gc, err := conn.Dial(target, dialOpts...)
	if err != nil {
		return nil, err
	}
	c.conn = gc
	c.health = serverv1.NewHealthServiceClient(gc)
	c.users = serverv1.NewUsersServiceClient(gc)
	c.groups = serverv1.NewGroupsServiceClient(gc)
	c.databases = serverv1.NewDatabasesServiceClient(gc)
	c.Health = &HealthService{c: c, api: c.health}
	c.Users = &UsersService{c: c}
	c.Groups = &GroupsService{c: c}
	c.Databases = c.UseDatabase(cfg.DatabaseID)
	c.Projects = &ProjectsService{c: c, api: serverv1.NewProjectsServiceClient(gc)}
	c.Storage = &StorageService{c: c, api: serverv1.NewStorageServiceClient(gc)}
	c.Functions = &FunctionsService{c: c, api: serverv1.NewFunctionsServiceClient(gc)}
	c.OAuthProviders = &OAuthProvidersService{c: c, api: serverv1.NewOAuthProvidersServiceClient(gc)}
	c.Payments = &PaymentsService{c: c, api: serverv1.NewPaymentsServiceClient(gc)}
	c.Assets = &AssetsService{c: c, api: serverv1.NewAssetsServiceClient(gc)}
	c.Subscriptions = &SubscriptionsService{c: c, api: serverv1.NewSubscriptionsServiceClient(gc)}
	c.Billing = &BillingService{c: c, api: serverv1.NewBillingServiceClient(gc)}
	c.Outbox = &OutboxService{c: c, api: serverv1.NewOutboxServiceClient(gc)}
	return c, nil
}

// authInterceptor 注入 x-api-key 与 x-torchwood-project 头。
func (c *Client) authInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(c.authContext(ctx), method, req, reply, cc, opts...)
	}
}

func (c *Client) authContext(ctx context.Context) context.Context {
	md := metadata.MD{}
	if c.cfg.APIKey != "" {
		md.Set("x-api-key", c.cfg.APIKey)
	}
	if c.cfg.ProjectID != "" {
		md.Set("x-torchwood-project", c.cfg.ProjectID)
	}
	if len(md) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.conn.Close() }
