package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

// grpc-gateway 的 ServeMux.Handle 是头插（LIFO）：同 HTTP 方法下后注册的
// pattern 先匹配，注册顺序 = proto 声明顺序。当字面量路径（/v1/console/
// admins/me）与变量路径（/v1/console/admins/{id}）同形竞争时，字面量 rpc
// 必须声明在后，否则 /me 被变量路由吞掉——曾导致 PATCH /v1/console/admins/me
// 实际执行 UpdateAdmin(id="me")，GetAdmin("me") 查库 nil → 404 "admin not
// found"（Console 保存时区偏好全挂）。本测试以 in-process Server 变体注册
// （mux.Handle 顺序与生产 FromEndpoint/Client 变体同源生成），锁定路由归属。
type recordingAdminsService struct {
	consolev1.UnimplementedAdminsServiceServer

	mu     sync.Mutex
	called []string
}

func (s *recordingAdminsService) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = append(s.called, name)
}

func (s *recordingAdminsService) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.called...)
}

func (s *recordingAdminsService) GetCurrentAdmin(context.Context, *consolev1.GetCurrentAdminRequest) (*consolev1.Admin, error) {
	s.record("GetCurrentAdmin")
	return &consolev1.Admin{}, nil
}

func (s *recordingAdminsService) UpdateCurrentAdmin(context.Context, *consolev1.UpdateCurrentAdminRequest) (*consolev1.Admin, error) {
	s.record("UpdateCurrentAdmin")
	return &consolev1.Admin{}, nil
}

func (s *recordingAdminsService) ListAdmins(context.Context, *consolev1.ListAdminsRequest) (*consolev1.ListAdminsResponse, error) {
	s.record("ListAdmins")
	return &consolev1.ListAdminsResponse{}, nil
}

func (s *recordingAdminsService) CreateAdmin(context.Context, *consolev1.CreateAdminRequest) (*consolev1.Admin, error) {
	s.record("CreateAdmin")
	return &consolev1.Admin{}, nil
}

func (s *recordingAdminsService) UpdateAdmin(context.Context, *consolev1.UpdateAdminRequest) (*consolev1.Admin, error) {
	s.record("UpdateAdmin")
	return &consolev1.Admin{}, nil
}

func (s *recordingAdminsService) DeleteAdmin(context.Context, *consolev1.DeleteAdminRequest) (*sharedv1.Empty, error) {
	s.record("DeleteAdmin")
	return &sharedv1.Empty{}, nil
}

func TestGatewayRouteOrder_AdminsMeLiteralBeatsWildcard(t *testing.T) {
	t.Parallel()

	svc := &recordingAdminsService{}
	mux := gwruntime.NewServeMux(
		gwruntime.WithErrorHandler(HTTPErrorHandler),
		gwruntime.WithMarshalerOption("application/json", NewCustomMarshaler()),
	)
	require.NoError(t, consolev1.RegisterAdminsServiceHandlerServer(context.Background(), mux, svc))

	do := func(method, target, body string) int {
		svc.mu.Lock()
		svc.called = nil
		svc.mu.Unlock()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	// 自述双端点必须落在字面量 /me 路由，而不是 /{id}（id="me" 查库必空）。
	require.Equal(t, http.StatusOK, do(http.MethodGet, "/v1/console/admins/me", ""))
	require.Equal(t, []string{"GetCurrentAdmin"}, svc.names(),
		"GET /v1/console/admins/me 被 {id} 变量路由抢占——检查 admins.proto 中字面量 rpc 的声明顺序")

	require.Equal(t, http.StatusOK, do(http.MethodPatch, "/v1/console/admins/me", `{"timezone":"Asia/Shanghai"}`))
	require.Equal(t, []string{"UpdateCurrentAdmin"}, svc.names(),
		"PATCH /v1/console/admins/me 被 UpdateAdmin(id=\"me\") 抢占——即 404 admin not found 事故的路由翻转，检查 admins.proto 声明顺序")

	// 管理他人的变量路由不受影响（sanity：修复没有把 {id} 路由挤掉）。
	require.Equal(t, http.StatusOK, do(http.MethodPatch, "/v1/console/admins/0192c0de-0000-7000-8000-000000000001", `{"role":"member"}`))
	require.Equal(t, []string{"UpdateAdmin"}, svc.names())

	// 未登记的字面量不会被变量路由误吞后静默成功：DeleteAdmin 面保持变量语义。
	require.Equal(t, http.StatusOK, do(http.MethodDelete, "/v1/console/admins/0192c0de-0000-7000-8000-000000000001", ""))
	require.Equal(t, []string{"DeleteAdmin"}, svc.names())
}
