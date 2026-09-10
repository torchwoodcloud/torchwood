package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRuntimeEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want RuntimeEnv
	}{
		{raw: "", want: EnvProduction},
		{raw: "production", want: EnvProduction},
		{raw: "PROD", want: EnvProduction},
		{raw: " staging ", want: EnvProduction},
		{raw: "unknown", want: EnvProduction},
		{raw: "development", want: EnvDevelopment},
		{raw: "DEV", want: EnvDevelopment},
		{raw: "local", want: EnvDevelopment},
		{raw: "test", want: EnvDevelopment},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, ParseRuntimeEnv(tc.raw))
		})
	}
}

func TestDrainTimeoutForEnv(t *testing.T) {
	t.Parallel()
	require.Equal(t, time.Duration(0), DrainTimeoutFor(EnvDevelopment, ""))
	require.Equal(t, 30*time.Second, DrainTimeoutFor(EnvProduction, ""))
}

func TestDrainTimeoutOverride(t *testing.T) {
	t.Parallel()
	require.Equal(t, 5*time.Second, DrainTimeoutFor(EnvDevelopment, "5s"))
	require.Equal(t, time.Duration(0), DrainTimeoutFor(EnvProduction, "0"))
	require.Equal(t, time.Duration(0), DrainTimeoutFor(EnvProduction, "0s"))
	// 非法或负值忽略，回退到环境默认。
	require.Equal(t, time.Duration(0), DrainTimeoutFor(EnvDevelopment, "nope"))
	require.Equal(t, 30*time.Second, DrainTimeoutFor(EnvProduction, "-1s"))
}

func TestCurrentDrainTimeoutReadsEnv(t *testing.T) {
	t.Setenv(EnvVarRuntime, "development")
	t.Setenv(EnvVarDrainTimeout, "")
	require.Equal(t, EnvDevelopment, CurrentRuntimeEnv())
	require.Equal(t, time.Duration(0), CurrentDrainTimeout())

	t.Setenv(EnvVarRuntime, "production")
	require.Equal(t, 30*time.Second, CurrentDrainTimeout())

	t.Setenv(EnvVarDrainTimeout, "2s")
	require.Equal(t, 2*time.Second, CurrentDrainTimeout())
}
