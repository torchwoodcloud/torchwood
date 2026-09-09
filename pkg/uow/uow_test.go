package uow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
)

type stubRunner struct {
	calls int
}

func (s *stubRunner) Run(ctx context.Context, fn func(context.Context) error) error {
	s.calls++
	return fn(ctx)
}

func TestRunnerRunsFn(t *testing.T) {
	r := &stubRunner{}
	var ran bool
	err := r.Run(context.Background(), func(context.Context) error {
		ran = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, ran)
	require.Equal(t, 1, r.calls)

	want := errors.New("boom")
	err = r.Run(context.Background(), func(context.Context) error { return want })
	require.ErrorIs(t, err, want)

	var _ uow.Runner = r
}
