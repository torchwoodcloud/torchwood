package interceptor

import (
	"testing"

	"github.com/torchwooddev/torchwood/internal/domain/shared"
)

func TestPrincipalHasAnyRole_FailClosed(t *testing.T) {
	t.Parallel()
	p := &shared.Principal{
		Roles:       []string{"users", "user:abc"},
		Permissions: []string{"storage.read"},
	}
	if !p.HasAnyRole([]string{"users"}) {
		t.Fatal("expected users role to match")
	}
	if p.HasAnyRole([]string{"storage.read"}) {
		t.Fatal("scopes must not match HasAnyRole")
	}
	if p.HasAnyRole([]string{"admin"}) {
		t.Fatal("expected admin role to fail")
	}
	if p.HasAnyRole(nil) {
		t.Fatal("empty required roles must fail-closed")
	}
	if p.HasScope("users") {
		t.Fatal("roles must not match HasScope")
	}
	if !p.HasScope("storage.read") {
		t.Fatal("expected storage.read scope to match")
	}
}
