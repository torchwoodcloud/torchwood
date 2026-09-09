package clients

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

type Database struct {
	*bun.DB
}

type DataClients struct {
	DB  *Database
	RDB *redis.Client
}

func NewDataClients(cfg *config.AppConfig, logger *slog.Logger) (*DataClients, func(), error) {
	ctx := context.Background()
	db, closeDb, err := newDatabase(cfg.GetData().GetDatabase(), logger)
	if err != nil {
		return nil, nil, err
	}

	rdb, err := newRedis(cfg.GetData().GetRedis())
	if err != nil {
		closeDb()
		return nil, nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = rdb.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		closeDb()
		_ = rdb.Close()
		return nil, nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &DataClients{
			DB:  db,
			RDB: rdb,
		}, func() {
			closeDb()
			_ = rdb.Close()
		}, nil
}

func NewDatabase(dataClients *DataClients) *Database {
	return dataClients.DB
}

func NewRedis(dataClients *DataClients) *redis.Client {
	return dataClients.RDB
}

func newDatabase(cfg *config.Database, logger *slog.Logger) (*Database, func(), error) {
	source := strings.TrimSpace(cfg.GetSource())
	if source == "" {
		return nil, func() {}, fmt.Errorf("database source is empty: set data.database.source or TORCHWOOD_DATA_DATABASE_SOURCE")
	}
	u, err := url.Parse(source)
	if err != nil {
		return nil, func() {}, fmt.Errorf("invalid database source: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, func() {}, fmt.Errorf("invalid database scheme %q: expected postgres", u.Scheme)
	}

	// WithBufferSize(2<<20)：pgdriver 默认写缓冲仅 4KB。PR2 起每次用户集合
	// 写都会把整份 outbox 信封 JSON 作为单个 JSONB 参数插入
	// document_events_outbox（256KiB 上限 + 余量），默认缓冲下 >4KB 参数报
	// bufio: buffer full，整段 RunInTx 回滚、文档写失败。测试库已在
	// testutil.SetupTestDB 对齐同值。
	//
	// 超时显式化：不设置时 pgdriver 隐式默认 ReadTimeout=10s 会在客户端
	// 杀掉长查询（大集合 CREATE INDEX / 迁移）而服务端继续执行，重试放大
	// 负载。ReadTimeout 放宽到 60s 容纳索引构建；不设服务端
	// statement_timeout（会误杀控制面迁移与拷贝任务）。
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(source),
		pgdriver.WithBufferSize(2<<20),
		pgdriver.WithDialTimeout(5*time.Second),
		pgdriver.WithReadTimeout(60*time.Second),
		pgdriver.WithWriteTimeout(10*time.Second),
		pgdriver.WithApplicationName("torchwood"),
	))
	if pool := cfg.GetPool(); pool != nil {
		maxOpen, maxIdle := normalizePoolSizes(int(pool.GetMaxOpenConns()), int(pool.GetMaxIdleConns()), logger)
		sqldb.SetMaxOpenConns(maxOpen)
		sqldb.SetMaxIdleConns(maxIdle)
		if d, err := time.ParseDuration(pool.GetConnMaxIdleTime()); err == nil {
			sqldb.SetConnMaxIdleTime(d)
		} else if pool.GetConnMaxIdleTime() != "" {
			// 配错单位（如 "30" 缺 s）静默跳过会用 database/sql 默认无限寿命，
			// 连接老化失效，必须显式告警（Round4 J5-5）。
			logger.Warn("invalid data.database.pool.conn_max_idle_time; ignoring (default unlimited)",
				slog.String("configured", pool.GetConnMaxIdleTime()))
		}
		if d, err := time.ParseDuration(pool.GetConnMaxLifetime()); err == nil {
			sqldb.SetConnMaxLifetime(d)
		} else if pool.GetConnMaxLifetime() != "" {
			logger.Warn("invalid data.database.pool.conn_max_lifetime; ignoring (default unlimited)",
				slog.String("configured", pool.GetConnMaxLifetime()))
		}
	} else {
		maxOpen := 4 * runtime.GOMAXPROCS(0)
		sqldb.SetMaxOpenConns(maxOpen)
		sqldb.SetMaxIdleConns(maxOpen)
	}

	db := bun.NewDB(sqldb, pgdialect.New())
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	pingErr := db.PingContext(pingCtx)
	cancel()
	if pingErr != nil {
		_ = db.Close()
		return nil, func() {}, fmt.Errorf("database ping failed: %w", pingErr)
	}
	if hook := NewSlowQueryHook(cfg.GetSlowQueryThreshold(), cfg.GetDebug(), logger); hook != nil {
		db.AddQueryHook(hook)
	}
	return &Database{db}, func() { _ = db.Close() }, nil
}

// newRedis 构造 Redis 客户端（Round4 J5-5）：
//   - addr 必填校验，缺失报错带 env 名（仿 data.database.source 范本），
//     不再静默回退 localhost:6379 掩盖配置错误；
//   - DialTimeout 显式 5s：go-redis 默认 5s 依赖库默认值，这里显式化并
//     与 DB 建连超时对齐；Ping 已有独立 5s 上限兜底。
func newRedis(cfg *config.Redis) (*redis.Client, error) {
	addr := strings.TrimSpace(cfg.GetAddr())
	if addr == "" {
		return nil, fmt.Errorf("redis addr is empty: set data.redis.addr or TORCHWOOD_DATA_REDIS_ADDR")
	}
	return redis.NewClient(&redis.Options{
		Addr:        addr,
		Password:    cfg.GetPassword(),
		DB:          int(cfg.GetDb()),
		DialTimeout: 5 * time.Second,
		// PoolSize 保持 go-redis 默认（10*GOMAXPROCS）：限流/会话/频控等
		// 热路径均为短命令，默认池足够且避免与 DB 池参数耦合。显式注释
		// 防止后人误以为未配置是疏漏。
	}), nil
}

// normalizePoolSizes 将 pool 配置的 ≤0 值落为安全默认并 Warn：
//   - max_open ≤ 0 时取 4*GOMAXPROCS——database/sql 的 0 表示无上限，
//     长期运行下会积累远超实际需要的连接数，属于零值陷阱；
//   - max_idle ≤ 0 时取 max_open——database/sql 的 0 表示不保留空闲连接，
//     每次请求都重新建连，短查询场景性能劣化明显。
func normalizePoolSizes(maxOpen, maxIdle int, logger *slog.Logger) (int, int) {
	if logger == nil {
		logger = slog.Default()
	}
	if maxOpen <= 0 {
		logger.Warn("data.database.pool.max_open_conns <= 0, falling back to safe default",
			slog.Int("configured", maxOpen), slog.Int("effective", 4*runtime.GOMAXPROCS(0)))
		maxOpen = 4 * runtime.GOMAXPROCS(0)
	}
	if maxIdle <= 0 {
		logger.Warn("data.database.pool.max_idle_conns <= 0, falling back to safe default",
			slog.Int("configured", maxIdle), slog.Int("effective", maxOpen))
		maxIdle = maxOpen
	}
	return maxOpen, maxIdle
}
