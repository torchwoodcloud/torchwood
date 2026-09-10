package functions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domaindatabases "github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
)

// ——订阅串解析/校验（v3 §4.1：Appwrite 风格；一期 database 精确、collection
// 与 op 支持通配）——

func TestParseEventPattern(t *testing.T) {
	t.Run("合法形态", func(t *testing.T) {
		cases := []struct {
			in   string
			want EventPattern
		}{
			{
				in:   "databases.app.collections.notes.documents.create",
				want: EventPattern{DatabaseID: "app", CollectionID: "notes", Op: EventOpCreate},
			},
			{
				in:   "databases.app.collections.notes.documents.delete",
				want: EventPattern{DatabaseID: "app", CollectionID: "notes", Op: EventOpDelete},
			},
			{ // collection 级通配（设计 §4.1 规范形态 collections.*.documents.*）
				in:   "databases.app.collections.*.documents.*",
				want: EventPattern{DatabaseID: "app", CollectionID: "*", Op: "*"},
			},
			{ // 精确 collection + 任意 op（语法超集，自然允许）
				in:   "databases.app.collections.notes.documents.*",
				want: EventPattern{DatabaseID: "app", CollectionID: "notes", Op: "*"},
			},
			{ // 通配 collection + 精确 op
				in:   "databases.app.collections.*.documents.update",
				want: EventPattern{DatabaseID: "app", CollectionID: "*", Op: EventOpUpdate},
			},
		}
		for _, c := range cases {
			got, err := ParseEventPattern(c.in)
			require.NoError(t, err, c.in)
			require.Equal(t, c.want, got, c.in)
		}
	})

	t.Run("非法形态", func(t *testing.T) {
		bad := []string{
			"",                                // 空
			"databases.app.collections.notes", // 段数不足
			"database.app.collections.notes.documents.create",                                // 固定段拼错
			"databases.app.collection.notes.documents.create",                                // 固定段拼错
			"databases.app.collections.notes.document.create",                                // 固定段拼错
			"databases..collections.notes.documents.create",                                  // database 空
			"databases.*.collections.notes.documents.create",                                 // database 通配（一期拒绝）
			"databases.app.collections..documents.create",                                    // collection 空
			"databases.app.collections.notes.documents.upsert",                               // op 词表外
			"databases.app.collections.notes.documents.",                                     // op 空
			"databases." + string(make([]byte, 300)) + ".collections.notes.documents.create", // 超长
		}
		for _, s := range bad {
			_, err := ParseEventPattern(s)
			require.Error(t, err, s)
		}
	})
}

func TestValidateEventPatterns(t *testing.T) {
	require.Error(t, ValidateEventPatterns(nil), "空列表拒绝")
	require.Error(t, ValidateEventPatterns([]string{"databases.a.collections.b.documents.create", "bad"}))
	require.NoError(t, ValidateEventPatterns([]string{
		"databases.a.collections.b.documents.create",
		"databases.a.collections.*.documents.*",
	}))
	// 超过上限拒绝。
	many := make([]string, MaxEventPatterns+1)
	for i := range many {
		many[i] = "databases.a.collections.b.documents.create"
	}
	require.Error(t, ValidateEventPatterns(many))
}

// ——匹配语义（任务验收：精确命中、通配命中、op 不匹配、database 不匹配）——

func TestEventPatternMatches(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		db      string
		coll    string
		op      string
		want    bool
	}{
		{"精确命中", "databases.app.collections.notes.documents.create", "app", "notes", "create", true},
		{"op 不匹配", "databases.app.collections.notes.documents.create", "app", "notes", "update", false},
		{"collection 不匹配", "databases.app.collections.notes.documents.create", "app", "other", "create", false},
		{"database 不匹配", "databases.app.collections.notes.documents.create", "other", "notes", "create", false},
		{"通配命中任意 collection/op", "databases.app.collections.*.documents.*", "app", "anything", "delete", true},
		{"通配 collection + 精确 op 命中", "databases.app.collections.*.documents.update", "app", "x", "update", true},
		{"通配 collection + 精确 op 不匹配", "databases.app.collections.*.documents.update", "app", "x", "create", false},
		{"database 恒精确", "databases.app.collections.*.documents.*", "other", "x", "create", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseEventPattern(c.pattern)
			require.NoError(t, err)
			require.Equal(t, c.want, p.Matches(c.db, c.coll, c.op))
		})
	}
}

