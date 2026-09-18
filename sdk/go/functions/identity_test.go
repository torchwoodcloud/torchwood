package functions

import (
	"context"
	"testing"
)

func TestIdentityRoundTrip(t *testing.T) {
	want := Identity{
		ExecutionToken: "twx_abc",
		APIBaseURL:     "https://api.example.com",
		ExecutionID:    "exec-42",
		Source:         "cron:nightly",
		InvokingUserID: "user-1",
		ProjectID:      "proj-1",
	}
	ctx := WithIdentity(context.Background(), want)
	if got := FromContext(ctx); got != want {
		t.Fatalf("FromContext = %+v, want %+v", got, want)
	}
	// 嵌套注入取最近一层。
	inner := Identity{Source: "client"}
	if got := FromContext(WithIdentity(ctx, inner)); got != inner {
		t.Fatalf("FromContext(inner) = %+v, want %+v", got, inner)
	}
}

func TestFromContextWithoutIdentity(t *testing.T) {
	if got := FromContext(context.Background()); got != (Identity{}) {
		t.Fatalf("FromContext = %+v, want zero Identity", got)
	}
	if got := FromContext(nil); got != (Identity{}) {
		t.Fatalf("FromContext(nil) = %+v, want zero Identity", got)
	}
}
