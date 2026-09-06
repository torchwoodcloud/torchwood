package interceptor

import "testing"

func TestAPIKeyScopeAllowed(t *testing.T) {
	t.Parallel()
	method := "/torchwood.server.v1.UsersService/ListUsers"

	if APIKeyScopeAllowed(method, nil) {
		t.Fatal("empty scopes should deny resource-scoped methods")
	}
	if !APIKeyScopeAllowed(method, []string{"*"}) {
		t.Fatal("wildcard scope should allow")
	}
	if !APIKeyScopeAllowed(method, []string{"all"}) {
		t.Fatal("all scope should allow")
	}
	if !APIKeyScopeAllowed(method, []string{"users"}) {
		t.Fatal("matching bare resource scope should allow")
	}
	if !APIKeyScopeAllowed(method, []string{"users.read"}) {
		t.Fatal("read scope should allow read method")
	}
	if APIKeyScopeAllowed(method, []string{"users.write"}) {
		t.Fatal("write scope must NOT allow read method")
	}
	if APIKeyScopeAllowed(method, []string{"storage"}) {
		t.Fatal("unrelated scope should deny")
	}

	oauthMethod := "/torchwood.server.v1.OAuthProvidersService/ListOAuthProviders"
	if APIKeyScopeAllowed(oauthMethod, nil) {
		t.Fatal("empty scopes should deny oauth providers method")
	}
	if APIKeyScopeAllowed(oauthMethod, []string{"projects"}) {
		t.Fatal("projects scope must NOT allow oauth providers method (client_secret isolation)")
	}
	if !APIKeyScopeAllowed(oauthMethod, []string{"oauthproviders"}) {
		t.Fatal("oauthproviders scope should allow oauth providers method")
	}
	if !APIKeyScopeAllowed(oauthMethod, []string{"oauthproviders.read"}) {
		t.Fatal("read scope should allow oauth providers method")
	}

	unmapped := "/torchwood.server.v1.SomeFutureService/DoSomething"
	if APIKeyScopeAllowed(unmapped, []string{"*"}) {
		t.Fatal("unmapped service must fail closed even for wildcard scope")
	}
	if APIKeyScopeAllowed(unmapped, []string{"all"}) {
		t.Fatal("unmapped service must fail closed even for all scope")
	}

	createMethod := "/torchwood.server.v1.StorageService/CreateFile"
	getMethod := "/torchwood.server.v1.StorageService/GetFile"
	if !APIKeyScopeAllowed(createMethod, []string{"storage.write"}) {
		t.Fatal("storage.write scope should allow upload")
	}
	if !APIKeyScopeAllowed(getMethod, []string{"storage.read"}) {
		t.Fatal("storage.read scope should allow download")
	}
	// B2 精确匹配：storage.read 不再因前缀匹配放行写方法（原前缀语义在此收紧）。
	if APIKeyScopeAllowed(createMethod, []string{"storage.read"}) {
		t.Fatal("storage.read scope must NOT allow upload")
	}
	if APIKeyScopeAllowed(getMethod, []string{"storage.write"}) {
		t.Fatal("storage.write scope must NOT allow download")
	}
	if APIKeyScopeAllowed(getMethod, []string{"users"}) {
		t.Fatal("unrelated scope should deny download")
	}
	if !APIKeyScopeAllowed(createMethod, []string{"storage"}) {
		t.Fatal("bare resource scope should allow upload")
	}
	if !APIKeyScopeAllowed(getMethod, []string{"storage"}) {
		t.Fatal("bare resource scope should allow download")
	}

	// Databases：方法级 read/write 细分（B2）。
	listDocs := "/torchwood.server.v1.DatabasesService/ListDocuments"
	getDoc := "/torchwood.server.v1.DatabasesService/GetDocument"
	countDocs := "/torchwood.server.v1.DatabasesService/CountDocuments"
	deleteDB := "/torchwood.server.v1.DatabasesService/DeleteDatabase"
	createDoc := "/torchwood.server.v1.DatabasesService/CreateDocument"
	upsertDoc := "/torchwood.server.v1.DatabasesService/UpsertDocument"
	bulkUpdate := "/torchwood.server.v1.DatabasesService/BulkUpdateDocuments"
	if !APIKeyScopeAllowed(listDocs, []string{"databases.read"}) {
		t.Fatal("databases.read should allow ListDocuments")
	}
	if !APIKeyScopeAllowed(getDoc, []string{"databases.read"}) {
		t.Fatal("databases.read should allow GetDocument")
	}
	if !APIKeyScopeAllowed(countDocs, []string{"databases.read"}) {
		t.Fatal("databases.read should allow CountDocuments")
	}
	if APIKeyScopeAllowed(deleteDB, []string{"databases.read"}) {
		t.Fatal("databases.read must NOT allow DeleteDatabase")
	}
	if APIKeyScopeAllowed(createDoc, []string{"databases.read"}) {
		t.Fatal("databases.read must NOT allow CreateDocument")
	}
	if APIKeyScopeAllowed(bulkUpdate, []string{"databases.read"}) {
		t.Fatal("databases.read must NOT allow BulkUpdateDocuments")
	}
	if APIKeyScopeAllowed(upsertDoc, []string{"databases.read"}) {
		t.Fatal("databases.read must NOT allow UpsertDocument")
	}
	if !APIKeyScopeAllowed(upsertDoc, []string{"databases.write"}) {
		t.Fatal("databases.write should allow UpsertDocument")
	}
	if !APIKeyScopeAllowed(upsertDoc, []string{"*"}) {
		t.Fatal("wildcard scope should allow UpsertDocument")
	}
	if !APIKeyScopeAllowed(upsertDoc, []string{"all"}) {
		t.Fatal("all scope should allow UpsertDocument")
	}
	if !APIKeyScopeAllowed(upsertDoc, []string{"databases"}) {
		t.Fatal("bare databases scope should allow UpsertDocument")
	}
	if !APIKeyScopeAllowed(deleteDB, []string{"databases.write"}) {
		t.Fatal("databases.write should allow DeleteDatabase")
	}
	if !APIKeyScopeAllowed(createDoc, []string{"databases.write"}) {
		t.Fatal("databases.write should allow CreateDocument")
	}
	if !APIKeyScopeAllowed(bulkUpdate, []string{"databases.write"}) {
		t.Fatal("databases.write should allow BulkUpdateDocuments")
	}
	if APIKeyScopeAllowed(listDocs, []string{"databases.write"}) {
		t.Fatal("databases.write must NOT allow ListDocuments")
	}
	// 裸 databases scope 全放行。
	if !APIKeyScopeAllowed(listDocs, []string{"databases"}) {
		t.Fatal("bare databases scope should allow read methods")
	}
	if !APIKeyScopeAllowed(deleteDB, []string{"databases"}) {
		t.Fatal("bare databases scope should allow write methods")
	}

	// Projects：UpdateProject 需要 projects.write（B2）。
	updateProject := "/torchwood.server.v1.ProjectsService/UpdateProject"
	if !APIKeyScopeAllowed(updateProject, []string{"projects.write"}) {
		t.Fatal("projects.write scope should allow UpdateProject")
	}
	if !APIKeyScopeAllowed(updateProject, []string{"projects"}) {
		t.Fatal("bare projects scope should allow UpdateProject")
	}
	if !APIKeyScopeAllowed(updateProject, []string{"*"}) {
		t.Fatal("wildcard scope should allow UpdateProject")
	}
	if !APIKeyScopeAllowed(updateProject, []string{"all"}) {
		t.Fatal("all scope should allow UpdateProject")
	}
	if APIKeyScopeAllowed(updateProject, []string{"projects.read"}) {
		t.Fatal("projects.read scope must NOT allow UpdateProject")
	}
	if APIKeyScopeAllowed(updateProject, []string{"users"}) {
		t.Fatal("unrelated scope must NOT allow UpdateProject")
	}

	deleteProject := "/torchwood.server.v1.ProjectsService/DeleteProject"
	if !APIKeyScopeAllowed(deleteProject, []string{"projects.write"}) {
		t.Fatal("projects.write scope should allow DeleteProject")
	}
	if APIKeyScopeAllowed(deleteProject, []string{"projects.read"}) {
		t.Fatal("projects.read scope must NOT allow DeleteProject")
	}

	// Groups prefs：GetGroupPrefs 需要 groups.read，UpdateGroupPrefs 需要 groups.write。
	getGroupPrefs := "/torchwood.server.v1.GroupsService/GetGroupPrefs"
	updateGroupPrefs := "/torchwood.server.v1.GroupsService/UpdateGroupPrefs"
	if !APIKeyScopeAllowed(getGroupPrefs, []string{"groups.read"}) {
		t.Fatal("groups.read scope should allow GetGroupPrefs")
	}
	if APIKeyScopeAllowed(getGroupPrefs, []string{"groups.write"}) {
		t.Fatal("groups.write scope must NOT allow GetGroupPrefs")
	}
	if APIKeyScopeAllowed(updateGroupPrefs, []string{"groups.read"}) {
		t.Fatal("groups.read scope must NOT allow UpdateGroupPrefs")
	}
	if !APIKeyScopeAllowed(updateGroupPrefs, []string{"groups.write"}) {
		t.Fatal("groups.write scope should allow UpdateGroupPrefs")
	}
	if !APIKeyScopeAllowed(getGroupPrefs, []string{"groups"}) {
		t.Fatal("bare groups scope should allow GetGroupPrefs")
	}
	if !APIKeyScopeAllowed(updateGroupPrefs, []string{"groups"}) {
		t.Fatal("bare groups scope should allow UpdateGroupPrefs")
	}
	if APIKeyScopeAllowed(updateGroupPrefs, []string{"users"}) {
		t.Fatal("unrelated scope must NOT allow UpdateGroupPrefs")
	}

	getUsage := "/torchwood.server.v1.BillingService/GetUsage"
	if !APIKeyScopeAllowed(getUsage, []string{"billing.read"}) {
		t.Fatal("billing.read scope should allow GetUsage")
	}
	if !APIKeyScopeAllowed(getUsage, []string{"billing"}) {
		t.Fatal("bare billing scope should allow GetUsage")
	}
	if APIKeyScopeAllowed(getUsage, []string{"billing.write"}) {
		t.Fatal("billing.write must NOT allow GetUsage")
	}
	if APIKeyScopeAllowed(getUsage, []string{"payments.read"}) {
		t.Fatal("payments.read must NOT allow GetUsage")
	}
}

