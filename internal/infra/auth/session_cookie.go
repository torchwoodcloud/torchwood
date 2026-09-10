package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
)

// SessionCookieCodec signs and verifies opaque session cookie values.
// A valid value has the form "base64(projectID:sessionID):signature".
type SessionCookieCodec struct {
	secret []byte
}

func NewSessionCookieCodec(secret string) *SessionCookieCodec {
	return &SessionCookieCodec{secret: []byte(secret)}
}

func (c *SessionCookieCodec) Sign(projectID, sessionID string) string {
	payload := base64.URLEncoding.EncodeToString([]byte(projectID + ":" + sessionID))
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + ":" + sig
}

func (c *SessionCookieCodec) Verify(token string) (projectID, sessionID string, err error) {
	parts := strings.Split(token, ":")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid session cookie format")
	}
	payload, sigHex := parts[0], parts[1]
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sigHex)) {
		return "", "", fmt.Errorf("invalid session cookie signature")
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", err
	}
	inner := string(decoded)
	projectID, sessionID, ok := strings.Cut(inner, ":")
	if !ok {
		return "", "", fmt.Errorf("invalid session cookie payload")
	}
	return projectID, sessionID, nil
}

// NewSessionCookieVerifier 提供 domainauth.SessionCookieVerifier（M5 C5）：
// 密钥派生与 SessionService/Validator 的会话 cookie 编解码同源
// （HMAC-SHA256(jwt.secret, purpose=session-cookie)）。
func NewSessionCookieVerifier(cfg *config.AppConfig) domainauth.SessionCookieVerifier {
	return NewSessionCookieCodec(string(jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeSessionCookie)))
}
