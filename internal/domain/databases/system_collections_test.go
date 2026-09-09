package databases

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

func TestIsSystemCollection_Sentinel(t *testing.T) {
	require.Equal(t, ident.ProjectDataPlaneID, SystemDatabaseID)
	require.True(t, IsSystemCollection("shop", ident.ProjectDataPlaneID, "users"))
	require.True(t, IsSystemCollection("shop", SystemDatabaseID, "sessions"))
	require.False(t, IsSystemCollection("shop", "default", "users"))
	require.False(t, IsSystemCollection("shop", "app", "users"))
	require.False(t, IsSystemCollection("shop", ident.ProjectDataPlaneID, "posts"))
	require.False(t, IsSystemCollection("shop", "default", "posts"))
}

func TestIsSystemCollectionID(t *testing.T) {
	require.True(t, IsSystemCollectionID("users"))
	require.True(t, IsSystemCollectionID("files"))
	require.False(t, IsSystemCollectionID("posts"))
}
