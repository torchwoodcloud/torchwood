package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/lynxtest"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// L2 装配测试的固定回环端口。gRPC 必须固定（gateway 按配置地址惰性拨号，
// 无法用 :0）；http/debug 固定是为了让测试直接探测。冲突面 = 仅本测试
// 自身（同包用例串行、其他包不绑这些端口），跑满 -p 4 亦安全。
const (
	l2GRPCAddr  = "127.0.0.1:19091"
	l2HTTPAddr  = "127.0.0.1:19092"
	l2DebugAddr = "127.0.0.1:19094"
)

// TestServerAssemblyL2 用与生产 main 完全相同的 setupApp 在测试进程内拉起
// 整机（lynxtest L2）：Wire 全量装配 + grpc/gateway/realtime/metrics/debug
// 五服务 + OnPreStart 钩子，配置经 lynxtest WithConfigMap 注入指向隔离测试
// 库与 miniredis。断言三件事：
//
//  1. gateway readiness（/healthz）就绪——聚合了 DB/Redis 健康检查；
//  2. gateway → gRPC → app → DB 全链路（GET /v1/console/auth/setup-status，
//     公开端点，读 admins 表）；
//  3. debug 诊断面（/healthz + /version）。
//
// t.Cleanup 走生产同源关停序列（排水 → 逆序 Stop → OnPostStop 关连接池），
// 顺带验证整机优雅关停不挂死。
//
// 集成测试：需要 .env（mise exec / mise run test）提供 TORCHWOOD_TEST_* DSN。
func TestServerAssemblyL2(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if testutil.AdminDSN() == "" {
		t.Skip("TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE is not set (run via `mise run test`, which loads .env)")
	}

	dsn, _ := testutil.SetupTestDBDSN(t)
	redis := miniredis.RunT(t)

	// readiness 聚合 postgres/redis/minio 三依赖（health.NewCheckers），
	// MinIO 指向本地 compose 实例（docker:up，与 PG/Redis 同为集成前置）；
	// 凭据经 .env（mise exec 加载），端点缺省 127.0.0.1:9000。
	app := lynxtest.Run(t, setupApp,
		lynxtest.WithConfigMap(map[string]any{
			"server.grpc.addr":             l2GRPCAddr,
			"server.http.addr":             l2HTTPAddr,
			"server.metrics.addr":          "127.0.0.1:0",
			"server.debug.addr":            l2DebugAddr,
			"security.jwt.secret":          l2RandomSecret(t),
			"data.database.source":         dsn,
			"data.redis.addr":              redis.Addr(),
			"storage.provider":             "s3",
			"storage.s3.endpoint":          l2EnvOr("TORCHWOOD_STORAGE_S3_ENDPOINT", "127.0.0.1:9000"),
			"storage.s3.access_key_id":     l2EnvOr("TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID", ""),
			"storage.s3.secret_access_key": l2EnvOr("TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY", ""),
			"storage.s3.use_ssl":           false,
			"functions.dispatcher.url":     "http://127.0.0.1:19099",
		}),
		// 真实 DB/Redis 连接池在 OnPostStop 关闭：放宽 lynxtest 1s 基线，
		// 避免清理被 CleanupTimeout 截断（关停序列本身仍受 30s 上限保护）。
		lynxtest.WithOptions(
			lynx.WithStopTimeout(15*time.Second),
			lynx.WithShutdownTimeout(15*time.Second),
			lynx.WithCleanupTimeout(15*time.Second),
		),
	)

	// lynxhttp 内置健康端点：<prefix>/liveness（不消费检查器）与
	// <prefix>/readiness（聚合检查器）。readiness 聚合 DB/Redis/MinIO 检查：
	// 200 即整套 infra 已连通。启动含 Wire 全量构造与 OnPreStart 钩子
	//（grants/scale/schema reconcile 扫 catalog），首次冷跑给足预算。
	l2 := l2Client{t: t}
	l2.EventuallyOK(l2HTTPAddr+"/healthz/liveness", 90*time.Second, "gateway liveness")
	l2.EventuallyOK(l2HTTPAddr+"/healthz/readiness", 90*time.Second, "gateway readiness")

	// gateway → gRPC → app → DB 全链路：公开端点读 admins 表，隔离库为空
	// → needs_setup=true；未配置 setup_token → setup_token_required=false。
	var setup struct {
		NeedsSetup         bool `json:"needs_setup"`
		SetupTokenRequired bool `json:"setup_token_required"`
	}
	l2.EventuallyJSON(l2HTTPAddr+"/v1/console/auth/setup-status", 30*time.Second, &setup)
	require.True(t, setup.NeedsSetup, "fresh test DB should need setup")
	require.False(t, setup.SetupTokenRequired)

	// debug 诊断面：/healthz 探活 + /version 构建信息（测试构建无 ldflags，
	// 包缺省值 dev/unknown 叠加 Go/OS/Arch 仍应完整输出）。
	l2.EventuallyOK(l2DebugAddr+"/healthz", 10*time.Second, "debug healthz")
	var ver map[string]any
	l2.EventuallyJSON(l2DebugAddr+"/version", 10*time.Second, &ver)
	require.Equal(t, "dev", ver["version"])
	require.NotEmpty(t, ver["go"])
	require.NotEmpty(t, ver["os"])
	require.NotEmpty(t, ver["arch"])

	// app.Run 留在后台 serving；t.Cleanup（lynxtest 注册）会走生产关停序列，
	// 这里显式等待一遍确保 Exited 语义在正常路径外仍可用（不触发提前退出）。
	select {
	case <-app.Exited():
		t.Fatalf("app exited while serving: %v", app.Err())
	default:
	}
}

// l2Client 收敛整机测试的 HTTP 探测：EventuallyOK 轮询状态码，
// EventuallyJSON 轮询直至响应可解码为目标结构。
type l2Client struct {
	t *testing.T
}

func (c *l2Client) get(url string) (int, []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+url, nil)
	if err != nil {
		return 0, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

func (c *l2Client) EventuallyOK(url string, timeout time.Duration, what string) {
	c.t.Helper()
	var status int
	testutil.Eventually(c.t, timeout, func() bool {
		status, _ = c.get(url)
		return status == http.StatusOK
	})
}

func (c *l2Client) EventuallyJSON(url string, timeout time.Duration, out any) {
	c.t.Helper()
	var body []byte
	testutil.Eventually(c.t, timeout, func() bool {
		status, b := c.get(url)
		if status != http.StatusOK {
			return false
		}
		body = b
		return json.Unmarshal(body, out) == nil
	})
}

// l2RandomSecret 生成不含弱子串黑名单词的随机密钥（bootkit
// ValidateSecret：≥32 字节 + WeakSecretTokens 子串拒绝）。
func l2RandomSecret(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return hex.EncodeToString(buf)
}

// l2EnvOr 读取环境变量（.env 经 mise exec 加载），空值回落缺省。
func l2EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
