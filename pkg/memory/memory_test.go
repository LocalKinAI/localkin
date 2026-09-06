package memory

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/LocalKinAI/kinclaw/pkg/brain"
)

func openTestDB(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	store, err := OpenMemory(path)
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpenMemory_CreatesDB(t *testing.T) {
	store := openTestDB(t)
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestOpenMemory_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "test.db")
	store, err := OpenMemory(path)
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	store.Close()
}

func TestSaveMessage_And_LoadHistory(t *testing.T) {
	store := openTestDB(t)
	session := "test-session-1"

	msg1 := brain.Message{Role: brain.RoleUser, Content: "Hello"}
	msg2 := brain.Message{Role: brain.RoleAssistant, Content: "Hi there"}

	if err := store.SaveMessage(session, msg1); err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}
	if err := store.SaveMessage(session, msg2); err != nil {
		t.Fatalf("SaveMessage failed: %v", err)
	}

	history := store.LoadHistory(session, 50)
	if len(history) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(history))
	}
	if history[0].Role != brain.RoleUser || history[0].Content != "Hello" {
		t.Errorf("first message: got role=%q content=%q", history[0].Role, history[0].Content)
	}
	if history[1].Role != brain.RoleAssistant || history[1].Content != "Hi there" {
		t.Errorf("second message: got role=%q content=%q", history[1].Role, history[1].Content)
	}
}

func TestLoadHistory_DefaultLimit(t *testing.T) {
	store := openTestDB(t)
	session := "limit-test"

	// Save more than default limit (0 => 50)
	for i := 0; i < 5; i++ {
		store.SaveMessage(session, brain.Message{Role: brain.RoleUser, Content: "msg"})
	}

	// Passing 0 should use default limit of 50
	history := store.LoadHistory(session, 0)
	if len(history) != 5 {
		t.Errorf("expected 5 messages, got %d", len(history))
	}
}

func TestLoadHistory_RespectLimit(t *testing.T) {
	store := openTestDB(t)
	session := "limit-test-2"

	for i := 0; i < 10; i++ {
		store.SaveMessage(session, brain.Message{Role: brain.RoleUser, Content: "msg"})
	}

	history := store.LoadHistory(session, 3)
	if len(history) != 3 {
		t.Errorf("expected 3 messages, got %d", len(history))
	}
}

func TestLoadHistory_OrderChronological(t *testing.T) {
	store := openTestDB(t)
	session := "order-test"

	store.SaveMessage(session, brain.Message{Role: brain.RoleUser, Content: "first"})
	store.SaveMessage(session, brain.Message{Role: brain.RoleUser, Content: "second"})
	store.SaveMessage(session, brain.Message{Role: brain.RoleUser, Content: "third"})

	history := store.LoadHistory(session, 10)
	if len(history) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(history))
	}
	if history[0].Content != "first" {
		t.Errorf("expected first message 'first', got %q", history[0].Content)
	}
	if history[2].Content != "third" {
		t.Errorf("expected last message 'third', got %q", history[2].Content)
	}
}

func TestLoadHistory_SessionIsolation(t *testing.T) {
	store := openTestDB(t)

	store.SaveMessage("session-a", brain.Message{Role: brain.RoleUser, Content: "msg-a"})
	store.SaveMessage("session-b", brain.Message{Role: brain.RoleUser, Content: "msg-b"})

	histA := store.LoadHistory("session-a", 50)
	histB := store.LoadHistory("session-b", 50)
	if len(histA) != 1 || histA[0].Content != "msg-a" {
		t.Errorf("session-a: unexpected history %v", histA)
	}
	if len(histB) != 1 || histB[0].Content != "msg-b" {
		t.Errorf("session-b: unexpected history %v", histB)
	}
}

func TestLoadHistory_EmptySession(t *testing.T) {
	store := openTestDB(t)
	history := store.LoadHistory("nonexistent", 50)
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d messages", len(history))
	}
}

func TestSaveMessage_WithToolCalls(t *testing.T) {
	store := openTestDB(t)
	session := "tool-test"

	msg := brain.Message{
		Role:    brain.RoleAssistant,
		Content: "Let me check that.",
		ToolCalls: []brain.ToolCall{
			{
				ID:   "tc-1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "shell", Arguments: `{"command":"echo hi"}`},
			},
		},
	}
	// Tool calls only load back as part of a complete group (a lone
	// tool_call is exactly what SanitizeHistory drops), so bracket it.
	group := []brain.Message{
		{Role: brain.RoleUser, Content: "check"},
		msg,
		{Role: brain.RoleTool, Content: "hi", ToolCallID: "tc-1"},
	}
	if err := store.SaveMessages(session, group); err != nil {
		t.Fatalf("SaveMessages with tool calls failed: %v", err)
	}

	history := store.LoadHistory(session, 10)
	if len(history) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(history))
	}
	if len(history[1].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(history[1].ToolCalls))
	}
	if history[1].ToolCalls[0].Function.Name != "shell" {
		t.Errorf("expected tool name 'shell', got %q", history[1].ToolCalls[0].Function.Name)
	}
}

