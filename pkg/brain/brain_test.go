package brain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewBrain_ClaudeDefault(t *testing.T) {
	b := NewBrain("claude", "", "test-model", "sk-test", 0.7)
	if b == nil {
		t.Fatal("expected non-nil brain")
	}
	if _, ok := b.(*claudeBrain); !ok {
		t.Errorf("expected *claudeBrain, got %T", b)
	}
}

func TestNewBrain_OpenAI(t *testing.T) {
	b := NewBrain("openai", "", "gpt-4", "sk-test", 0.5)
	if b == nil {
		t.Fatal("expected non-nil brain")
	}
	if _, ok := b.(*openAIBrain); !ok {
		t.Errorf("expected *openAIBrain, got %T", b)
	}
}

func TestNewBrain_Ollama(t *testing.T) {
	b := NewBrain("ollama", "", "llama3", "", 0.7)
	if b == nil {
		t.Fatal("expected non-nil brain")
	}
	if _, ok := b.(*openAIBrain); !ok {
		t.Errorf("expected *openAIBrain for ollama provider, got %T", b)
	}
}

func TestNewBrain_UnknownDefaultsToClaude(t *testing.T) {
	b := NewBrain("unknown", "", "model", "key", 0.5)
	if _, ok := b.(*claudeBrain); !ok {
		t.Errorf("expected unknown provider to default to *claudeBrain, got %T", b)
	}
}

func TestNewClaudeBrain_DefaultEndpoint(t *testing.T) {
	b := NewClaudeBrain("", "model", "key", 0.7)
	cb := b.(*claudeBrain)
	if cb.baseURL != "https://api.anthropic.com" {
		t.Errorf("expected default endpoint, got %q", cb.baseURL)
	}
}

func TestNewClaudeBrain_CustomEndpoint(t *testing.T) {
	b := NewClaudeBrain("http://localhost:8080/", "model", "key", 0.7)
	cb := b.(*claudeBrain)
	if cb.baseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash stripped, got %q", cb.baseURL)
	}
}

func TestNewOpenAIBrain_DefaultEndpoint(t *testing.T) {
	b := NewOpenAIBrain("", "gpt-4", "key", 0.5)
	ob := b.(*openAIBrain)
	if ob.baseURL != "https://api.openai.com" {
		t.Errorf("expected default endpoint, got %q", ob.baseURL)
	}
}

func TestToolCall_ParseArguments(t *testing.T) {
	tc := ToolCall{
		ID:   "tc-1",
		Type: "function",
	}
	tc.Function.Name = "shell"
	tc.Function.Arguments = `{"command":"echo hello","verbose":"true"}`

	params, err := tc.ParseArguments()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params["command"] != "echo hello" {
		t.Errorf("expected command 'echo hello', got %q", params["command"])
	}
	if params["verbose"] != "true" {
		t.Errorf("expected verbose 'true', got %q", params["verbose"])
	}
}

func TestToolCall_ParseArguments_Invalid(t *testing.T) {
	tc := ToolCall{}
	tc.Function.Arguments = "not json"
	_, err := tc.ParseArguments()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestToolCall_ParseArguments_NumericValues(t *testing.T) {
	tc := ToolCall{}
	tc.Function.Arguments = `{"count":42,"ratio":3.14}`
	params, err := tc.ParseArguments()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params["count"] != "42" {
		t.Errorf("expected count '42', got %q", params["count"])
	}
	if params["ratio"] != "3.14" {
		t.Errorf("expected ratio '3.14', got %q", params["ratio"])
	}
}

func TestRetryDo_ImmediateSuccess(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := srv.Client()
	resp, err := retryDo(client, func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryDo_RetriesOn429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := srv.Client()
	resp, err := retryDo(client, func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after retries, got %d", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRetryDo_RetriesOn500(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client := srv.Client()
	resp, err := retryDo(client, func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestRetryDo_NoRetryOnClientError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(400)
	}))
	defer srv.Close()

	client := srv.Client()
	resp, err := retryDo(client, func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on 400), got %d", calls)
	}
}

func TestRetryDo_MaxAttemptsExhausted(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Retry-After: 0 keeps the test instant; the backoff schedule
		// itself is covered by TestRetryDo_BacksOffOn429AndHonoursRetryAfter.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(503)
	}))
	defer srv.Close()

	client := srv.Client()
	resp, err := retryDo(client, func() (*http.Request, error) {
		return http.NewRequest("GET", srv.URL, nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("expected 503 after exhausting retries, got %d", resp.StatusCode)
	}
	if calls != 5 {
		t.Errorf("expected 5 attempts, got %d", calls)
	}
}

func TestConvertToolsToClaude(t *testing.T) {
	openaiTool := json.RawMessage(`{
		"type": "function",
		"function": {
			"name": "shell",
			"description": "Run a command",
			"parameters": {"type": "object", "properties": {"command": {"type": "string"}}}
		}
	}`)
	result := convertToolsToClaude([]json.RawMessage{openaiTool})
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	var claude struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(result[0], &claude); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if claude.Type != "custom" {
		t.Errorf("expected type 'custom', got %q", claude.Type)
	}
	if claude.Name != "shell" {
		t.Errorf("expected name 'shell', got %q", claude.Name)
	}
}

func TestConvertToolsToClaude_NonFunction(t *testing.T) {
	// Non-function type tools should be passed through unchanged
	tool := json.RawMessage(`{"type":"custom","name":"test"}`)
	result := convertToolsToClaude([]json.RawMessage{tool})
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if string(result[0]) != string(tool) {
		t.Errorf("expected passthrough, got %s", string(result[0]))
	}
}

func TestMessageRoleConstants(t *testing.T) {
	if RoleSystem != "system" {
		t.Errorf("RoleSystem = %q", RoleSystem)
	}
	if RoleUser != "user" {
		t.Errorf("RoleUser = %q", RoleUser)
	}
	if RoleAssistant != "assistant" {
		t.Errorf("RoleAssistant = %q", RoleAssistant)
	}
	if RoleTool != "tool" {
		t.Errorf("RoleTool = %q", RoleTool)
	}
}

func TestChatResult_ZeroValue(t *testing.T) {
	r := &ChatResult{}
	if r.Content != "" {
		t.Errorf("expected empty content, got %q", r.Content)
	}
	if r.ToolCalls != nil {
		t.Errorf("expected nil tool calls, got %v", r.ToolCalls)
	}
}

func TestRetryDo_NewReqError(t *testing.T) {
	client := &http.Client{}
	_, err := retryDo(client, func() (*http.Request, error) {
		return nil, fmt.Errorf("bad request factory")
	})
	if err == nil {
		t.Fatal("expected error from newReq")
	}
	if !strings.Contains(err.Error(), "bad request factory") {
		t.Errorf("unexpected error: %v", err)
	}
}
