package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// changesDocDB：仅覆写 ListChanges/GetCollection（其余经嵌入接口 nil 兜底，
// 不在测试路径上）。
type changesDocDB struct {
	databases.DocumentDB
	changes      []databases.DocumentChange
	hasMore      bool
	nextSinceSeq int64
	err          error
}

func (c *changesDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return &databases.Collection{ID: "posts", Name: "Posts"}, nil
}

func (c *changesDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	if c.err != nil {
		return nil, false, 0, c.err
	}
	return c.changes, c.hasMore, c.nextSinceSeq, nil
}

// TestServerGRPC_ListChanges（阶段④ §4.5）：映射（seq/tombstone/
// transaction_id）、has_more 透传、RESUME_EXPIRED 域码透传。
func TestServerGRPC_ListChanges(t *testing.T) {
	docDB := &changesDocDB{changes: []databases.DocumentChange{
		{Seq: 6, EventID: "e1", Event: domainevents.EventDocumentsCreate, DocumentID: "d1",
			Version: 1, CreatedAt: time.Now(),
			Data: &databases.Document{ID: "d1", Data: map[string]any{"t": "v"}, Version: 1}},
		{Seq: 7, EventID: "e2", Event: domainevents.EventDocumentsDelete, DocumentID: "d1",
			Version: 2, CreatedAt: time.Now(), TransactionID: "tx-9"},
	}, hasMore: true, nextSinceSeq: 7}
	svc := NewDatabasesService(server.NewDatabases(paginationProjectRepo{}, docDB, nil))
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "admin-1", ActorKind: shared.ActorKindAdmin, ProjectID: "proj-1",
		Roles: []string{"owner"}, IsPlatformAdmin: true,
	})

	resp, err := svc.ListChanges(ctx, &serverv1.ListChangesRequest{
		DatabaseId: "app", CollectionId: "posts", SinceSeq: 5, Limit: 10,
	})
	require.NoError(t, err)
	require.True(t, resp.HasMore)
	require.Len(t, resp.Changes, 2)
	require.Equal(t, int64(6), resp.Changes[0].Seq)
	require.NotNil(t, resp.Changes[0].Data, "create 事件带 data")
	require.Equal(t, int64(7), resp.Changes[1].Seq)
	require.Nil(t, resp.Changes[1].Data, "delete 事件无 data（tombstone）")
	require.Equal(t, "tx-9", resp.Changes[1].TransactionId)
	require.Equal(t, int64(7), resp.NextSinceSeq, "R15 扫描游标透传")

	// 游标过期 → FailedPrecondition / EVENTS.RESUME_EXPIRED。
	docDB.err = databases.ErrResumeExpired
	_, err = svc.ListChanges(ctx, &serverv1.ListChangesRequest{DatabaseId: "app", CollectionId: "posts", SinceSeq: 1})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "EVENTS.RESUME_EXPIRED")

	// 负游标 → InvalidArgument：形状校验已上收为 ListChangesRequest 的
	// buf.validate gte 注解，由 validate 拦截器求值（直调 handler 绕过
	// 拦截器，覆盖见 interceptor 包 TestValidateInterceptorRejectsShapeRules）。
}
