package interceptor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type failingValidator struct{ err error }

func (f failingValidator) Authenticate(_ context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return nil, f.err
}

func (f failingValidator) ValidateToken(context.Context, string) (*shared.Principal, error) {
	return nil, f.err
}

func (f failingValidator) ValidateCredential(context.Context, string, shared.CredentialType) (*shared.Principal, error) {
	return nil, f.err
}

func (f failingValidator) ValidateAdminProjectAccess(context.Context, *shared.Principal) error {
	return nil
}

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func invokeAuth(ic *AuthInterceptor, ctx context.Context, method string) error {
	_, err := ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { return nil, nil })
	return err
}

func TestAuthInterceptor_LogsCredentialMissing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(stubValidator{}, nil, nil, nil)
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := contexts.WithClientInfo(
		metadata.NewIncomingContext(context.Background(), metadata.MD{}),
		contexts.ClientInfo{IP: "203.0.113.1", UserAgent: "agent/1.0"},
	)
	err = invokeAuth(ic, ctx, "/torchwood.server.v1.UsersService/ListUsers")
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"grpc auth rejected",
		"method=/torchwood.server.v1.UsersService/ListUsers",
		"reason=credential_missing",
		"ip=203.0.113.1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log output missing %q: %s", want, out)
		}
	}
}

func TestAuthInterceptor_LogsMultipleCredentials(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(stubValidator{}, nil, nil, nil)
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer tok",
		"x-api-key", "k",
	))
	err = invokeAuth(ic, ctx, "/torchwood.server.v1.UsersService/ListUsers")
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
	if !strings.Contains(buf.String(), "reason=multiple_credentials") {
		t.Fatalf("log output missing multiple_credentials: %s", buf.String())
	}
}

func TestAuthInterceptor_LogsInvalidAuthorization(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(stubValidator{}, nil, nil, nil)
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic abc"))
	err = invokeAuth(ic, ctx, "/torchwood.server.v1.UsersService/ListUsers")
	if st, _ := status.FromError(err); st.Code() != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
	if !strings.Contains(buf.String(), "reason=invalid_authorization") {
		t.Fatalf("log output missing invalid_authorization: %s", buf.String())
	}
}

func TestAuthInterceptor_LogsInvalidCredential(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(failingValidator{err: errors.New("bad key")}, nil, nil, nil)
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := contexts.WithClientInfo(
		metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "leaked-key")),
		contexts.ClientInfo{IP: "198.51.100.9"},
	)
	_ = invokeAuth(ic, ctx, "/torchwood.server.v1.UsersService/ListUsers")

	out := buf.String()
	if !strings.Contains(out, "reason=credential_invalid") || !strings.Contains(out, "credential_type=api_key") {
		t.Fatalf("unexpected log output: %s", out)
	}
	// 绝不记录凭证本体。
	if strings.Contains(out, "leaked-key") {
		t.Fatalf("log output leaked credential: %s", out)
	}
}

func TestAuthInterceptor_LogsPermissionDenied(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		Roles:          []string{"keys"},
		Permissions:    []string{"*"},
	}}, nil, []string{"/torchwood.server.v1.APIKeysService/CreateAPIKey"}, nil)
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "k"))
	_ = invokeAuth(ic, ctx, "/torchwood.server.v1.APIKeysService/CreateAPIKey")

	out := buf.String()
	if !strings.Contains(out, "reason=apikey_self_management_denied") {
		t.Fatalf("unexpected log output: %s", out)
	}
}

func TestAuthInterceptor_NoLogOnSuccess(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    "user-1",
		Roles:     []string{"users"},
	}}, nil, nil, map[string][]string{
		"/torchwood.client.v1.AccountService/Me": {"users"},
	})
	requireNoError(t, err)
	ic.WithLogger(captureLogger(&buf))

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer token"))
	if err := invokeAuth(ic, ctx, "/torchwood.client.v1.AccountService/Me"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log on success, got: %s", buf.String())
	}
}
