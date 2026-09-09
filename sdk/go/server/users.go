package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// UsersService 封装 Server API 的用户管理服务。
type UsersService struct{ c *Client }

// CreateUser 创建用户（Agent 账号也走此接口）。
func (u *UsersService) CreateUser(ctx context.Context, email, password, name, status string, labels, prefs map[string]any) (*serverv1.User, error) {
	labelStruct, err := toStruct(labels)
	if err != nil {
		return nil, err
	}
	prefsStruct, err := toStruct(prefs)
	if err != nil {
		return nil, err
	}
	return u.c.users.CreateUser(ctx, &serverv1.CreateUserRequest{
		Email:    email,
		Password: password,
		Name:     name,
		Status:   status,
		Labels:   labelStruct,
		Prefs:    prefsStruct,
	})
}

// GetUser 按 ID 获取用户。
func (u *UsersService) GetUser(ctx context.Context, userID string) (*serverv1.User, error) {
	return u.c.users.GetUser(ctx, &serverv1.GetUserRequest{Id: userID})
}

// ListUsers 按查询 DSL 列出用户。
func (u *UsersService) ListUsers(ctx context.Context, queries []string, pageSize int32, pageToken string) (*serverv1.ListUsersResponse, error) {
	return u.c.users.ListUsers(ctx, &sharedv1.ListRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
		Queries:   queries,
	})
}

// UpdateUser 更新用户档案字段；name/email/status 传 nil 表示不修改，
// 传指针（含空串）表示更新/清空。
func (u *UsersService) UpdateUser(ctx context.Context, userID string, name, email, status *string, labels, prefs map[string]any) (*serverv1.User, error) {
	labelStruct, err := toStruct(labels)
	if err != nil {
		return nil, err
	}
	prefsStruct, err := toStruct(prefs)
	if err != nil {
		return nil, err
	}
	return u.c.users.UpdateUser(ctx, &serverv1.UpdateUserRequest{
		Id:     userID,
		Name:   name,
		Email:  email,
		Status: status,
		Labels: labelStruct,
		Prefs:  prefsStruct,
	})
}

// UpdateUserPassword 更新用户密码（agent 账号重置密码）。
func (u *UsersService) UpdateUserPassword(ctx context.Context, userID, password string) (*serverv1.User, error) {
	return u.c.users.UpdateUserPassword(ctx, &serverv1.UpdateUserPasswordRequest{
		Id:       userID,
		Password: password,
	})
}

// DeleteUser 删除用户。
func (u *UsersService) DeleteUser(ctx context.Context, userID string) error {
	_, err := u.c.users.DeleteUser(ctx, &serverv1.GetUserRequest{Id: userID})
	return err
}

// ListUserSessions 列出用户会话。
func (u *UsersService) ListUserSessions(ctx context.Context, userID string) (*serverv1.ListUserSessionsResponse, error) {
	return u.c.users.ListUserSessions(ctx, &serverv1.GetUserRequest{Id: userID})
}

// DeleteUserSession 删除指定用户会话（Agent 撤权三重生效的一环）。
func (u *UsersService) DeleteUserSession(ctx context.Context, userID, sessionID string) error {
	_, err := u.c.users.DeleteUserSession(ctx, &serverv1.DeleteUserSessionRequest{
		Id:        userID,
		SessionId: sessionID,
	})
	return err
}

// CreateUserToken 为任意用户签发 client token（模拟登录/调试）。

func (u *UsersService) CreateUserToken(ctx context.Context, userID string) (*serverv1.CreateUserTokenResponse, error) {
	return u.c.users.CreateUserToken(ctx, &serverv1.GetUserRequest{Id: userID})
}

func toStruct(data map[string]any) (*structpb.Struct, error) {
	if len(data) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(data)
}