func TestValidAPIKeyScope(t *testing.T) {
	t.Parallel()

	for _, s := range []string{
		"*", "all",
		"databases", "users", "groups", "storage", "projects", "oauthproviders", "apikeys",
		"databases.read", "databases.write",
		"storage.read", "storage.write",
		"users.read", "users.write",
		"groups.read", "groups.write",
		"projects.read", "projects.write",
		"oauthproviders.read", "oauthproviders.write",
		"apikeys.read", "apikeys.write",
		"payments", "payments.read", "payments.write",
		"economy", "economy.read", "economy.write",
		"subscriptions", "subscriptions.read", "subscriptions.write",
		"billing", "billing.read", "billing.write",
	} {
		if !ValidAPIKeyScope(s) {
			t.Fatalf("scope %q should be valid", s)
		}
	}

	for _, s := range []string{"", "foo", "health", "health.read", "databases.delete", "databases.read.extra", "any", "users.read.write"} {
		if ValidAPIKeyScope(s) {
			t.Fatalf("scope %q should be invalid", s)
		}
	}
}

func TestIsAPIKeysServiceMethod(t *testing.T) {
	t.Parallel()

	if !IsAPIKeysServiceMethod("/torchwood.server.v1.APIKeysService/CreateAPIKey") {
		t.Fatal("APIKeysService method should be detected")
	}
	if IsAPIKeysServiceMethod("/torchwood.server.v1.UsersService/ListUsers") {
		t.Fatal("UsersService method should not be detected")
	}
	if IsAPIKeysServiceMethod("malformed") {
		t.Fatal("malformed method should not be detected")
	}
}

