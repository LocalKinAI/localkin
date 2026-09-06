package brain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func call(id, name string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = "{}"
	return tc
}

func TestSanitizeHistory_DropsLeadingOrphanToolResult(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Content: "stale", ToolCallID: "t0"},
		{Role: RoleAssistant, Content: "stale reply"},
		{Role: RoleUser, Content: "hi"},
		{Role: RoleAssistant, Content: "hello"},
	}
	out := SanitizeHistory(in)
	if len(out) != 2 || out[0].Role != RoleUser || out[1].Content != "hello" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestSanitizeHistory_KeepsCompleteToolGroup(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "do it"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("a", "shell"), call("b", "ui")}},
		{Role: RoleTool, Content: "ok", ToolCallID: "b"},
		{Role: RoleTool, Content: "ok", ToolCallID: "a"},
		{Role: RoleAssistant, Content: "done"},
	}
	out := SanitizeHistory(in)
	if len(out) != 5 {
		t.Fatalf("expected 5 messages kept, got %d: %+v", len(out), out)
	}
}

func TestSanitizeHistory_DropsIncompleteToolGroup(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "do it"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("a", "shell"), call("b", "ui")}},
		{Role: RoleTool, Content: "ok", ToolCallID: "a"},
		// "b" never answered (interrupted turn)
		{Role: RoleUser, Content: "continue"},
	}
	out := SanitizeHistory(in)
	if len(out) != 2 || out[0].Content != "do it" || out[1].Content != "continue" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestSanitizeHistory_TrailingUnansweredCallDropped(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "go"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{call("a", "shell")}},
	}
	out := SanitizeHistory(in)
	if len(out) != 1 {
		t.Fatalf("expected trailing unanswered call dropped, got %+v", out)
	}
}

func TestEstimateTokens_CJKCountsPerCharacter(t *testing.T) {
	zh := EstimateText("帮我优化这个项目让它更好用")
	if zh < 12 || zh > 14 {
		t.Fatalf("expected ~13 tokens for 13 CJK chars, got %d", zh)
	}
	en := EstimateText("help me optimize this project")
	if en < 6 || en > 9 {
		t.Fatalf("expected ~7 tokens, got %d", en)
	}
	if EstimateText("") != 0 {
		t.Fatal("empty string should be 0")
	}
}

func TestRetryDo_BacksOffOn429AndHonoursRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	resp, err := retryDo(srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	})
	if err != nil || resp.StatusCode != 200 || hits != 3 {
		t.Fatalf("err=%v status=%d hits=%d", err, resp.StatusCode, hits)
	}
}

func TestRetryDo_StopsOnCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := retryDo(srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("expected prompt cancellation, err=%v after %s", err, time.Since(start))
	}
}
