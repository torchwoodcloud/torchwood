package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func expiredBundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "old", RefreshToken: "refresh-1", ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))}
}

func freshBundle() *clientv1.TokenBundle {
	return &clientv1.TokenBundle{AccessToken: "fresh", RefreshToken: "refresh-1", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
}

func TestProactiveRefresh(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.Save(expiredBundle()))
	c, fake := newTestClient(t, WithTokenStore(store))
	fake.tokens = freshBundle()

	_, err := c.Account.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, int32(1), fake.refreshCalls.Load())
	auth := fake.lastAuth.Load().([]string)
	require.Equal(t, []string{"Bearer fresh"}, auth)
}

func TestNoProactiveRefreshWhenFresh(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.Save(freshBundle()))
	c, fake := newTestClient(t, WithTokenStore(store))

	_, err := c.Account.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, int32(0), fake.refreshCalls.Load())
	auth := fake.lastAuth.Load().([]string)
	require.Equal(t, []string{"Bearer fresh"}, auth)
}

func TestRetryOnUnauthorized(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.Save(freshBundle()))
	c, fake := newTestClient(t, WithTokenStore(store))
	fake.failFirstMe.Store(true)
	fake.tokens = &clientv1.TokenBundle{AccessToken: "rotated", RefreshToken: "refresh-1", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}

	me, err := c.Account.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, "acc-1", me.Id)
	require.Equal(t, int32(1), fake.refreshCalls.Load())
	require.Equal(t, int32(2), fake.meCalls.Load())
}

func TestRefreshUnauthenticatedClearsTokens(t *testing.T) {
	var cb *clientv1.TokenBundle
	store := NewMemoryTokenStore()
	bad := &clientv1.TokenBundle{AccessToken: "old", RefreshToken: "bad", ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))}
	require.NoError(t, store.Save(bad))
	c, _ := newTestClient(t, WithTokenStore(store), WithOnTokensChanged(func(b *clientv1.TokenBundle) { cb = b }))

	_, err := c.Account.Me(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	got, err := store.Load()
	require.NoError(t, err)
	require.Nil(t, got)
	require.Nil(t, cb)
}

func TestRefreshTemporaryErrorKeepsTokens(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.Save(expiredBundle()))
	c, fake := newTestClient(t, WithTokenStore(store))
	fake.refreshErr = status.Error(codes.Unavailable, "server down")

	_, err := c.Account.Me(context.Background())
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	got, err := store.Load()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "old", got.AccessToken)
}

func TestConcurrentRefreshDedup(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.Save(expiredBundle()))
	c, fake := newTestClient(t, WithTokenStore(store))
	fake.tokens = freshBundle()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Account.Me(context.Background())
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), fake.refreshCalls.Load())
}
