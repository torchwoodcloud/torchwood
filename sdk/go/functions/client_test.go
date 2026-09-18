package functions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newIdentityContext 构建携带执行身份的 ctx（apiBaseUrl 指向 fake 平台）。
func newIdentityContext(baseURL string) context.Context {
	return WithIdentity(context.Background(), Identity{
		ExecutionToken: testToken,
		APIBaseURL:     baseURL,
		ExecutionID:    testExecID,
		Source:         testSource,
		InvokingUserID: testUserID,
		ProjectID:      testProjectID,
	})
}

func TestClientCreateDocument(t *testing.T) {
	var gotAuth, gotCT, gotPath, gotBody string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"doc-new","data":{"k":"v"},"version":"1",` +
			`"createdAt":"2026-09-18T01:02:03Z","updatedAt":"2026-09-18T01:02:04Z"}`))
	}))
	defer fake.Close()

	c := NewClient(newIdentityContext(fake.URL))
	docID, err := c.CreateDocument(context.Background(), "app", "users", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if docID != "doc-new" {
		t.Fatalf("docID = %q, want doc-new", docID)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("auth = %q, want bearer execution token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	if gotPath != "/v1/server/databases/app/collections/users/documents" {
		t.Fatalf("path = %q", gotPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("request body %q: %v", gotBody, err)
	}
	if body["data"].(map[string]any)["k"] != "v" {
		t.Fatalf("request body = %q, want {\"data\":{\"k\":\"v\"}}", gotBody)
	}
}

func TestClientGetDocument(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/server/databases/app/collections/users/documents/doc-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// protojson 形状：version 为 int64 字符串化。
		_, _ = w.Write([]byte(`{"id":"doc-1","data":{"name":"Ann"},"permissions":["read"],` +
			`"version":"3","createdAt":"2026-09-18T01:02:03Z"}`))
	}))
	defer fake.Close()

	c := NewClient(newIdentityContext(fake.URL))
	rec, err := c.GetDocument(context.Background(), "app", "users", "doc-1")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if rec.ID != "doc-1" || rec.Data["name"] != "Ann" || rec.Version != 3 {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Permissions) != 1 || rec.Permissions[0] != "read" {
		t.Fatalf("permissions = %v", rec.Permissions)
	}
	if !rec.CreatedAt.Equal(time.Date(2026, 9, 18, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("createdAt = %s", rec.CreatedAt)
	}
}

func TestClientVersionAsNumber(t *testing.T) {
	// encoding/json 形态（version 为数字）也兼容。
	var rec DocumentRecord
	if err := json.Unmarshal([]byte(`{"id":"d","version":9}`), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Version != 9 {
		t.Fatalf("version = %d, want 9", rec.Version)
	}
}

func TestClientAPIError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"PERMISSION.DENIED"}`, http.StatusForbidden)
	}))
	defer fake.Close()

	c := NewClient(newIdentityContext(fake.URL))
	_, err := c.GetDocument(context.Background(), "app", "users", "doc-1")
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden || !strings.Contains(apiErr.Body, "PERMISSION.DENIED") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "403 Forbidden") {
		t.Fatalf("Error() = %q", apiErr.Error())
	}
}

func TestClientDoGeneric(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/server/assets/defs" || r.Method != http.MethodPost {
			t.Errorf("call = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer fake.Close()

	c := NewClient(newIdentityContext(fake.URL))
	var out struct {
		Items []any `json:"items"`
	}
	if err := c.Do(context.Background(), http.MethodPost, "/v1/server/assets/defs", map[string]any{"limit": 10}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Items == nil {
		t.Fatal("out.items not decoded")
	}
}

func TestClientWithoutIdentityFailsCleanly(t *testing.T) {
	c := NewClient(context.Background())
	_, err := c.GetDocument(context.Background(), "app", "users", "doc-1")
	if err == nil {
		t.Fatal("expected error without identity")
	}
	if strings.Contains(err.Error(), "nil pointer") {
		t.Fatalf("error should be a clean request error, got %v", err)
	}
}