func TestEventOpFromEnvelope(t *testing.T) {
	require.Equal(t, EventOpCreate, EventOpFromEnvelope(domainevents.EventDocumentsCreate))
	require.Equal(t, EventOpUpdate, EventOpFromEnvelope(domainevents.EventDocumentsUpdate))
	require.Equal(t, EventOpDelete, EventOpFromEnvelope(domainevents.EventDocumentsDelete))
	require.Equal(t, "", EventOpFromEnvelope("databases.documents.weird"))
	require.Equal(t, "", EventOpFromEnvelope("nodot"))
}

// ——匹配器索引（project → 订阅条目；worker 周期快照换入）——

func TestEventTriggerIndexMatchAndSwap(t *testing.T) {
	triggers := []Trigger{
		{
			ID: "trg_1", ProjectID: "p1", FunctionID: "fn_1", Type: TriggerTypeEvent, Enabled: true,
			Config: TriggerConfig{Events: []string{
				"databases.app.collections.notes.documents.create",
				"databases.app.collections.*.documents.delete",
			}},
		},
		{
			ID: "trg_2", ProjectID: "p1", FunctionID: "fn_2", Type: TriggerTypeEvent, Enabled: false, // 禁用不进索引
			Config: TriggerConfig{Events: []string{"databases.app.collections.*.documents.*"}},
		},
		{
			ID: "trg_3", ProjectID: "p2", FunctionID: "fn_3", Type: TriggerTypeEvent, Enabled: true,
			Config: TriggerConfig{Events: []string{"databases.app.collections.*.documents.*"}},
		},
		{ID: "trg_4", ProjectID: "p1", FunctionID: "fn_4", Type: TriggerTypeCron, Enabled: true}, // 非 event 忽略
	}
	idx := NewEventTriggerIndex(triggers)
	require.Equal(t, 3, idx.Len())

	// 精确 + 通配双命中（同一事件两触发器各投一次）。
	subs := idx.Match("p1", "app", "notes", "create")
	require.Len(t, subs, 1)
	require.Equal(t, "trg_1", subs[0].TriggerID)
	require.Equal(t, "fn_1", subs[0].FunctionID)

	subs = idx.Match("p1", "app", "logs", "delete")
	require.Len(t, subs, 1, "通配 delete 命中 trg_1 第二条订阅")
	require.Equal(t, "trg_1", subs[0].TriggerID)

	// 项目隔离（B-1）：跨项目同名集合不串。
	subs = idx.Match("p2", "app", "notes", "create")
	require.Len(t, subs, 1)
	require.Equal(t, "trg_3", subs[0].TriggerID)

	// 未命中。
	require.Empty(t, idx.Match("p1", "app", "notes", "update"))
	require.Empty(t, idx.Match("p_missing", "app", "notes", "create"))

	// nil 安全。
	var nilIdx *EventTriggerIndex
	require.Empty(t, nilIdx.Match("p1", "app", "notes", "create"))
	require.Equal(t, 0, nilIdx.Len())

	// 换入换出：重建索引后原子替换——旧查询不受影响（快照语义），
	// 新快照立即可见。
	triggers[1].Enabled = true
	idx2 := NewEventTriggerIndex(triggers)
	require.Equal(t, 4, idx2.Len(), "trg_1×2 + trg_2×1 + trg_3×1")
	subs = idx2.Match("p1", "app", "anything", "update")
	require.Len(t, subs, 1, "trg_1 通配仅 delete 不命中 update；trg_2 通配全命中")
	require.Equal(t, "trg_2", subs[0].TriggerID)
	_ = idx // 旧快照引用仍持有原数据（atomic.Pointer 换入换出的测试语义）
}

