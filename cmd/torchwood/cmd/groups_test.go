package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildCreateGroupReq(t *testing.T) {
	tests := []struct {
		name        string
		groupName   string
		permissions string
		wantErr     string
	}{
		{name: "缺 name", wantErr: "--name 必填"},
		{name: "最小字段", groupName: "核心组", wantErr: ""},
		{name: "带权限", groupName: "核心组", permissions: `["read(\"groups\")"]`, wantErr: ""},
		{name: "permissions 非法", groupName: "核心组", permissions: `oops`, wantErr: "--permissions 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateGroupReq(tt.groupName, tt.permissions)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.groupName, req["name"])
			if tt.permissions != "" {
				require.Equal(t, []string{`read("groups")`}, req["permissions"])
			} else {
				require.NotContains(t, req, "permissions")
			}
		})
	}
}

func TestBuildUpdateGroupPrefsReq(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		data    string
		wantErr string
	}{
		{name: "缺 id", wantErr: "缺少用户组 ID"},
		{name: "缺 data", id: "t1", wantErr: "--data 必填"},
		{name: "data 非对象", id: "t1", data: `"str"`, wantErr: "--data 解析失败"},
		{name: "正常", id: "t1", data: `{"theme":"dark"}`, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateGroupPrefsReq(tt.id, tt.data)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, req["id"])
			prefs, ok := req["prefs"].(map[string]any)
			require.True(t, ok, "prefs 不是 map: %v", req["prefs"])
			require.Equal(t, "dark", prefs["theme"])
		})
	}
}

func TestBuildCreateMembershipReq(t *testing.T) {
	tests := []struct {
		name       string
		groupID    string
		userID     string
		email      string
		memberName string
		roles      string
		status     string
		wantErr    string
	}{
		{name: "缺 group-id", wantErr: "缺少 group-id"},
		{name: "user-id/email 全缺", groupID: "t1", wantErr: "--user-id 与 --email 至少提供一个"},
		{name: "按 user-id", groupID: "t1", userID: "u1", roles: `["admin"]`, status: "active", wantErr: ""},
		{name: "按 email 邀请", groupID: "t1", email: "a@b.c", memberName: "Alice", wantErr: ""},
		{name: "roles 非法", groupID: "t1", userID: "u1", roles: `x`, wantErr: "--roles 解析失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildCreateMembershipReq(tt.groupID, tt.userID, tt.email, tt.memberName, tt.roles, tt.status)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.groupID, req["groupId"])
			if tt.userID != "" {
				require.Equal(t, tt.userID, req["userId"])
			} else {
				require.NotContains(t, req, "userId")
			}
			if tt.email != "" {
				require.Equal(t, tt.email, req["email"])
			} else {
				require.NotContains(t, req, "email")
			}
			if tt.memberName != "" {
				require.Equal(t, tt.memberName, req["name"])
			} else {
				require.NotContains(t, req, "name")
			}
			if tt.roles != "" {
				require.Equal(t, []string{"admin"}, req["roles"])
			} else {
				require.NotContains(t, req, "roles")
			}
			if tt.status != "" {
				require.Equal(t, tt.status, req["status"])
			} else {
				require.NotContains(t, req, "status")
			}
		})
	}
}

func TestBuildListMembershipsReq(t *testing.T) {
	tests := []struct {
		name      string
		groupID   string
		queries   string
		pageSize  int
		pageToken string
		wantErr   string
	}{
		{name: "缺 group-id", wantErr: "缺少 group-id"},
		{name: "最小字段", groupID: "t1", wantErr: ""},
		{name: "queries 非法", groupID: "t1", queries: `x`, wantErr: "--queries 解析失败"},
		{name: "全字段", groupID: "t1", queries: `["equal(\"status\",\"active\")"]`, pageSize: 10, pageToken: "tok", wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildListMembershipsReq(tt.groupID, tt.queries, tt.pageSize, tt.pageToken)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.groupID, req["groupId"])
			if tt.queries != "" {
				require.Equal(t, []string{`equal("status","active")`}, req["queries"])
			} else {
				require.NotContains(t, req, "queries")
			}
			if tt.pageSize > 0 {
				require.Equal(t, 10, req["pageSize"])
			} else {
				require.NotContains(t, req, "pageSize")
			}
			if tt.pageToken != "" {
				require.Equal(t, tt.pageToken, req["pageToken"])
			} else {
				require.NotContains(t, req, "pageToken")
			}
		})
	}
}

func TestBuildUpdateMembershipReq(t *testing.T) {
	tests := []struct {
		name         string
		groupID      string
		membershipID string
		roles        string
		wantErr      string
	}{
		{name: "缺 membership-id", groupID: "t1", roles: `["admin"]`, wantErr: "缺少 membership-id"},
		{name: "缺 roles", groupID: "t1", membershipID: "m1", wantErr: "--roles 必填"},
		{name: "正常", groupID: "t1", membershipID: "m1", roles: `["admin","viewer"]`, wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateMembershipReq(tt.groupID, tt.membershipID, tt.roles)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.groupID, req["groupId"])
			require.Equal(t, tt.membershipID, req["membershipId"])
			require.Equal(t, []string{"admin", "viewer"}, req["roles"])
		})
	}
}

func TestBuildUpdateMembershipStatusReq(t *testing.T) {
	tests := []struct {
		name         string
		groupID      string
		membershipID string
		status       string
		wantErr      string
	}{
		{name: "缺 status", groupID: "t1", membershipID: "m1", wantErr: "--status 必填"},
		{name: "正常", groupID: "t1", membershipID: "m1", status: "blocked", wantErr: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := buildUpdateMembershipStatusReq(tt.groupID, tt.membershipID, tt.status)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.groupID, req["groupId"])
			require.Equal(t, tt.membershipID, req["membershipId"])
			require.Equal(t, tt.status, req["status"])
		})
	}
}
