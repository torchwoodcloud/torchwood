package clientgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/genproto/client/v1"
	appclient "github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
)

// clientChangesDocDB：仅覆写 ListChanges/GetCollection（其余经嵌入接口
// nil 兜底，不在测试路径上）。
type clientChangesDocDB struct {
	databases.DocumentDB
	changes      []databases.DocumentChange
	hasMore      bool
	nextSinceSeq int64
	err          error
}

func (c *clientChangesDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return &databases.Collection{ID: "posts", Name: "Posts"}, nil
}

func (c *clientChangesDocDB) ListChanges(context.Context, string, string, string, databases.ListChangesOptions, databases.Principal) ([]databases.DocumentChange, bool, int64, error) {
	if c.err != nil {
		return nil, false, 0, c.err
	}
	return c.changes, c.hasMore, c.nextSinceSeq, nil
}

// TestClientGRPC_ListChanges（阶段④ §4.5，与 Server 面同用例核心）：
// 映射与 has_more 透传、RESUME_EXPIRED 域码透传。
func TestClientGRPC_ListChanges(t *testing.T) {
	docDB := &clientChangesDocDB{changes: []databases.DocumentChange{
		{Seq: 3, EventID: "e1", Event: domainevents.EventDocumentsCreate, DocumentID: "d1",
			Version: 1, CreatedAt: time.Now(),
			Data: &databases.Document{ID: "d1", Data: map[string]any{"t": "v"}, Version: 1}},
	}, hasMore: false}
	svc := NewDatabasesService(appclient.NewDatabases(&fakeProjectRepo{project: &projects.Project{ID: "proj-1"}}, docDB, nil))
	ctx := clientCtx()

	resp, err := svc.ListChanges(ctx, &clientv1.ListChangesRequest{
		ProjectId: "proj-1", DatabaseId: "app", CollectionId: "posts", SinceSeq: 2,
	})
	require.NoError(t, err)
	require.False(t, resp.HasMore)
	require.Len(t, resp.Changes, 1)
	require.Equal(t, int64(3), resp.Changes[0].Seq)
	require.NotNil(t, resp.Changes[0].Data)

	docDB.err = databases.ErrResumeExpired
	_, err = svc.ListChanges(ctx, &clientv1.ListChangesRequest{
		ProjectId: "proj-1", DatabaseId: "app", CollectionId: "posts", SinceSeq: 1,
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "EVENTS.RESUME_EXPIRED")
}

func clientCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "user-1", ActorKind: shared.ActorKindEndUser, ProjectID: "proj-1",
		UserID: "u1", Roles: []string{"users", "user:u1"},
	})
}