func TestSaveMessage_WithToolCallID(t *testing.T) {
	store := openTestDB(t)
	session := "tool-result-test"

	// A tool result only survives LoadHistory as part of a complete
	// group (user → assistant tool_call → tool result), so save the
	// whole group and check the id round-trips on the last row.
	call := brain.ToolCall{ID: "tc-42", Type: "function"}
	call.Function.Name = "shell"
	call.Function.Arguments = "{}"
	group := []brain.Message{
		{Role: brain.RoleUser, Content: "run it"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{call}},
		{Role: brain.RoleTool, Content: "hello", ToolCallID: "tc-42"},
	}
	if err := store.SaveMessages(session, group); err != nil {
		t.Fatalf("SaveMessages with tool_call_id failed: %v", err)
	}

	history := store.LoadHistory(session, 10)
	if len(history) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(history))
	}
	if history[2].ToolCallID != "tc-42" {
		t.Errorf("expected tool_call_id 'tc-42', got %q", history[2].ToolCallID)
	}
}

func TestSave_And_Recall(t *testing.T) {
	store := openTestDB(t)

	result, err := store.Save("user_name", "Alice")
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if !strings.Contains(result, "Saved memory") {
		t.Errorf("unexpected save result: %q", result)
	}

	recalled, err := store.Recall("user_name")
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if !strings.Contains(recalled, "user_name") {
		t.Errorf("expected recall to contain key, got %q", recalled)
	}
	if !strings.Contains(recalled, "Alice") {
		t.Errorf("expected recall to contain value 'Alice', got %q", recalled)
	}
}

func TestSave_Upsert(t *testing.T) {
	store := openTestDB(t)

	store.Save("color", "blue")
	store.Save("color", "red")

	recalled, err := store.Recall("color")
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if !strings.Contains(recalled, "red") {
		t.Errorf("expected upserted value 'red', got %q", recalled)
	}
	// Should only have one entry for "color"
	count := strings.Count(recalled, "[color]")
	if count != 1 {
		t.Errorf("expected exactly 1 entry for 'color', found %d", count)
	}
}

func TestRecall_NoMatch(t *testing.T) {
	store := openTestDB(t)

	result, err := store.Recall("nonexistent_query")
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if !strings.Contains(result, "No memories found") {
		t.Errorf("expected no memories message, got %q", result)
	}
}

func TestRecall_SearchByValue(t *testing.T) {
	store := openTestDB(t)

	store.Save("pet", "golden retriever")
	store.Save("food", "spaghetti")

	recalled, err := store.Recall("retriever")
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if !strings.Contains(recalled, "golden retriever") {
		t.Errorf("expected to find 'golden retriever' by value search, got %q", recalled)
	}
}

func TestRecall_MultipleResults(t *testing.T) {
	store := openTestDB(t)

	store.Save("project_name", "LocalKin")
	store.Save("project_version", "1.0")

	recalled, err := store.Recall("project")
	if err != nil {
		t.Fatalf("Recall failed: %v", err)
	}
	if !strings.Contains(recalled, "project_name") || !strings.Contains(recalled, "project_version") {
		t.Errorf("expected both project entries, got %q", recalled)
	}
}

func TestDefaultDBPath(t *testing.T) {
	path := DefaultDBPath()
	if !strings.Contains(path, ".localkin") {
		t.Errorf("expected path to contain '.localkin', got %q", path)
	}
	if !strings.HasSuffix(path, "memory.db") {
		t.Errorf("expected path to end with 'memory.db', got %q", path)
	}
}

