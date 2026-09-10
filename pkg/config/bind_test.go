package config

import (
	"strings"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

const testYAML = `
server:
  grpc:
    addr: ":9090"
    timeout: "30s"
  http:
    addr: ":8080"
    cors:
      allow_origins: ["http://localhost:3000"]
      allow_credentials: true
      max_age: 600
  metrics:
    addr: ":9100"
security:
  jwt:
    secret: test-secret
    access_ttl: 15m
  api_key:
    header: X-API-Key
  trusted_proxies: ["127.0.0.1/32"]
  sessions:
    max_per_user: 50
data:
  database:
    source: postgres://localhost:5432/torchwood
    debug: true
    pool:
      max_idle_conns: 5
      conn_max_lifetime: 30m
  redis:
    addr: 127.0.0.1:6379
    db: 2
storage:
  provider: s3
  s3:
    endpoint: http://minio:9000
    region: us-east-1
    bucket: torchwood
    access_key_id: minioadmin
    secret_access_key: minioadmin
    use_ssl: false
  local:
    path: ./data
functions:
  executor: docker
  docker:
    host: unix:///var/run/docker.sock
telemetry:
  enabled: true
  otlp_endpoint: http://otel:4317
  service_name: torchwood
messaging:
  smtp:
    host: smtp.example.com
    port: 587
    username: user
    from: no-reply@example.com
    use_tls: true
  dev_log_otp: true
  sms:
    provider: twilio
    twilio:
      account_sid: AC123
      auth_token: tok
  dev_log_sms: false
idgen:
  default_strategy: snowflake
  random:
    length: 12
    charset: alphanumeric
    redis_key_prefix: id:rnd
    max_retries: 5
  snowflake:
    node_id: 3
  sequence:
    redis_key_prefix: id:seq
  resources:
    users: "user:{id}"
`

func newTestConfig(t *testing.T, yaml string) lynx.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(yaml)))
	return lynx.NewViperConfig(v)
}

func TestUnmarshalConfig(t *testing.T) {
	cfg := newTestConfig(t, testYAML)
	var out AppConfig
	require.NoError(t, UnmarshalConfig(cfg, &out))

	require.NotNil(t, out.GetServer().GetGrpc())
	require.Equal(t, ":9090", out.GetServer().GetGrpc().GetAddr())
	require.Equal(t, "30s", out.GetServer().GetGrpc().GetTimeout())
	require.Equal(t, ":8080", out.GetServer().GetHttp().GetAddr())
	require.Equal(t, []string{"http://localhost:3000"}, out.GetServer().GetHttp().GetCors().GetAllowOrigins())
	require.True(t, out.GetServer().GetHttp().GetCors().GetAllowCredentials())
	require.EqualValues(t, 600, out.GetServer().GetHttp().GetCors().GetMaxAge())
	require.Equal(t, ":9100", out.GetServer().GetMetrics().GetAddr())

	require.Equal(t, "test-secret", out.GetSecurity().GetJwt().GetSecret())
	require.Equal(t, "15m", out.GetSecurity().GetJwt().GetAccessTtl())
	require.Equal(t, "X-API-Key", out.GetSecurity().GetApiKey().GetHeader())
	require.Equal(t, []string{"127.0.0.1/32"}, out.GetSecurity().GetTrustedProxies())
	require.EqualValues(t, 50, out.GetSecurity().GetSessions().GetMaxPerUser())

	require.Equal(t, "postgres://localhost:5432/torchwood", out.GetData().GetDatabase().GetSource())
	require.True(t, out.GetData().GetDatabase().GetDebug())
	require.EqualValues(t, 5, out.GetData().GetDatabase().GetPool().GetMaxIdleConns())
	require.Equal(t, "30m", out.GetData().GetDatabase().GetPool().GetConnMaxLifetime())
	require.Equal(t, "127.0.0.1:6379", out.GetData().GetRedis().GetAddr())
	require.EqualValues(t, 2, out.GetData().GetRedis().GetDb())

	require.Equal(t, "s3", out.GetStorage().GetProvider())
	require.Equal(t, "minioadmin", out.GetStorage().GetS3().GetAccessKeyId())
	require.Equal(t, "minioadmin", out.GetStorage().GetS3().GetSecretAccessKey())
	require.False(t, out.GetStorage().GetS3().GetUseSsl())
	require.Equal(t, "./data", out.GetStorage().GetLocal().GetPath())

	require.Equal(t, "docker", out.GetFunctions().GetExecutor())
	require.Equal(t, "unix:///var/run/docker.sock", out.GetFunctions().GetDocker().GetHost())

	require.True(t, out.GetTelemetry().GetEnabled())
	require.Equal(t, "http://otel:4317", out.GetTelemetry().GetOtlpEndpoint())
	require.Equal(t, "torchwood", out.GetTelemetry().GetServiceName())

	require.EqualValues(t, 587, out.GetMessaging().GetSmtp().GetPort())
	require.True(t, out.GetMessaging().GetSmtp().GetUseTls())
	require.True(t, out.GetMessaging().GetDevLogOtp())
	require.Equal(t, "AC123", out.GetMessaging().GetSms().GetTwilio().GetAccountSid())
	require.False(t, out.GetMessaging().GetDevLogSms())

	require.Equal(t, "snowflake", out.GetIdgen().GetDefaultStrategy())
	require.EqualValues(t, 12, out.GetIdgen().GetRandom().GetLength())
	require.Equal(t, "alphanumeric", out.GetIdgen().GetRandom().GetCharset())
	require.EqualValues(t, 3, out.GetIdgen().GetSnowflake().GetNodeId())
	require.Equal(t, "id:seq", out.GetIdgen().GetSequence().GetRedisKeyPrefix())
	require.Equal(t, "user:{id}", out.GetIdgen().GetResources().GetUsers())
}

