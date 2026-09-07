package idgen_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/pkg/idgen"
)

func TestAPIKeySecret_Format(t *testing.T) {
	t.Parallel()
	secret := idgen.APIKeySecret()
	require.True(t, strings.HasPrefix(secret, idgen.APIKeySecretPrefix))
	// sk- + 两个 canonical UUID（36 字符×2）。
	require.Len(t, secret, len(idgen.APIKeySecretPrefix)+72)
	trimmed := strings.TrimPrefix(secret, idgen.APIKeySecretPrefix)
	for half := range 2 {
		u := trimmed[half*36 : (half+1)*36]
		for i := 0; i < len(u); i++ {
			switch i {
			case 8, 13, 18, 23:
				require.Equal(t, '-', rune(u[i]), "UUID 连字符位置错位: %s", u)
			default:
				require.True(t, (u[i] >= '0' && u[i] <= '9') || (u[i] >= 'a' && u[i] <= 'f'), "非小写 hex 字符: %c", u[i])
			}
		}
	}
}

func TestAPIKeySecret_Unique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		seen[idgen.APIKeySecret()] = struct{}{}
	}
	require.Len(t, seen, 1000)
}
