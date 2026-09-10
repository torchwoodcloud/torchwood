package messaging

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/torchwoodcloud/torchwood/pkg/config"
)

// MailerService delivers outbound email using SMTP or development logging.
type MailerService struct {
	smtpHost     string
	smtpPort     int
	smtpUser     string
	smtpPassword string
	from         string
	useTLS       bool
	devLogOTP    bool
}

func NewMailer(cfg *config.AppConfig) *MailerService {
	smtpCfg := cfg.GetMessaging().GetSmtp()
	svc := &MailerService{
		smtpHost:     strings.TrimSpace(smtpCfg.GetHost()),
		smtpPort:     smtpPort(smtpCfg),
		smtpUser:     smtpCfg.GetUsername(),
		smtpPassword: smtpCfg.GetPassword(),
		from:         smtpFrom(smtpCfg),
		useTLS:       smtpCfg.GetUseTls(),
		devLogOTP:    cfg.GetMessaging().GetDevLogOtp(),
	}
	return svc
}

func (m *MailerService) Send(ctx context.Context, to, subject, body string) error {
	if m.smtpHost != "" {
		return m.sendSMTP(to, subject, body)
	}
	if !m.devLogOTP {
		return fmt.Errorf("smtp is not configured")
	}
	// fail-closed：生产环境禁止把含验证码的邮件打到 stdout（日志可读 =
	// 任意邮箱 OTP 接管面）。宁可发送失败也不泄漏。
	if config.CurrentRuntimeEnv() == config.EnvProduction {
		return fmt.Errorf("dev_log_otp is forbidden in production: configure messaging.smtp or set messaging.dev_log_otp=false")
	}
	fmt.Printf("[Torchwood-dev-mailer] to=%s subject=%q body=%q\n", to, subject, body)
	return nil
}

func (m *MailerService) sendSMTP(to, subject, body string) error {
	if to == "" {
		return fmt.Errorf("smtp: recipient is required")
	}
	addr := fmt.Sprintf("%s:%d", m.smtpHost, m.smtpPort)
	msg := buildMessage(m.from, to, subject, body)
	if m.useTLS {
		return m.sendTLS(addr, to, msg)
	}
	var auth smtp.Auth
	if m.smtpUser != "" {
		auth = smtp.PlainAuth("", m.smtpUser, m.smtpPassword, m.smtpHost)
	}
	return smtp.SendMail(addr, auth, m.from, []string{to}, msg)
}

func (m *MailerService) sendTLS(addr, to string, msg []byte) error {
	tlsCfg := &tls.Config{ServerName: m.smtpHost, MinVersion: tls.VersionTLS12}
	conn, err := tls.DialWithDialer(&net.Dialer{}, "tcp", addr, tlsCfg)
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, m.smtpHost)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if m.smtpUser != "" {
		if err := client.Auth(smtp.PlainAuth("", m.smtpUser, m.smtpPassword, m.smtpHost)); err != nil {
			return err
		}
	}
	if err := client.Mail(m.from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

func smtpPort(cfg *config.Messaging_SMTP) int {
	if cfg.GetPort() > 0 {
		return int(cfg.GetPort())
	}
	return 587
}

func smtpFrom(cfg *config.Messaging_SMTP) string {
	if from := strings.TrimSpace(cfg.GetFrom()); from != "" {
		return from
	}
	return "noreply@torchwood.local"
}

func buildMessage(from, to, subject, body string) []byte {
	headers := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}
	return []byte(strings.Join(headers, "\r\n"))
}
