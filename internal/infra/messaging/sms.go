package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// SMSService delivers outbound SMS using Twilio or development logging.
type SMSService struct {
	provider   string
	accountSID string
	authToken  string
	from       string
	devLogSMS  bool
	httpClient *http.Client
}

func NewSMSService(cfg *config.AppConfig) *SMSService {
	smsCfg := cfg.GetMessaging().GetSms()
	return &SMSService{
		provider:   strings.ToLower(strings.TrimSpace(smsCfg.GetProvider())),
		accountSID: smsCfg.GetTwilio().GetAccountSid(),
		authToken:  smsCfg.GetTwilio().GetAuthToken(),
		from:       smsCfg.GetTwilio().GetFrom(),
		devLogSMS:  cfg.GetMessaging().GetDevLogSms(),
		httpClient: http.DefaultClient,
	}
}

func (s *SMSService) Send(ctx context.Context, to, body string) error {
	switch s.provider {
	case "twilio":
		return s.sendTwilio(ctx, to, body)
	default:
		if !s.devLogSMS {
			return fmt.Errorf("sms provider is not configured")
		}
		// fail-closed：生产环境禁止把含验证码的短信打到 stdout（同 mailer）。
		if config.CurrentRuntimeEnv() == config.EnvProduction {
			return fmt.Errorf("dev_log_sms is forbidden in production: configure messaging.sms.provider or set messaging.dev_log_sms=false")
		}
		fmt.Printf("[Torchwood-dev-sms] to=%s body=%q\n", to, body)
		return nil
	}
}

func (s *SMSService) sendTwilio(ctx context.Context, to, body string) error {
	if s.accountSID == "" || s.authToken == "" || s.from == "" {
		return fmt.Errorf("twilio sms is not configured")
	}
	endpoint := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", s.accountSID)
	form := url.Values{}
	form.Set("To", to)
	form.Set("From", s.from)
	form.Set("Body", body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.accountSID, s.authToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if msg, ok := payload["message"].(string); ok && msg != "" {
			return fmt.Errorf("twilio sms failed: %s", msg)
		}
		return fmt.Errorf("twilio sms failed: status %d", resp.StatusCode)
	}
	return nil
}
