//go:build docexample

// Package docexamples 承载 docs/developer/12-sdk.md 与 sdk/README.md 中的
// 全部 Go 示例代码，保证文档片段可编译（go vet -tags docexample ./sdk/...）。
// 修改上述文档中的 Go 示例时，必须同步修改本文件对应函数——两者一一对应。
// 本包不参与常规构建，也不会被任何测试执行（仅编译期校验）。
package docexamples

import (
	"context"
	"os"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/client"
	"github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// DocSDKGuideServerAPI 对应 docs/developer/12-sdk.md §4.4「典型用法」的
// Server API（管理面）部分。
func DocSDKGuideServerAPI(ctx context.Context) error {
	srv, err := server.New("127.0.0.1:9060",
		server.WithAPIKey(os.Getenv("TORCHWOOD_API_KEY")),
		server.WithDatabaseID("app"))
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	user, err := srv.Users.CreateUser(ctx, "agent@example.com", "Pass@123", "Agent", "active", nil, nil)
	if err != nil {
		return err
	}
	tok, err := srv.Users.CreateUserToken(ctx, user.Id)
	if err != nil {
		return err
	}
	doc, err := srv.Databases.UpsertDocument(ctx, "members", "m1",
		map[string]any{"channel_id": "ch1"}, []string{"channel_id", "user_id"}, nil)
	if err != nil {
		return err
	}
	n, err := srv.Databases.CountDocuments(ctx, "messages", []string{`equal("channel_id","ch1")`})
	if err != nil {
		return err
	}
	raw, err := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))
	if err != nil {
		return err
	}
	letters, err := srv.Outbox.ListDeadLetters(ctx, &serverv1.ListDeadLettersRequest{
		ProjectId: "default",
		PageSize:  20,
	})
	if err != nil {
		return err
	}
	_, _, _, _, _ = tok, doc, n, raw, letters
	return nil
}

// DocSDKGuideClientAPI 对应 docs/developer/12-sdk.md §4.4 的 Client API
// （自动刷新）部分。
func DocSDKGuideClientAPI(ctx context.Context) error {
	store := client.NewFileTokenStore("~/.torchwood/tokens.json")
	c, err := client.New("127.0.0.1:9060",
		client.WithProjectID("default"),
		client.WithTokenStore(store))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Account.SignIn(ctx, "u@example.com", "Pass@123"); err != nil {
		return err
	}
	me, err := c.Account.Me(ctx)
	if err != nil {
		return err
	}
	_ = me
	return nil
}

// DocSDKGuideInvokeJSON 对应 docs/developer/12-sdk.md §4.3 的 InvokeJSON
// 动态分发票段。
func DocSDKGuideInvokeJSON(ctx context.Context) error {
	srv, err := server.New("127.0.0.1:9060", server.WithAPIKey(os.Getenv("TORCHWOOD_API_KEY")))
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	respJSON, err := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{}`))
	_ = respJSON
	return err
}

// DocReadmeGoExample 对应 sdk/README.md「Go SDK」一节的完整示例。
func DocReadmeGoExample(ctx context.Context) error {
	apiKey := os.Getenv("TORCHWOOD_API_KEY")

	// Server API：以 API Key 管理用户/用户组/文档库
	srv, err := server.New("127.0.0.1:9060",
		server.WithAPIKey(apiKey),
		server.WithDatabaseID("app"))
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()

	// 为 Agent 账号签发 client token
	tok, err := srv.Users.CreateUserToken(ctx, "user-1")
	if err != nil {
		return err
	}

	// 文档 upsert（按唯一索引冲突列）
	doc, err := srv.Databases.UpsertDocument(ctx, "members", "m1",
		map[string]any{"channel_id": "ch1", "last_read_seq": 42},
		[]string{"channel_id", "user_id"}, nil)
	if err != nil {
		return err
	}

	// 逃生舱：按方法名 + JSON 调用任意 Server API unary 方法
	respJSON, err := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))
	if err != nil {
		return err
	}

	// Agent 默认工具箱：工具名 → 已有 RPC（不含 API key 管理）
	respJSON, err = srv.InvokeTool(ctx, "list_users", []byte(`{"pageSize":10}`))
	if err != nil {
		return err
	}

	// Client API：注册/登录自动保存 token，过期自动刷新
	store := client.NewFileTokenStore("~/.torchwood/tokens.json")
	c, err := client.New("127.0.0.1:9060",
		client.WithProjectID("default"),
		client.WithTokenStore(store),
	)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Account.SignIn(ctx, "u@example.com", "Pass@123"); err != nil {
		return err
	}
	me, err := c.Account.Me(ctx)
	if err != nil {
		return err
	}
	_, _, _, _ = tok, doc, respJSON, me
	return nil
}