func TestConfigureViperEnvBinding(t *testing.T) {
	v := viper.New()
	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f.String("config-dir", "./testdata", "config file path")
	f.String("log-level", "info", "log level")

	t.Setenv("TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID", "env-access-key")
	t.Setenv("TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY", "")
	t.Setenv("TORCHWOOD_SERVER_GRPC_ADDR", ":10001")
	t.Setenv("TORCHWOOD_SECURITY_TRUSTED_PROXIES", "10.0.0.0/8,192.168.0.0/16")
	t.Setenv("TORCHWOOD_SECURITY_SESSIONS_MAX_PER_USER", "3")

	require.NoError(t, ConfigureViper(f, lynx.NewViperConfig(v)))

	require.Equal(t, ":10001", v.GetString("server.grpc.addr"))
	require.Equal(t, "env-access-key", v.GetString("storage.s3.access_key_id"))
	require.Equal(t, "10.0.0.0/8,192.168.0.0/16", v.GetString("security.trusted_proxies"))
	require.Empty(t, v.GetString("storage.s3.secret_access_key"))
	require.Equal(t, 3, v.GetInt("security.sessions.max_per_user"))
}

func TestConfigureViperEnvBindingUnmarshal(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(testYAML)))
	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f.String("config-dir", "./testdata", "config file path")

	t.Setenv("TORCHWOOD_SERVER_GRPC_ADDR", ":10001")
	t.Setenv("TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID", "env-access-key")
	t.Setenv("TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("TORCHWOOD_SECURITY_TRUSTED_PROXIES", "10.0.0.0/8,192.168.0.0/16")

	require.NoError(t, ConfigureViper(f, lynx.NewViperConfig(v)))

	var out AppConfig
	require.NoError(t, UnmarshalConfig(lynx.NewViperConfig(v), &out))
	require.Equal(t, ":10001", out.GetServer().GetGrpc().GetAddr())
	require.Equal(t, "env-access-key", out.GetStorage().GetS3().GetAccessKeyId())
	require.Equal(t, "env-secret", out.GetStorage().GetS3().GetSecretAccessKey())
	require.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, out.GetSecurity().GetTrustedProxies())
}

func TestEnvNameForKey(t *testing.T) {
	require.Equal(t, "TORCHWOOD_DATA_DATABASE_SOURCE", envNameForKey("data.database.source"))
	require.Equal(t, "TORCHWOOD_SECURITY_JWT_SECRET", envNameForKey("security.jwt.secret"))
}

// TestRateLimitBinding：security.rate_limit 的 YAML 解码（optional bool 的
// 显式 presence）与 TORCHWOOD_SECURITY_RATE_LIMIT_* 环境变量覆盖。
func TestRateLimitBinding(t *testing.T) {
	yaml := `
security:
  rate_limit:
    enabled: false
    ip:
      limit: 100
      window: "30s"
`
	cfg := newTestConfig(t, yaml)
	var out AppConfig
	require.NoError(t, UnmarshalConfig(cfg, &out))
	rl := out.GetSecurity().GetRateLimit()
	require.NotNil(t, rl)
	require.NotNil(t, rl.Enabled, "explicit false must set presence")
	require.False(t, rl.GetEnabled())
	require.EqualValues(t, 100, rl.GetIp().GetLimit())
	require.Equal(t, "30s", rl.GetIp().GetWindow())
	// 未配置的维度解码为零值结构（limit=0/window=""），由拦截器回落默认值。
	require.Zero(t, rl.GetUser().GetLimit())
	require.Empty(t, rl.GetUser().GetWindow())

	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(yaml)))
	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f.String("config-dir", "./testdata", "config file path")

	t.Setenv("TORCHWOOD_SECURITY_RATE_LIMIT_ENABLED", "true")
	t.Setenv("TORCHWOOD_SECURITY_RATE_LIMIT_IP_LIMIT", "42")
	t.Setenv("TORCHWOOD_SECURITY_RATE_LIMIT_API_KEY_WINDOW", "10s")

	require.NoError(t, ConfigureViper(f, lynx.NewViperConfig(v)))
	var envOut AppConfig
	require.NoError(t, UnmarshalConfig(lynx.NewViperConfig(v), &envOut))
	envRL := envOut.GetSecurity().GetRateLimit()
	require.NotNil(t, envRL)
	require.True(t, envRL.GetEnabled())
	require.EqualValues(t, 42, envRL.GetIp().GetLimit())
	require.Equal(t, "10s", envRL.GetApiKey().GetWindow())
}
