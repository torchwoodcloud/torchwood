package projects

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMatchRedirectURL(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"https://app.example.com/callback",
		"http://localhost:5173",
	}
	require.True(t, MatchRedirectURL("https://app.example.com/callback?x=1", allowed))
	require.True(t, MatchRedirectURL("http://localhost:5173/oauth", allowed))
	require.False(t, MatchRedirectURL("https://evil.example.com/callback", allowed))
}

func TestOAuthAllowedRedirectURLs(t *testing.T) {
	t.Parallel()
	settings := map[string]any{
		SettingsKeyOAuthAllowedRedirectURLs: []any{"https://app.example.com"},
	}
	require.Equal(t, []string{"https://app.example.com"}, OAuthAllowedRedirectURLs(settings))
}

func TestValidateAllowlistEntry(t *testing.T) {
	t.Parallel()
	valid := []string{
		"https://app.example.com",
		"https://app.example.com/auth/callback",
		"http://localhost:5173",
		"https://app.example.com:8443/cb",
	}
	for _, entry := range valid {
		require.NoError(t, ValidateAllowlistEntry(entry), entry)
	}

	invalid := map[string]string{
		"":                      "entry is required",
		"   ":                   "entry is required",
		"app.example.com":       "must use http or https",
		"ftp://app.example.com": "must use http or https",
		"https://":              "host is required",
		"file:///etc/passwd":    "must use http or https",
		strings.Repeat("a", MaxAllowlistEntryLen+1): "at most",
	}
	for entry, wantErr := range invalid {
		require.ErrorContains(t, ValidateAllowlistEntry(entry), wantErr, entry)
	}
}
