package shared

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRejectExternalDatabaseID(t *testing.T) {
	require.NoError(t, RejectExternalDatabaseID("default"))
	require.NoError(t, RejectExternalDatabaseID("app"))
	require.Equal(t, codes.InvalidArgument, status.Code(RejectExternalDatabaseID("")))
	require.Equal(t, ident.ErrInvalidSchemaResourceID.Error(), status.Convert(RejectExternalDatabaseID("")).Message())
	require.Equal(t, codes.InvalidArgument, status.Code(RejectExternalDatabaseID(ident.ProjectDataPlaneID)))
	require.Equal(t, codes.InvalidArgument, status.Code(RejectExternalDatabaseID("Bad-ID")))
}

func TestMapIdentError(t *testing.T) {
	require.Nil(t, MapIdentError(nil))
	mapped := MapIdentError(ident.ErrInvalidSchemaResourceID)
	require.Equal(t, codes.InvalidArgument, status.Code(mapped))
	other := errors.New("other")
	require.Equal(t, other, MapIdentError(other))
}
