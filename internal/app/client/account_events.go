// auth 域系统行为事件（functions-v3 §4.1 增补；词表唯一声明源
// internal/domain/events/catalog.go）。发布语义与事件脊柱不变量一致：
// 调用方在 uow.Run 内调用时与业务写同一 COMMIT（SignUp/SignIn/SignOut
// 均如此）；事件发布失败即整个用例失败——outbox 与 users/sessions 同库
// 同实例，「用户已创建但 created 事件丢失」不允许发生。
package client

import (
	"context"
	"time"

	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// authUsersEventEnvelope 组装 auth.users.* 事件信封（Domain=auth，
// Channel=accounts.{userId} 与 payments/economy 同频道——WS 本人订阅
// accounts 频道零改动即收到；Attrs 是脱敏白名单，绝不含密码哈希/token/IP）。
func authUsersEventEnvelope(projectID, event string, user *User, now time.Time, extra map[string]any) domainevents.Envelope {
	attrs := map[string]any{
		"user_id": user.ID,
	}
	if user.Email != "" {
		attrs["email"] = user.Email
	}
	if user.Name != "" {
		attrs["name"] = user.Name
	}
	for k, v := range extra {
		attrs[k] = v
	}
	return domainevents.Envelope{
		EventID:   idgen.UUID().String(),
		Event:     event,
		ProjectID: projectID,
		Domain:    domainevents.AuthEventDomain,
		Channel:   "accounts." + user.ID,
		CreatedAt: now,
		// version 用事件时刻纳秒：同频道内单调递增，客户端可判序（对齐
		// payments P1-14 语义）。
		Version: now.UnixNano(),
		Attrs:   attrs,
	}
}

// publishAuthUsersEvent 发布一条 auth.users.* 事件（nil 端口 = 未装配，
// 测试兼容直接跳过，对齐 analyticsDeletions 先例；生产装配恒非 nil）。
func (a *Account) publishAuthUsersEvent(ctx context.Context, projectID, event string, user *User, extra map[string]any) error {
	if a.events == nil || user == nil {
		return nil
	}
	return a.events.Publish(ctx, authUsersEventEnvelope(projectID, event, user, time.Now().UTC(), extra))
}
