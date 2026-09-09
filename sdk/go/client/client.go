// Package client 是 Torchwood Client API 的 Go 客户端（end-user Bearer JWT，
// 自动刷新 token）。
package client

import (
	"sync"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/internal/conn"
	"google.golang.org/grpc"
)

// refreshSkew 主动刷新的提前量。
const refreshSkew = 30 * time.Second

// Config 汇总 Client API 客户端配置，由 Option 修改。
type Config struct {
	ProjectID  string
	DatabaseID string

	tokenStore      TokenStore
	onTokensChanged func(*clientv1.TokenBundle)
	initialTokens   *clientv1.TokenBundle

	timeout       time.Duration // <=0 视为 conn.DefaultTimeout
	retryDisabled bool
	dialOptions   []grpc.DialOption
}

// Option 以函数选项模式修改 Config。
type Option func(*Config)

// WithProjectID 设置默认项目 ID（填入 Account 请求的 project_id）。
func WithProjectID(projectID string) Option { return func(c *Config) { c.ProjectID = projectID } }

// WithDatabaseID 设置文档 API 的默认 database_id（可选便利默认，
// 不知道时用 Client.UseDatabase 晚绑定）。
func WithDatabaseID(databaseID string) Option { return func(c *Config) { c.DatabaseID = databaseID } }

// WithTokenStore 设置 token 持久化；默认 MemoryTokenStore。
func WithTokenStore(store TokenStore) Option { return func(c *Config) { c.tokenStore = store } }

// WithOnTokensChanged 设置 token 变化回调（登录/刷新/清空；清空传 nil）。
func WithOnTokensChanged(fn func(*clientv1.TokenBundle)) Option {
	return func(c *Config) { c.onTokensChanged = fn }
}

// WithInitialTokens 预置已有会话的 token。
func WithInitialTokens(t *clientv1.TokenBundle) Option {
	return func(c *Config) { c.initialTokens = t }
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

// Client 封装 Torchwood Client API（end-user 认证，自动刷新 token）。
type Client struct {
	cfg   Config
	conn  *grpc.ClientConn
	store TokenStore

	mu  sync.Mutex // 串行化刷新
	now func() time.Time

	account       clientv1.AccountServiceClient
	groups        clientv1.GroupsServiceClient
	databases     clientv1.DatabasesServiceClient
	payments      clientv1.PaymentsServiceClient
	assets        clientv1.AssetsServiceClient
	subscriptions clientv1.SubscriptionsServiceClient
	functions     clientv1.FunctionsServiceClient

	// Account 提供注册/登录/账户管理。
	Account *AccountService
	// Groups 提供用户组与成员管理。
	Groups *GroupsService
	// Databases 提供文档 CRUD，绑定默认 DatabaseID。
	Databases *DatabasesService
	// Payments 提供终端用户建单与本人订单查询。
	Payments *PaymentsService
	// Assets 提供终端用户只读资产查询（无写入口）。
	Assets *AssetsService
	// Subscriptions 提供计划列表、订阅与期末取消。
	Subscriptions *SubscriptionsService
	// Functions 提供客户端调用面（P2：按 per-function 策略同步调用函数）。
	Functions *FunctionsService
}

// New 建立 Client API 连接。target 为 gRPC 目标地址，不能为空。
func New(target string, opts ...Option) (*Client, error) {
	var cfg Config
	for _, o := range opts {
		o(&cfg)
	}
	store := cfg.tokenStore
	if store == nil {
		store = NewMemoryTokenStore()
	}
	c := &Client{cfg: cfg, store: store, now: time.Now}
	if cfg.initialTokens != nil {
		if err := c.store.Save(cfg.initialTokens); err != nil {
			return nil, err
		}
	}
	dialOpts := append([]grpc.DialOption{
		// 超时兜底在最外层（含刷新/401 重试逻辑的整体预算），auth 在内层。
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
	c.account = clientv1.NewAccountServiceClient(gc)
	c.groups = clientv1.NewGroupsServiceClient(gc)
	c.databases = clientv1.NewDatabasesServiceClient(gc)
	c.payments = clientv1.NewPaymentsServiceClient(gc)
	c.assets = clientv1.NewAssetsServiceClient(gc)
	c.subscriptions = clientv1.NewSubscriptionsServiceClient(gc)
	c.functions = clientv1.NewFunctionsServiceClient(gc)
	c.Account = &AccountService{c: c}
	c.Groups = &GroupsService{c: c}
	c.Databases = c.UseDatabase(cfg.DatabaseID)
	c.Payments = &PaymentsService{c: c}
	c.Assets = &AssetsService{c: c}
	c.Subscriptions = &SubscriptionsService{c: c}
	c.Functions = &FunctionsService{c: c}
	return c, nil
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.conn.Close() }

// UseDatabase 返回绑定指定 databaseID 的文档服务副本。
func (c *Client) UseDatabase(databaseID string) *DatabasesService {
	return newDatabasesService(c, databaseID)
}

// saveTokens 保存 token 并触发回调。
func (c *Client) saveTokens(t *clientv1.TokenBundle) error {
	if err := c.store.Save(t); err != nil {
		return err
	}
	if c.cfg.onTokensChanged != nil {
		c.cfg.onTokensChanged(t)
	}
	return nil
}

// clearTokens 清空 token 并触发回调（nil）。
func (c *Client) clearTokens() {
	_ = c.store.Clear()
	if c.cfg.onTokensChanged != nil {
		c.cfg.onTokensChanged(nil)
	}
}