// ——投影（§4.2：32KB 预算、两级截断分名）——

func docOf(size int) *domaindatabases.Document {
	blob := make([]byte, size)
	for i := range blob {
		blob[i] = 'x'
	}
	return &domaindatabases.Document{
		ID: "doc_1", Version: 7,
		Data:      map[string]any{"blob": string(blob)},
		CreatedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
	}
}

func TestBuildEventInvocationData(t *testing.T) {
	t.Run("完整投影（含文档 data）", func(t *testing.T) {
		ev := &domainevents.Envelope{
			EventID: "ev1", Event: domainevents.EventDocumentsUpdate, Seq: 42,
			ProjectID: "p1", DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_1",
			Version: 7, Data: docOf(256),
		}
		out, err := BuildEventInvocationData(ev)
		require.NoError(t, err)
		parsed := parseInvocation(t, out)
		require.Equal(t, "event", parsed["type"])
		require.Equal(t, "ev1", parsed["event_id"])
		require.Equal(t, float64(42), parsed["seq"])
		require.Equal(t, "app", parsed["database_id"])
		require.Equal(t, "notes", parsed["collection_id"])
		require.Equal(t, "doc_1", parsed["document_id"])
		require.Equal(t, float64(7), parsed["version"])
		require.Equal(t, false, parsed["envelope_truncated"])
		require.Contains(t, parsed, "data")
		require.NotContains(t, parsed, "data_truncated")
		// 投影 data 与 REST Document 同形（id/data/version）。
		dataMap, ok := parsed["data"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "doc_1", dataMap["id"])
	})

	t.Run("delete 事件无 data", func(t *testing.T) {
		ev := &domainevents.Envelope{
			EventID: "ev2", Event: domainevents.EventDocumentsDelete, Seq: 43,
			ProjectID: "p1", DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_1",
			Version: 8, // delete：Data == nil
		}
		out, err := BuildEventInvocationData(ev)
		require.NoError(t, err)
		parsed := parseInvocation(t, out)
		require.NotContains(t, parsed, "data")
		require.NotContains(t, parsed, "data_truncated")
	})

	t.Run("信封级截断分名（envelope_truncated）", func(t *testing.T) {
		ev := &domainevents.Envelope{
			EventID: "ev3", Event: domainevents.EventDocumentsCreate, Seq: 44,
			ProjectID: "p1", DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_1",
			Version: 9, Truncated: true, Data: docOf(128),
		}
		out, err := BuildEventInvocationData(ev)
		require.NoError(t, err)
		parsed := parseInvocation(t, out)
		require.Equal(t, true, parsed["envelope_truncated"], "信封级截断独立承载")
		require.Contains(t, parsed, "data", "小文档投影仍尽力塞入")
		require.NotContains(t, parsed, "data_truncated", "两级截断语义不混")
	})

	t.Run("投影级截断（data_truncated 剥 data）", func(t *testing.T) {
		ev := &domainevents.Envelope{
			EventID: "ev4", Event: domainevents.EventDocumentsUpdate, Seq: 45,
			ProjectID: "p1", DatabaseID: "app", CollectionID: "notes", DocumentID: "doc_big",
			Version: 10, Data: docOf(64 << 10), // 64KB 文档：必超 32KB 预算
		}
		out, err := BuildEventInvocationData(ev)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out), EventDataBudgetBytes, "投影恒在 32KB 预算内")
		parsed := parseInvocation(t, out)
		require.NotContains(t, parsed, "data", "超限剥掉 data")
		require.Equal(t, true, parsed["data_truncated"], "投影级截断独立命名")
		require.Equal(t, "doc_big", parsed["document_id"], "ID 保留供 databases:read 回读")
	})
}

func parseInvocation(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m
}