// mustPanic 断言 fn 触发 panic 并返回 panic 值（测试内联辅助）。
func mustPanic(t *testing.T, fn func()) (v any) {
	t.Helper()
	defer func() {
		v = recover()
		if v == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	fn()
	return nil
}

// TestAssertAPIKeyScopeCoverage 直接测断言函数本身：集合一致不 panic；
// 缺一条（新增 ACCESS_API_KEY 方法未登记）或多余一条（规则表残留）都 panic。
func TestAssertAPIKeyScopeCoverage(t *testing.T) {
	t.Parallel()

	ruleMethods := make([]string, 0, len(apiKeyScopeRules))
	for m := range apiKeyScopeRules {
		ruleMethods = append(ruleMethods, m)
	}
	if len(ruleMethods) == 0 {
		t.Fatal("apiKeyScopeRules must not be empty")
	}

	t.Run("exact match does not panic", func(t *testing.T) {
		t.Parallel()
		AssertAPIKeyScopeCoverage(ruleMethods)
	})

	t.Run("missing rule for proto method panics", func(t *testing.T) {
		t.Parallel()
		missing := ruleMethods[:len(ruleMethods)-1]
		v := mustPanic(t, func() { AssertAPIKeyScopeCoverage(missing) })
		if msg, ok := v.(string); !ok || msg == "" {
			t.Fatalf("panic value should be a non-empty message, got %#v", v)
		}
	})

	t.Run("extra rule panics", func(t *testing.T) {
		t.Parallel()
		extra := append([]string{"/torchwood.server.v1.StaleService/RemovedMethod"}, ruleMethods...)
		v := mustPanic(t, func() { AssertAPIKeyScopeCoverage(extra) })
		msg, ok := v.(string)
		if !ok || msg == "" {
			t.Fatalf("panic value should be a non-empty message, got %#v", v)
		}
	})

	t.Run("APIKeyScopeRules returns full exported copy", func(t *testing.T) {
		t.Parallel()
		exported := APIKeyScopeRules()
		if len(exported) != len(apiKeyScopeRules) {
			t.Fatalf("exported rules size %d != internal %d", len(exported), len(apiKeyScopeRules))
		}
		for m, r := range apiKeyScopeRules {
			er, ok := exported[m]
			if !ok {
				t.Fatalf("exported rules missing %s", m)
			}
			if er.Resource != r.resource || er.Op != r.op {
				t.Fatalf("exported rule %s = %+v, want %+v", m, er, r)
			}
		}
	})
}
