package client

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 回归测试：completeOAuth2Code 失败时 result 为 nil，
// HandleOAuth2Callback 不得解引用空指针（曾导致可远程触发的 panic）。
func TestHandleOAuth2Callback_NilResultOnError(t *testing.T) {
	t.Parallel()

	// oauthState 为 nil 时 completeOAuth2Code 直接返回 nil result + error。
	a := &Account{}
	result, err := a.HandleOAuth2Callback(context.Background(), "wechat", "code", "state", nil, nil)
	if err == nil {
		t.Fatal("expected error when oauth2 is not configured")
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented, got %v", status.Code(err))
	}
	if result == nil {
		t.Fatal("expected non-nil callback result for redirect")
	}
	if result.FailureURL != "/" {
		t.Fatalf("expected fallback failure URL '/', got %q", result.FailureURL)
	}
	if result.RedirectURL == "" {
		t.Fatal("expected redirect URL carrying the error")
	}
}
