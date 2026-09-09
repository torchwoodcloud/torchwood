package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

func TestToolsCatalogResolvesFullMethods(t *testing.T) {
	want := []struct {
		name   string
		method string
	}{
		{ToolListUsers, "/torchwood.server.v1.UsersService/ListUsers"},
		{ToolGetUser, "/torchwood.server.v1.UsersService/GetUser"},
		{ToolCreateUser, "/torchwood.server.v1.UsersService/CreateUser"},
		{ToolQueryDocuments, "/torchwood.server.v1.DatabasesService/ListDocuments"},
		{ToolGetDocument, "/torchwood.server.v1.DatabasesService/GetDocument"},
		{ToolCreateDocument, "/torchwood.server.v1.DatabasesService/CreateDocument"},
		{ToolUpdateDocument, "/torchwood.server.v1.DatabasesService/UpdateDocument"},
		{ToolUpsertDocument, "/torchwood.server.v1.DatabasesService/UpsertDocument"},
		{ToolDeleteDocument, "/torchwood.server.v1.DatabasesService/DeleteDocument"},
		{ToolListCollections, "/torchwood.server.v1.DatabasesService/ListCollections"},
		{ToolGetCollection, "/torchwood.server.v1.DatabasesService/GetCollection"},
		{ToolInvokeFunction, "/torchwood.server.v1.FunctionsService/CreateExecution"},
		{ToolListFiles, "/torchwood.server.v1.StorageService/ListFiles"},
		{ToolGetFile, "/torchwood.server.v1.StorageService/GetFile"},
		{ToolGrantAsset, "/torchwood.server.v1.AssetsService/Grant"},
		{ToolListUserAssets, "/torchwood.server.v1.AssetsService/ListUserAssets"},
		{ToolGetOrder, "/torchwood.server.v1.PaymentsService/GetOrder"},
		{ToolGetHealth, "/torchwood.server.v1.HealthService/Check"},
	}
	require.Len(t, want, 18)
	require.Len(t, Tools, 18)

	seen := make(map[string]struct{}, len(Tools))
	for i, tool := range Tools {
		require.Equal(t, want[i].name, tool.Name)
		require.Equal(t, want[i].method, tool.FullMethod)
		require.NotContains(t, tool.FullMethod, "APIKeys")
		_, err := findServerMethod(tool.FullMethod)
		require.NoError(t, err, "tool %s method %s", tool.Name, tool.FullMethod)
		got, ok := LookupTool(tool.Name)
		require.True(t, ok, tool.Name)
		require.Equal(t, tool, got)
		_, dup := seen[tool.Name]
		require.False(t, dup, "duplicate tool name %s", tool.Name)
		seen[tool.Name] = struct{}{}
	}

	keyMethods := []string{
		serverv1.APIKeysService_CreateAPIKey_FullMethodName,
		serverv1.APIKeysService_ListAPIKeys_FullMethodName,
		serverv1.APIKeysService_GetAPIKey_FullMethodName,
		serverv1.APIKeysService_DeleteAPIKey_FullMethodName,
	}
	for _, m := range keyMethods {
		for _, tool := range Tools {
			require.NotEqual(t, m, tool.FullMethod)
		}
		_, err := findServerMethod(m)
		require.Error(t, err)
	}
	for _, name := range []string{"create_api_key", "list_api_keys", "get_api_key", "delete_api_key"} {
		_, ok := LookupTool(name)
		require.False(t, ok, name)
	}
}

func TestInvokeToolGetHealth(t *testing.T) {
	lis, rec := newBufconn(t)
	c := newTestClient(t, lis, WithAPIKey("k"))
	out, err := c.InvokeTool(context.Background(), ToolGetHealth, nil)
	require.NoError(t, err)
	require.Regexp(t, `"status":\s+"ok"`, string(out))
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, []string{"k"}, rec.md.Get("x-api-key"))
}

func TestInvokeToolUnknownAndKeyNames(t *testing.T) {
	lis, _ := newBufconn(t)
	c := newTestClient(t, lis)
	_, err := c.InvokeTool(context.Background(), "create_api_key", nil)
	require.ErrorContains(t, err, `torchwood: unknown tool "create_api_key"`)
	require.False(t, strings.Contains(err.Error(), "APIKeysService"))
}
