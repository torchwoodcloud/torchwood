package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"google.golang.org/grpc"
)

func TestBuildMethodPolicies_EndUserRequiresUsersRole(t *testing.T) {
	t.Parallel()

	set, err := BuildMethodPolicies(clientv1.File_client_v1_groups_proto)
	require.NoError(t, err)

	p, ok := set.Get("/torchwood.client.v1.GroupsService/CreateGroup")
	require.True(t, ok, "GroupsService methods should be in the policy set")
	require.Equal(t, domainauth.AccessEndUser, p.Access)
	require.Equal(t, []string{"users"}, p.Permissions)
}

func TestBuildMethodPolicies_AccountPublicMethods(t *testing.T) {
	t.Parallel()

	set, err := BuildMethodPolicies(clientv1.File_client_v1_account_proto)
	require.NoError(t, err)

	p, ok := set.Get("/torchwood.client.v1.AccountService/SignIn")
	require.True(t, ok)
	require.Equal(t, domainauth.AccessPublic, p.Access)

	p, ok = set.Get("/torchwood.client.v1.AccountService/Me")
	require.True(t, ok)
	require.Equal(t, []string{"users"}, p.Permissions)
}

// fakeUnaryHandler 满足 grpc.MethodDesc 的 Handler 签名，仅用于注册测试服务。
func fakeUnaryHandler(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	return nil, nil
}

func newServerWithServices(serviceNames ...string) *grpc.Server {
	srv := grpc.NewServer()
	for _, name := range serviceNames {
		srv.RegisterService(&grpc.ServiceDesc{
			ServiceName: name,
			Methods:     []grpc.MethodDesc{{MethodName: "DoThing", Handler: fakeUnaryHandler}},
		}, nil)
	}
	return srv
}

func testPolicySet(policies ...domainauth.MethodPolicy) *domainauth.PolicySet {
	set, err := domainauth.NewPolicySet(policies)
	if err != nil {
		panic(err)
	}
	return set
}

func TestAssertRegisteredMethodsHaveAuthz(t *testing.T) {
	t.Parallel()

	t.Run("all methods covered", func(t *testing.T) {
		t.Parallel()
		srv := newServerWithServices("torchwood.test.v1.PublicService", "torchwood.test.v1.KeyService", "torchwood.test.v1.PermService")
		err := assertRegisteredMethodsHaveAuthz(srv, testPolicySet(
			domainauth.MethodPolicy{Method: "/torchwood.test.v1.PublicService/DoThing", Service: "/torchwood.test.v1.PublicService", Access: domainauth.AccessPublic},
			domainauth.MethodPolicy{Method: "/torchwood.test.v1.KeyService/DoThing", Service: "/torchwood.test.v1.KeyService", Access: domainauth.AccessServer, Scope: &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeRead}},
			domainauth.MethodPolicy{Method: "/torchwood.test.v1.PermService/DoThing", Service: "/torchwood.test.v1.PermService", Access: domainauth.AccessPermission, Permissions: []string{"users"}},
		))
		require.NoError(t, err)
	})

	t.Run("unannotated method fails closed", func(t *testing.T) {
		t.Parallel()
		srv := newServerWithServices("torchwood.test.v1.UnannotatedService")
		err := assertRegisteredMethodsHaveAuthz(srv, testPolicySet())
		require.Error(t, err)
		require.Contains(t, err.Error(), "/torchwood.test.v1.UnannotatedService/DoThing")
	})

	t.Run("framework services are exempt", func(t *testing.T) {
		t.Parallel()
		srv := newServerWithServices("grpc.health.v1.Health", "grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection")
		require.NoError(t, assertRegisteredMethodsHaveAuthz(srv, testPolicySet()))
	})
}
