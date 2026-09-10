package servergrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/infra/health"
	"github.com/torchwoodcloud/torchwood/pkg/buildinfo"
)

func fakeCheckers(results map[string]error) *health.Checkers {
	deps := make([]*health.DependencyChecker, 0, len(results))
	for name, err := range results {
		err := err
		deps = append(deps, &health.DependencyChecker{Name: name, Check: func(ctx context.Context) error { return err }})
	}
	return health.NewCheckersFromDeps(deps...)
}

func TestHealthService_CheckAllOK(t *testing.T) {
	s := NewHealthService(fakeCheckers(map[string]error{"postgres": nil, "redis": nil}), buildinfo.BuildInfo{})
	resp, err := s.Check(context.Background(), &serverv1.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.GetStatus())
	require.Len(t, resp.GetDependencies(), 2)
	for _, d := range resp.GetDependencies() {
		require.Equal(t, "ok", d.GetStatus())
	}
}

func TestHealthService_CheckUnavailable(t *testing.T) {
	s := NewHealthService(fakeCheckers(map[string]error{
		"postgres": nil,
		"redis":    errors.New("connection refused"),
	}), buildinfo.BuildInfo{})
	resp, err := s.Check(context.Background(), &serverv1.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, "unavailable", resp.GetStatus())

	var found bool
	for _, d := range resp.GetDependencies() {
		if d.GetName() == "redis" {
			found = true
			require.Equal(t, "unavailable", d.GetStatus())
			require.Equal(t, "connection refused", d.GetError())
		}
	}
	require.True(t, found)
}

func TestHealthService_GetVersion(t *testing.T) {
	s := NewHealthService(fakeCheckers(map[string]error{}), buildinfo.BuildInfo{Version: "v1.2.3", Commit: "abc123", Date: "20260809"})
	resp, err := s.GetVersion(context.Background(), &serverv1.GetVersionRequest{})
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", resp.GetVersion())
	require.Equal(t, "abc123", resp.GetCommit())
	require.Equal(t, "20260809", resp.GetDate())
}
