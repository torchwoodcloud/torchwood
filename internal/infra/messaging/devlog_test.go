package messaging

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

// withEnv 设置 TORCHWOOD_ENV（t.Setenv 自动还原）。
func withEnv(t *testing.T, env string) {
	t.Helper()
	t.Setenv(config.EnvVarRuntime, env)
}

// 2026-08 评审 P1-3：生产环境禁止 dev 日志通道输出验证码（fail-closed）。
func TestMailer_DevLogForbiddenInProduction(t *testing.T) {
	withEnv(t, "production")
	cfg := &config.AppConfig{Messaging: &config.Messaging{
		Smtp:      &config.Messaging_SMTP{Host: ""},
		DevLogOtp: true,
	}}
	m := NewMailer(cfg)
	err := m.Send(context.Background(), "user@example.com", "code", "your code is 123456")
	require.Error(t, err)
	require.ErrorContains(t, err, "forbidden in production")
}

func TestMailer_DevLogAllowedInDevelopment(t *testing.T) {
	withEnv(t, "development")
	cfg := &config.AppConfig{Messaging: &config.Messaging{
		Smtp:      &config.Messaging_SMTP{Host: ""},
		DevLogOtp: true,
	}}
	m := NewMailer(cfg)
	require.NoError(t, m.Send(context.Background(), "user@example.com", "code", "your code is 123456"))
}

func TestMailer_NoDevLogUnconfigured(t *testing.T) {
	withEnv(t, "development")
	cfg := &config.AppConfig{Messaging: &config.Messaging{
		Smtp:      &config.Messaging_SMTP{Host: ""},
		DevLogOtp: false,
	}}
	m := NewMailer(cfg)
	err := m.Send(context.Background(), "user@example.com", "code", "body")
	require.ErrorContains(t, err, "smtp is not configured")
}

func TestSMS_DevLogForbiddenInProduction(t *testing.T) {
	withEnv(t, "production")
	cfg := &config.AppConfig{Messaging: &config.Messaging{
		Sms:       &config.SMS{Provider: ""},
		DevLogSms: true,
	}}
	s := NewSMSService(cfg)
	err := s.Send(context.Background(), "+10000000000", "your code is 123456")
	require.Error(t, err)
	require.ErrorContains(t, err, "forbidden in production")
}

func TestSMS_DevLogAllowedInDevelopment(t *testing.T) {
	withEnv(t, "development")
	cfg := &config.AppConfig{Messaging: &config.Messaging{
		Sms:       &config.SMS{Provider: ""},
		DevLogSms: true,
	}}
	s := NewSMSService(cfg)
	require.NoError(t, s.Send(context.Background(), "+10000000000", "your code is 123456"))
}
