package runtime

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/principal"
	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// 执行侧迁移验收（MIGRATION §6「streaming 方法 open-time 授权通过；per-message
// 需求单独验证」）：torchwood 当前无流式 RPC（AssertSemantic 禁用 streaming，
// authn 尚无 stream 拦截器），本文件用合成策略集验证库 authz 拦截器的 stream
// 面在 torchwood 注册表/规范树上可用——服务端流、客户端流、双向流三种流型
// 的 open-time 授权 + per-message 模式的逐消息实例门。未来放开流式 RPC 前，
// 先补 authn stream 拦截器（否则无 principal，authz 恒拒）。

// fakeStream 满足 grpc.ServerStream；RecvMsg 用 proto.Merge 把预设消息
// 拷贝进调用方缓冲（实例门依赖消息字段）。
type fakeStream struct {
	ctx  context.Context
	msgs []proto.Message
	idx  int
}

func (s *fakeStream) Context() context.Context     { return s.ctx }
func (s *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeStream) SetTrailer(metadata.MD)       {}
func (s *fakeStream) SendMsg(m any) error          { return nil }
func (s *fakeStream) RecvMsg(m any) error {
	if s.idx >= len(s.msgs) {
		return io.EOF
	}
	msg := s.msgs[s.idx]
	s.idx++
	dst, ok := m.(proto.Message)
	if !ok {
		return status.Error(codes.Internal, "fakeStream: dst is not proto.Message")
	}
	proto.Merge(dst, msg)
	return nil
}

// streamTestSet 构造三种流型（open-time）+ per-message 的合成策略集
// （SERVER 档 databases.read + database_id 实例门；与生产规范树同构）。
func streamTestSet(t *testing.T) *policy.Set {
	t.Helper()
	reg, err := domainauth.PolicyRegistry()
	require.NoError(t, err)
	spec := domainauth.TreeSpec{
		Access:            domainauth.AccessServer,
		Scope:             &domainauth.ScopeRule{Resource: "databases", Op: domainauth.ScopeRead},
		InstanceAddressed: true,
	}
	tree, err := spec.Expand()
	require.NoError(t, err)
	ops := []policy.Operation{}
	for _, id := range []string{
		"/test.StreamService/ServerStream",
		"/test.StreamService/ClientStream",
		"/test.StreamService/BidiStream",
	} {
		ops = append(ops, policy.Operation{
			ID: id, Kind: policy.KindGRPC, Service: "test.StreamService",
			Policy: tree, Exposure: policy.ExposureInternal, Streaming: true,
			StreamAuth: policy.StreamAuthOpen,
		})
	}
	ops = append(ops, policy.Operation{
		ID: "/test.StreamService/PerMessage", Kind: policy.KindGRPC, Service: "test.StreamService",
		Policy: tree, Exposure: policy.ExposureInternal, Streaming: true,
		StreamAuth: policy.StreamAuthPerMessage,
	})
	set, err := policy.NewSetWithVocabulary(reg, domainauth.PolicyVocabulary(), ops...)
	require.NoError(t, err)
	return set
}

func streamCtx(p *shared.Principal) context.Context {
	return principal.With(context.Background(), domainauth.ProjectPrincipal(p))
}

func streamKey(scopes ...string) *shared.Principal {
	return &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		APIKeyID:       "key-stream",
		ProjectID:      "proj-1",
		Roles:          []string{"keys"},
		Permissions:    scopes,
	}
}

func streamAdmin() *shared.Principal {
	return &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		CredentialType:  shared.CredentialTypeSession,
		AdminID:         "admin-1",
		ProjectID:       "proj-1",
		IsPlatformAdmin: true,
		Roles:           []string{"viewer"},
	}
}

// TestAuthzStream_OpenTime 三种流型（服务端流/客户端流/双向流）open-time
// 授权：admin 过 admin 门放行；匿名 Unauthenticated；无 scope key 拒绝；
// 实例 token key 过 resource 门（寻址 blog）。
func TestAuthzStream_OpenTime(t *testing.T) {
	t.Parallel()
	set := streamTestSet(t)
	az, err := authz.NewInterceptor(set, nil, authz.Options{})
	require.NoError(t, err)

	run := func(ctx context.Context, method string) error {
		handler := func(any, grpc.ServerStream) error { return nil }
		return az.Stream()(nil, &fakeStream{ctx: ctx, msgs: []proto.Message{
			&serverv1.GetDatabaseRequest{Id: "blog"},
		}}, &grpc.StreamServerInfo{FullMethod: method}, handler)
	}

	// 三种流型共享同一 open-time 语义（流型差异不影响裁决）。
	for _, method := range []string{
		"/test.StreamService/ServerStream",
		"/test.StreamService/ClientStream",
		"/test.StreamService/BidiStream",
	} {
		require.NoError(t, run(streamCtx(streamAdmin()), method), "admin（不限角色）应放行 %s", method)
		require.Equal(t, codes.Unauthenticated, status.Code(run(context.Background(), method)),
			"匿名 %s 应 Unauthenticated", method)
		require.Equal(t, codes.PermissionDenied, status.Code(run(streamCtx(streamKey()), method)),
			"无 scope key %s 应拒绝", method)
		// open-time 授权在库拦截器中不携带请求消息（req=nil → 实例门哨兵），
		// 实例 token key 无法证明寻址目标 → 拒绝（fail-closed，与旧「无目标
		// 方法对实例限定 scope 恒不匹配」语义一致；裸资源/通配符照常放行）。
		require.Equal(t, codes.PermissionDenied, status.Code(run(streamCtx(streamKey("databases:blog")), method)),
			"实例 token key open-time 应拒绝 %s", method)
		require.NoError(t, run(streamCtx(streamKey("databases")), method),
			"裸资源 key open-time 应放行 %s", method)
	}
}

// TestAuthzStream_PerMessage 逐消息授权：per-message 模式下每条消息重新
// 求值——实例限定 key（databases:blog）在寻址其他实例的消息处中断流。
func TestAuthzStream_PerMessage(t *testing.T) {
	t.Parallel()
	set := streamTestSet(t)
	az, err := authz.NewInterceptor(set, nil, authz.Options{})
	require.NoError(t, err)

	run := func(scopes ...string) error {
		stream := &fakeStream{ctx: streamCtx(streamKey(scopes...)), msgs: []proto.Message{
			&serverv1.GetDatabaseRequest{Id: "blog"},
			&serverv1.GetDatabaseRequest{Id: "cms"},
		}}
		handler := func(_ any, ss grpc.ServerStream) error {
			var m serverv1.GetDatabaseRequest
			for {
				// 逐条消费：第二条消息（cms）触发实例门拒绝并中断流；
				// 消费完毕（io.EOF）正常收尾。
				if err := ss.RecvMsg(&m); err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
			}
		}
		return az.Stream()(nil, stream, &grpc.StreamServerInfo{FullMethod: "/test.StreamService/PerMessage"}, handler)
	}

	require.Equal(t, codes.PermissionDenied, status.Code(run("databases:blog")),
		"实例限定 key 在寻址其他实例的消息处应中断流")
	require.NoError(t, run("databases"), "裸资源 key 逐消息恒放行")
}