// TestClearTransientMemories: '_' prefixed keys get nuked, bare keys
// survive. The whole point of the new-session bug fix.
func TestClearTransientMemories(t *testing.T) {
	store := openTestDB(t)
	// 2 durable user facts + 3 transient working scratches.
	store.Save("daughter_name", "Mei")
	store.Save("home_city", "SF")
	store.Save("_finding_1", "zillow.com/abc: SOMA 1BR")
	store.Save("_finding_2", "apartments.com/xyz: Marina")
	store.Save("_draft_report", "## Apartments\n…")

	if err := store.ClearTransientMemories(); err != nil {
		t.Fatalf("ClearTransientMemories: %v", err)
	}

	all, err := store.AllMemories()
	if err != nil {
		t.Fatalf("AllMemories: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 durable rows after clear, got %d: %+v", len(all), all)
	}
	keys := map[string]bool{}
	for _, m := range all {
		keys[m.Key] = true
		if strings.HasPrefix(m.Key, "_") {
			t.Errorf("transient key leaked into AllMemories: %q", m.Key)
		}
	}
	if !keys["daughter_name"] || !keys["home_city"] {
		t.Errorf("durable facts missing after transient clear: %+v", keys)
	}

	// Recall by an exact transient key should miss now.
	got, _ := store.Recall("_finding_1")
	if !strings.Contains(got, "No memories") {
		t.Errorf("expected transient _finding_1 gone post-clear, got: %q", got)
	}

	// Idempotent — second clear is a no-op.
	if err := store.ClearTransientMemories(); err != nil {
		t.Errorf("second clear should be no-op, got error: %v", err)
	}
}

// TestAllMemories_FiltersTransient: even WITHOUT calling clear,
// AllMemories() (which feeds the system-prompt boot dump) hides
// '_'-prefixed keys. Belt-and-suspenders for older kernel builds
// or DBs migrated from before the convention.
func TestAllMemories_FiltersTransient(t *testing.T) {
	store := openTestDB(t)
	store.Save("user_name", "Jacky")
	store.Save("_scratch_1", "ephemeral")

	all, _ := store.AllMemories()
	if len(all) != 1 {
		t.Fatalf("expected only durable to surface, got %d: %+v", len(all), all)
	}
	if all[0].Key != "user_name" {
		t.Errorf("expected user_name, got %q", all[0].Key)
	}
}

func TestLoadHistory_SanitizesWindowOpeningMidToolGroup(t *testing.T) {
	store := openTestDB(t)
	sid := "sanitize-test"
	tc := brain.ToolCall{ID: "call-1", Type: "function"}
	tc.Function.Name = "ui"
	tc.Function.Arguments = "{}"
	msgs := []brain.Message{
		{Role: brain.RoleUser, Content: "first"},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{tc}},
		{Role: brain.RoleTool, ToolCallID: "call-1", Content: "tree"},
		{Role: brain.RoleAssistant, Content: "done"},
		{Role: brain.RoleUser, Content: "second"},
		{Role: brain.RoleAssistant, Content: "ok"},
	}
	for _, m := range msgs {
		if err := store.SaveMessage(sid, m); err != nil {
			t.Fatal(err)
		}
	}
	// Limit 4 → window opens on the tool result (orphan).
	got := store.LoadHistory(sid, 4)
	if len(got) != 2 || got[0].Content != "second" {
		t.Fatalf("expected sanitized tail [second, ok], got %+v", got)
	}
}

func TestArchiveSessionAndSessions(t *testing.T) {
	store := openTestDB(t)
	store.SaveMessage("KinClaw Pilot", brain.Message{Role: brain.RoleUser, Content: "hi"})
	store.SaveMessage("KinClaw Pilot", brain.Message{Role: brain.RoleAssistant, Content: "hello"})
	archive, n, err := store.ArchiveSession("KinClaw Pilot")
	if err != nil || n != 2 || !strings.HasPrefix(archive, "KinClaw Pilot@") {
		t.Fatalf("archive: %q n=%d err=%v", archive, n, err)
	}
	if h := store.LoadHistory("KinClaw Pilot", 50); len(h) != 0 {
		t.Fatalf("live key should be empty after archive, got %d", len(h))
	}
	if h := store.LoadHistory(archive, 50); len(h) != 2 {
		t.Fatalf("archive should hold the rows, got %d", len(h))
	}
	sessions, err := store.Sessions()
	if err != nil || len(sessions) != 1 || sessions[0].ID != archive || sessions[0].Messages != 2 {
		t.Fatalf("sessions: %+v err=%v", sessions, err)
	}
}

func TestListAndForgetMemories(t *testing.T) {
	store := openTestDB(t)
	store.Save("home_city", "Mountain View")
	store.Save("_scratch", "tmp")
	durable, _ := store.ListMemories(false)
	all, _ := store.ListMemories(true)
	if len(durable) != 1 || len(all) != 2 {
		t.Fatalf("durable=%d all=%d", len(durable), len(all))
	}
	ok, err := store.Forget("home_city")
	if err != nil || !ok {
		t.Fatalf("forget: ok=%v err=%v", ok, err)
	}
	if ok, _ := store.Forget("home_city"); ok {
		t.Fatal("second forget should report not found")
	}
}
