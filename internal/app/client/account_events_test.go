package client

// auth.users.* 系统事件发布用例（functions-v3 §4.1 增补）：注册发 created
// +signed_in、登出发 signed_out、Attrs 脱敏锐断、发布失败即用例失败、
// nil 端口跳过。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

type captureEventPublisher struct {
	fail bool
	envs []domainevents.Envelope
}

func (p *captureEventPublisher) Publish(_ context.Context, ev domainevents.Envelope) error {
	if p.fail {
		return errors.New("outbox down")
	}
	p.envs = append(p.envs, ev)
	return nil
}

func newEventCaptureAccount(repo users.Repository, projectID string, pub *captureEventPublisher) *Account {
	a := newAccountWithUserRepo(repo, projectID)
	a.events = pub
	return a
}

func TestSignUpPublishesCreatedAndSignedIn(t *testing.T) {
	repo := newRecordingUserRepo()
	pub := &captureEventPublisher{}
	a := newEventCaptureAccount(repo, "p1", pub)

	user, _, _, _, err := a.SignUp(context.Background(), SignUpCommand{
		ProjectID: "p1", Email: "new@example.com", Password: "User@123", Name: "New",
	})
	require.NoError(t, err)

	require.Len(t, pub.envs, 2, "注册完成（自动登录）= created + signed_in 两条")
	created, signedIn := pub.envs[0], pub.envs[1]
	require.Equal(t, domainevents.EventAuthUsersCreated, created.Event)
	require.Equal(t, domainevents.EventAuthUsersSignedIn, signedIn.Event)

	for _, ev := range pub.envs {
		require.Equal(t, "p1", ev.ProjectID)
		require.Equal(t, domainevents.AuthEventDomain, ev.Domain)
		require.Equal(t, "accounts."+user.ID, ev.Channel, "与 payments 同频道，WS 本人订阅零改动")
		require.NotEmpty(t, ev.EventID)
		require.Positive(t, ev.Version)
		// 脱敏锐断：白名单键，无密码哈希/token。
		for k := range ev.Attrs {
			require.Contains(t, []string{"user_id", "email", "name", "provider"}, k)
		}
	}
	require.Equal(t, map[string]any{"user_id": user.ID, "email": "new@example.com", "name": "New"}, created.Attrs)
	require.Equal(t, "email", signedIn.Attrs["provider"], "signed_in 携带登录方式")
}

func TestSignUpEventPublishFailureFailsSignUp(t *testing.T) {
	repo := newRecordingUserRepo()
	a := newEventCaptureAccount(repo, "p1", &captureEventPublisher{fail: true})

	_, _, _, _, err := a.SignUp(context.Background(), SignUpCommand{
		ProjectID: "p1", Email: "x@example.com", Password: "User@123",
	})
	require.Error(t, err, "发布失败即注册失败——用户已创建但 created 事件丢失不允许存在")
}

func TestSignUpWithNilPublisherSkipsEvents(t *testing.T) {
	repo := newRecordingUserRepo()
	a := newAccountWithUserRepo(repo, "p1") // events = nil（既有装配兼容）

	_, _, _, _, err := a.SignUp(context.Background(), SignUpCommand{
		ProjectID: "p1", Email: "y@example.com", Password: "User@123",
	})
	require.NoError(t, err, "nil 端口跳过发布不影响用例")
}

func TestSignOutPublishesSignedOut(t *testing.T) {
	pub := &captureEventPublisher{}
	a := newAccountWithUserRepo(newRecordingUserRepo(), "p1")
	a.events = pub
	a.sessionRepo = &memSessionRepo{}

	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser, ProjectID: "p1", UserID: "u_1", SessionID: "s_1",
	})
	require.NoError(t, a.SignOut(ctx))
	require.Len(t, pub.envs, 1)
	require.Equal(t, domainevents.EventAuthUsersSignedOut, pub.envs[0].Event)
	require.Equal(t, map[string]any{"user_id": "u_1"}, pub.envs[0].Attrs, "登出仅携带 user_id")
}

// memSessionRepo 是 SignOut 所需的最小 session repo 桩（嵌入接口零值，
// 仅覆写 Delete——SignOut 唯一调用的方法）。
type memSessionRepo struct {
	domainauth.SessionRepository
}

func (m *memSessionRepo) Delete(context.Context, string, string) error { return nil }
