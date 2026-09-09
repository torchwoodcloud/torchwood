package idgen_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

func TestSnowflake_Uniqueness(t *testing.T) {
	t.Parallel()
	sf, err := idgen.NewSnowflake(1)
	require.NoError(t, err)
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		v := sf.NextString()
		require.NotEmpty(t, v)
		require.NotContains(t, seen, v)
		seen[v] = struct{}{}
	}
}

func TestULID_Format(t *testing.T) {
	t.Parallel()
	s := idgen.ULID().String()
	require.Len(t, s, 26)
}

func TestNormalizeStrategy(t *testing.T) {
	t.Parallel()
	require.Equal(t, idgen.StrategyUUID, idgen.NormalizeStrategy(""))
	require.Equal(t, idgen.StrategyULID, idgen.NormalizeStrategy("ULID"))
	require.Equal(t, idgen.StrategySnowflake, idgen.NormalizeStrategy("SNOWFLAKE"))
	require.Equal(t, idgen.StrategyRandom, idgen.NormalizeStrategy("random"))
}
