package idgen_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

func TestRandomString_NumericLength(t *testing.T) {
	t.Parallel()
	s, err := idgen.RandomString(idgen.RandomConfig{Length: 10, Charset: idgen.RandomCharsetNumeric})
	require.NoError(t, err)
	require.Len(t, s, 10)
	for _, ch := range s {
		require.True(t, ch >= '0' && ch <= '9')
	}
}

func TestRandomString_Alphanumeric(t *testing.T) {
	t.Parallel()
	s, err := idgen.RandomString(idgen.RandomConfig{Length: 12, Charset: idgen.RandomCharsetAlphanumeric})
	require.NoError(t, err)
	require.Len(t, s, 12)
}
