package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Registry Tests ---

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()
	if r == nil {
		t.Fatal("expected non-nil registry")
	}
}

type mockSkill struct {
	name string
	desc string
}

func (m *mockSkill) Name() string        { return m.name }
func (m *mockSkill) Description() string { return m.desc }
func (m *mockSkill) ToolDef() json.RawMessage {
	return MakeToolDef(m.name, m.desc, nil, nil)
}
func (m *mockSkill) Execute(params map[string]string) (string, error) {
	return "mock result", nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	s := &mockSkill{name: "test_skill", desc: "A test skill"}
	r.Register(s)

	got, err := r.Get("test_skill")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Name() != "test_skill" {
		t.Errorf("expected name 'test_skill', got %q", got.Name())
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	r := NewRegistry()
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent skill")
	}
	if !strings.Contains(err.Error(), "skill not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRegistry_ToolDefs(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "a", desc: "skill a"})
	r.Register(&mockSkill{name: "b", desc: "skill b"})

	defs := r.ToolDefs()
	if len(defs) != 2 {
		t.Errorf("expected 2 tool defs, got %d", len(defs))
	}
}

func TestRegistry_FilteredToolDefs_Empty(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "a", desc: "skill a"})
	r.Register(&mockSkill{name: "b", desc: "skill b"})

	// Empty allow list returns all
	defs := r.FilteredToolDefs(nil)
	if len(defs) != 2 {
		t.Errorf("expected 2 (all) tool defs when allow is nil, got %d", len(defs))
	}
}

func TestRegistry_FilteredToolDefs_Selective(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "a", desc: "skill a"})
	r.Register(&mockSkill{name: "b", desc: "skill b"})
	r.Register(&mockSkill{name: "c", desc: "skill c"})

	defs := r.FilteredToolDefs([]string{"a", "c"})
	if len(defs) != 2 {
		t.Errorf("expected 2 filtered defs, got %d", len(defs))
	}
}

func TestRegistry_OverwriteSkill(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "x", desc: "first"})
	r.Register(&mockSkill{name: "x", desc: "second"})

	s, _ := r.Get("x")
	if s.Description() != "second" {
		t.Errorf("expected overwritten skill desc 'second', got %q", s.Description())
	}
	// Should still have only 1 tool def for "x"
	if len(r.ToolDefs()) != 1 {
		t.Errorf("expected 1 tool def after overwrite, got %d", len(r.ToolDefs()))
	}
}

// --- MakeToolDef Tests ---

func TestMakeToolDef(t *testing.T) {
	def := MakeToolDef("test_tool", "A test tool",
		map[string]map[string]string{
			"input": {"type": "string", "description": "The input"},
		},
		[]string{"input"},
	)
	var parsed struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  struct {
				Type       string                       `json:"type"`
				Properties map[string]map[string]string `json:"properties"`
				Required   []string                     `json:"required"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(def, &parsed); err != nil {
		t.Fatalf("failed to parse tool def: %v", err)
	}
	if parsed.Type != "function" {
		t.Errorf("expected type 'function', got %q", parsed.Type)
	}
	if parsed.Function.Name != "test_tool" {
		t.Errorf("expected name 'test_tool', got %q", parsed.Function.Name)
	}
	if parsed.Function.Parameters.Type != "object" {
		t.Errorf("expected params type 'object', got %q", parsed.Function.Parameters.Type)
	}
	if len(parsed.Function.Parameters.Required) != 1 || parsed.Function.Parameters.Required[0] != "input" {
		t.Errorf("unexpected required: %v", parsed.Function.Parameters.Required)
	}
}

func TestMakeToolDef_NoProperties(t *testing.T) {
	def := MakeToolDef("bare", "Bare tool", nil, nil)
	var parsed map[string]interface{}
	if err := json.Unmarshal(def, &parsed); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	fn := parsed["function"].(map[string]interface{})
	if fn["name"] != "bare" {
		t.Errorf("expected name 'bare', got %v", fn["name"])
	}
}

// --- ExecuteToolCalls Tests ---

func TestExecuteToolCalls_Single(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "mock", desc: "mock"})

	results := ExecuteToolCalls(r, []ToolCallInfo{
		{ID: "tc-1", Name: "mock", Params: map[string]string{}},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Output != "mock result" {
		t.Errorf("expected 'mock result', got %q", results[0].Output)
	}
	if results[0].ToolCallID != "tc-1" {
		t.Errorf("expected tool call id 'tc-1', got %q", results[0].ToolCallID)
	}
	if results[0].Err != nil {
		t.Errorf("unexpected error: %v", results[0].Err)
	}
}

func TestExecuteToolCalls_Multiple(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "a", desc: "a"})
	r.Register(&mockSkill{name: "b", desc: "b"})

	results := ExecuteToolCalls(r, []ToolCallInfo{
		{ID: "tc-1", Name: "a", Params: map[string]string{}},
		{ID: "tc-2", Name: "b", Params: map[string]string{}},
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Verify ordering is preserved
	if results[0].ToolCallID != "tc-1" || results[1].ToolCallID != "tc-2" {
		t.Errorf("result ordering not preserved")
	}
}

func TestExecuteToolCalls_SkillNotFound(t *testing.T) {
	r := NewRegistry()
	results := ExecuteToolCalls(r, []ToolCallInfo{
		{ID: "tc-1", Name: "nonexistent", Params: map[string]string{}},
	})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Error("expected error for nonexistent skill")
	}
	if !strings.Contains(results[0].Output, "skill not found") {
		t.Errorf("unexpected output: %q", results[0].Output)
	}
}

func TestExecuteToolCalls_Empty(t *testing.T) {
	r := NewRegistry()
	results := ExecuteToolCalls(r, nil)
	if results != nil {
		t.Errorf("expected nil for empty calls, got %v", results)
	}
}

// --- Native Skill Tests ---

func TestShellSkill_Echo(t *testing.T) {
	s := NewShellSkill(5)
	result, err := s.Execute(map[string]string{"command": "echo hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected output to contain 'hello', got %q", result)
	}
}

func TestShellSkill_EmptyCommand(t *testing.T) {
	s := NewShellSkill(5)
	_, err := s.Execute(map[string]string{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestShellSkill_BlockedCommands(t *testing.T) {
	s := NewShellSkill(5)
	blocked := []string{
		"rm -rf /",
		"mkfs.ext4 /dev/sda",
		"dd if=/dev/zero of=/dev/sda",
		":(){ :|:& };:",
		"shutdown -h now",
		"reboot",
		"halt",
	}
	for _, cmd := range blocked {
		_, err := s.Execute(map[string]string{"command": cmd})
		if err == nil {
			t.Errorf("expected command %q to be blocked", cmd)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("expected 'blocked' error for %q, got: %v", cmd, err)
		}
	}
}

func TestShellSkill_PipeBlocklist(t *testing.T) {
	s := NewShellSkill(5)
	blocked := []struct {
		cmd    string
		interp string
	}{
		{"cat file | bash", "bash"},
		{"cat file | sh", "sh"},
		{"cat file | python", "python"},
		{"cat file | perl", "perl"},
		{"cat file | ruby", "ruby"},
		{"echo x | bash -c 'rm /'", "bash"},
	}
	for _, tt := range blocked {
		_, err := s.Execute(map[string]string{"command": tt.cmd})
		if err == nil {
			t.Errorf("expected pipe to %s to be blocked: %q", tt.interp, tt.cmd)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("expected 'blocked' error for %q, got: %v", tt.cmd, err)
		}
	}
}

func TestShellSkill_PipeToAllowedCommand(t *testing.T) {
	s := NewShellSkill(5)
	result, err := s.Execute(map[string]string{"command": "echo hello | grep hello"})
	if err != nil {
		t.Fatalf("piping to grep should be allowed: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected 'hello' in output, got %q", result)
	}
}

func TestShellSkill_DefaultTimeout(t *testing.T) {
	s := NewShellSkill(0) // should default to 30
	if s.(*shellSkill).timeout.Seconds() != 30 {
		t.Errorf("expected default timeout 30s, got %v", s.(*shellSkill).timeout)
	}
}

func TestShellSkill_NameAndDescription(t *testing.T) {
	s := NewShellSkill(5)
	if s.Name() != "shell" {
		t.Errorf("expected name 'shell', got %q", s.Name())
	}
	if s.Description() == "" {
		t.Error("expected non-empty description")
	}
	def := s.ToolDef()
	if len(def) == 0 {
		t.Error("expected non-empty tool def")
	}
}

func TestFileReadSkill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("file content here"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewFileReadSkill()
	result, err := s.Execute(map[string]string{"path": path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "file content here" {
		t.Errorf("expected 'file content here', got %q", result)
	}
}

func TestFileReadSkill_EmptyPath(t *testing.T) {
	s := NewFileReadSkill()
	_, err := s.Execute(map[string]string{})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileReadSkill_NotFound(t *testing.T) {
	s := NewFileReadSkill()
	_, err := s.Execute(map[string]string{"path": "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileReadSkill_NameAndDef(t *testing.T) {
	s := NewFileReadSkill()
	if s.Name() != "file_read" {
		t.Errorf("expected name 'file_read', got %q", s.Name())
	}
	if len(s.ToolDef()) == 0 {
		t.Error("expected non-empty tool def")
	}
}

func TestFileWriteSkill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")

	s := NewFileWriteSkill()
	result, err := s.Execute(map[string]string{"path": path, "content": "hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Written") {
		t.Errorf("expected 'Written' message, got %q", result)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read written file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}
}

func TestFileWriteSkill_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "file.txt")

	s := NewFileWriteSkill()
	_, err := s.Execute(map[string]string{"path": path, "content": "nested"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "nested" {
		t.Errorf("expected 'nested', got %q", string(data))
	}
}

func TestFileWriteSkill_EmptyPath(t *testing.T) {
	s := NewFileWriteSkill()
	_, err := s.Execute(map[string]string{"content": "data"})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileWriteSkill_NameAndDef(t *testing.T) {
	s := NewFileWriteSkill()
	if s.Name() != "file_write" {
		t.Errorf("expected name 'file_write', got %q", s.Name())
	}
}

func TestFileEditSkill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewFileEditSkill()
	result, err := s.Execute(map[string]string{
		"path":     path,
		"old_text": "world",
		"new_text": "universe",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "replaced 1") {
		t.Errorf("unexpected result: %q", result)
	}

	data, _ := os.ReadFile(path)
	if string(data) != "hello universe" {
		t.Errorf("expected 'hello universe', got %q", string(data))
	}
}

func TestFileEditSkill_TextNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("hello world"), 0644)

	s := NewFileEditSkill()
	_, err := s.Execute(map[string]string{
		"path":     path,
		"old_text": "nonexistent",
		"new_text": "replacement",
	})
	if err == nil {
		t.Fatal("expected error when old_text not found")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileEditSkill_MultipleOccurrences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	os.WriteFile(path, []byte("aaa bbb aaa"), 0644)

	s := NewFileEditSkill()
	_, err := s.Execute(map[string]string{
		"path":     path,
		"old_text": "aaa",
		"new_text": "ccc",
	})
	if err == nil {
		t.Fatal("expected error when old_text found multiple times")
	}
	if !strings.Contains(err.Error(), "2 times") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileEditSkill_MissingParams(t *testing.T) {
	s := NewFileEditSkill()
	_, err := s.Execute(map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing params")
	}
}

func TestFileEditSkill_NameAndDef(t *testing.T) {
	s := NewFileEditSkill()
	if s.Name() != "file_edit" {
		t.Errorf("expected name 'file_edit', got %q", s.Name())
	}
}

// --- Web Fetch SSRF Protection ---

func TestIsPrivateURL(t *testing.T) {
	privateURLs := []string{
		"http://localhost/path",
		"http://127.0.0.1/path",
		"http://0.0.0.0/path",
		"http://10.0.0.1/path",
		"http://172.16.0.1/path",
		"http://192.168.1.1/path",
		"http://169.254.169.254/latest/meta-data/", // AWS metadata
	}
	for _, u := range privateURLs {
		if !isPrivateURL(u) {
			t.Errorf("expected %q to be classified as private", u)
		}
	}
}

func TestIsPrivateURL_PublicAllowed(t *testing.T) {
	publicURLs := []string{
		"http://example.com",
		"https://google.com/search",
		"https://api.github.com",
	}
	for _, u := range publicURLs {
		if isPrivateURL(u) {
			t.Errorf("expected %q to be classified as public", u)
		}
	}
}

func TestWebFetchSkill_SSRFBlocked(t *testing.T) {
	s := NewWebFetchSkill()
	_, err := s.Execute(map[string]string{"url": "http://127.0.0.1:8080/secret"})
	if err == nil {
		t.Fatal("expected SSRF to be blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestWebFetchSkill_EmptyURL(t *testing.T) {
	s := NewWebFetchSkill()
	_, err := s.Execute(map[string]string{})
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestWebFetchSkill_NameAndDef(t *testing.T) {
	s := NewWebFetchSkill()
	if s.Name() != "web_fetch" {
		t.Errorf("expected name 'web_fetch', got %q", s.Name())
	}
	if len(s.ToolDef()) == 0 {
		t.Error("expected non-empty tool def")
	}
}

// --- HTML to Text ---

func TestHtmlToText(t *testing.T) {
	html := `<html><head><script>alert('xss')</script><style>body{}</style></head><body><h1>Title</h1><p>Hello world</p></body></html>`
	text := htmlToText(html)
	if strings.Contains(text, "alert") {
		t.Error("expected script content to be stripped")
	}
	if strings.Contains(text, "body{}") {
		t.Error("expected style content to be stripped")
	}
	if !strings.Contains(text, "Title") {
		t.Error("expected 'Title' in text")
	}
	if !strings.Contains(text, "Hello world") {
		t.Error("expected 'Hello world' in text")
	}
}

func TestHtmlToText_PlainText(t *testing.T) {
	text := htmlToText("no html here")
	if text != "no html here" {
		t.Errorf("expected passthrough, got %q", text)
	}
}

// --- Memory Skill Tests ---

type mockMemory struct {
	data map[string]string
}

func newMockMemory() *mockMemory {
	return &mockMemory{data: make(map[string]string)}
}

func (m *mockMemory) Save(key, value string) (string, error) {
	m.data[key] = value
	return "Saved memory: " + key, nil
}

func (m *mockMemory) Recall(query string) (string, error) {
	for k, v := range m.data {
		if strings.Contains(k, query) || strings.Contains(v, query) {
			return "[" + k + "]: " + v, nil
		}
	}
	return "No memories found matching: " + query, nil
}

// RecallMessages stubs the messages-table search added when the
// MemoryBackend interface grew a `scope=history` recall path. Tests
// in this file don't exercise it, so a minimal "no results" return
// keeps the mock interface-compliant without changing test intent.
func (m *mockMemory) RecallMessages(query string, limit int) (string, error) {
	return "No messages found matching: " + query, nil
}

func TestMemorySkill_Save(t *testing.T) {
	mem := newMockMemory()
	s := NewMemorySkill(mem)

	result, err := s.Execute(map[string]string{
		"action": "save",
		"key":    "name",
		"value":  "Alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Saved") {
		t.Errorf("unexpected result: %q", result)
	}
	if mem.data["name"] != "Alice" {
		t.Errorf("expected 'Alice' saved, got %q", mem.data["name"])
	}
}

func TestMemorySkill_Recall(t *testing.T) {
	mem := newMockMemory()
	mem.data["color"] = "blue"
	s := NewMemorySkill(mem)

	result, err := s.Execute(map[string]string{
		"action": "recall",
		"query":  "color",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "blue") {
		t.Errorf("expected 'blue' in result, got %q", result)
	}
}

func TestMemorySkill_SaveMissingParams(t *testing.T) {
	s := NewMemorySkill(newMockMemory())
	_, err := s.Execute(map[string]string{"action": "save"})
	if err == nil {
		t.Fatal("expected error for missing key/value")
	}
}

func TestMemorySkill_RecallMissingQuery(t *testing.T) {
	s := NewMemorySkill(newMockMemory())
	_, err := s.Execute(map[string]string{"action": "recall"})
	if err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestMemorySkill_InvalidAction(t *testing.T) {
	s := NewMemorySkill(newMockMemory())
	_, err := s.Execute(map[string]string{"action": "delete"})
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
}

func TestMemorySkill_NameAndDef(t *testing.T) {
	s := NewMemorySkill(newMockMemory())
	if s.Name() != "memory" {
		t.Errorf("expected name 'memory', got %q", s.Name())
	}
	if len(s.ToolDef()) == 0 {
		t.Error("expected non-empty tool def")
	}
}

// --- SafeEnv Tests ---

func TestSafeEnv_FiltersSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	t.Setenv("OPENAI_API_KEY", "hidden")
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("NORMAL_VAR", "visible")

	env := SafeEnv()
	envStr := strings.Join(env, "\n")
	if strings.Contains(envStr, "ANTHROPIC_API_KEY") {
		t.Error("expected ANTHROPIC_API_KEY to be filtered")
	}
	if strings.Contains(envStr, "OPENAI_API_KEY") {
		t.Error("expected OPENAI_API_KEY to be filtered")
	}
	if strings.Contains(envStr, "GITHUB_TOKEN") {
		t.Error("expected GITHUB_TOKEN to be filtered")
	}
	if strings.Contains(envStr, "AWS_SECRET_ACCESS_KEY") {
		t.Error("expected AWS_SECRET_ACCESS_KEY to be filtered")
	}
	if !strings.Contains(envStr, "NORMAL_VAR") {
		t.Error("expected NORMAL_VAR to be present")
	}
}

// --- External Skill Tests ---

func TestLoadExternalSkill(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "greet")
	os.MkdirAll(skillDir, 0755)

	skillContent := `---
name: greet
description: Greet someone
command: [echo, hello]
schema:
  name:
    type: string
    description: Name to greet
    required: true
---

# Greet Skill
Says hello.
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillContent), 0644); err != nil {
		t.Fatal(err)
	}

	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		t.Fatalf("LoadExternalSkill failed: %v", err)
	}
	if ext.Name() != "greet" {
		t.Errorf("expected name 'greet', got %q", ext.Name())
	}
	if ext.Description() != "Greet someone" {
		t.Errorf("expected description 'Greet someone', got %q", ext.Description())
	}
	if len(ext.ToolDef()) == 0 {
		t.Error("expected non-empty tool def")
	}
}

func TestLoadExternalSkill_MissingFields(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	// Missing command
	content := "---\nname: bad\ndescription: Missing command\n---\nBody"
	os.WriteFile(skillPath, []byte(content), 0644)

	_, err := LoadExternalSkill(skillPath)
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestLoadExternalSkill_DefaultTimeout(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	content := "---\nname: test\ndescription: test skill\ncommand: [echo]\n---\nBody"
	os.WriteFile(skillPath, []byte(content), 0644)

	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ext.meta.Timeout != 30 {
		t.Errorf("expected default timeout 30, got %d", ext.meta.Timeout)
	}
}

func TestLoadExternalSkill_Execute(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "myskill")
	os.MkdirAll(skillDir, 0755)

	content := `---
name: myskill
description: Test execution
command: [echo, "{{greeting}}"]
args: ["{{name}}"]
---
Body
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)

	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		t.Fatalf("LoadExternalSkill failed: %v", err)
	}

	result, err := ext.Execute(map[string]string{"name": "World", "greeting": "Hi"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	// Both Command-side ({{greeting}}) and Args-side ({{name}})
	// templates must be substituted. Pre-fix this test passed even
	// though {{greeting}} was leaking through literally.
	if !strings.Contains(result, "World") {
		t.Errorf("expected output to contain 'World' (Args substitution), got %q", result)
	}
	if !strings.Contains(result, "Hi") {
		t.Errorf("expected output to contain 'Hi' (Command substitution), got %q", result)
	}
	if strings.Contains(result, "{{") {
		t.Errorf("expected no unsubstituted templates in output, got %q", result)
	}
}

// TestExtractImageMarkers covers the image:// marker scanner that
// reroutes screenshot paths from tool output text into the
// ToolResult.Images list. Vision-capable brains then inline the
// image bytes; text-only brains see the cleaned text without markers.
func TestExtractImageMarkers(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantText string
		wantImgs []string
	}{
		{
			"no_markers",
			"path: /a.png\ndimensions: 100x100",
			"path: /a.png\ndimensions: 100x100",
			nil,
		},
		{
			"single_marker",
			"path: /a.png\nimage:///a.png\ndimensions: 100x100",
			"path: /a.png\ndimensions: 100x100",
			[]string{"/a.png"},
		},
		{
			"multiple_markers",
			"image:///a.png\nimage:///b.jpg\nresult: ok",
			"result: ok",
			[]string{"/a.png", "/b.jpg"},
		},
		{
			"dedup_same_path",
			"image:///x.png\nfoo\nimage:///x.png\nbar",
			"foo\nbar",
			[]string{"/x.png"},
		},
		{
			"marker_with_indent_trimmed",
			"  image:///x.png  \nfoo",
			"foo",
			[]string{"/x.png"},
		},
		{
			"marker_only",
			"image:///solo.png",
			"",
			[]string{"/solo.png"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText, gotImgs := extractImageMarkers(tt.input)
			if gotText != tt.wantText {
				t.Errorf("text = %q, want %q", gotText, tt.wantText)
			}
			if len(gotImgs) != len(tt.wantImgs) {
				t.Fatalf("images = %v, want %v", gotImgs, tt.wantImgs)
			}
			for i := range gotImgs {
				if gotImgs[i] != tt.wantImgs[i] {
					t.Errorf("images[%d] = %q, want %q", i, gotImgs[i], tt.wantImgs[i])
				}
			}
		})
	}
}

// TestLoadExternalSkill_UnpassedTemplateStripped covers the second
// bug in the substitution path: when a SKILL.md author uses `{{var}}`
// inside a shell command as a "param wasn't passed" sentinel
// (e.g. `[ "$X" = "{{wait}}" ] && X=""`), the substitution-everywhere
// behavior rewrites BOTH the arg AND the sentinel literal — when the
// caller DID pass `wait=true`, the test became `[ "true" = "true" ]`
// → succeeds → the param value gets clobbered. The kernel now strips
// any leftover `{{name}}` placeholders to "" so SKILL.md authors can
// detect "missing param" with `[ -n "$X" ]` instead of brittle
// sentinel comparisons.
func TestLoadExternalSkill_UnpassedTemplateStripped(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "echo_args")
	os.MkdirAll(skillDir, 0755)

	content := `---
name: echo_args
description: Print the args verbatim so the test can see what shell received
command: [sh, -c, "echo arg1=[$1] arg2=[$2]", "_"]
args: ["{{required}}", "{{optional}}"]
schema:
  required:
    type: string
    required: true
  optional:
    type: string
---
body
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)
	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		t.Fatalf("LoadExternalSkill: %v", err)
	}

	// Caller omits `optional` — should arrive as "" not "{{optional}}".
	out, err := ext.Execute(map[string]string{"required": "value"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "arg1=[value]") {
		t.Errorf("expected arg1=[value], got %q", out)
	}
	if !strings.Contains(out, "arg2=[]") {
		t.Errorf("expected arg2=[] (stripped), got %q", out)
	}
	if strings.Contains(out, "{{optional}}") {
		t.Errorf("unsubstituted {{optional}} leaked into output: %q", out)
	}
}

// TestLoadExternalSkill_SentinelPatternNotSelfDefeating covers the
// concrete failure mode that motivated the strip: a SKILL.md uses
// `{{var}}` literally in its shell payload AND the caller passes
// var="true". Pre-fix, the substitution rewrote the literal to "true",
// the sentinel comparison `[ "true" = "true" ]` succeeded, and the
// branching went the wrong way. Post-fix, the leftover `{{var}}` form
// in the shell payload is stripped (since the same template name was
// already substituted to its value, there's nothing left to strip
// — but if a template name appears in the shell that was never an
// arg, it's stripped). The real defense: SKILL.md authors should now
// use `[ -n "$X" ]` for missing-param detection.
func TestLoadExternalSkill_SentinelPatternNotSelfDefeating(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "wait_check")
	os.MkdirAll(skillDir, 0755)

	// The shell uses `[ -n "$W" ]` as the proper missing-param check.
	content := `---
name: wait_check
description: Print which branch
command: [sh, -c, 'W="$1"; if [ "$W" = "true" ]; then echo BRANCH=blocking; elif [ -n "$W" ]; then echo BRANCH=other:$W; else echo BRANCH=missing; fi', "_"]
args: ["{{wait}}"]
schema:
  wait:
    type: string
---
body
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)
	ext, _ := LoadExternalSkill(skillPath)

	out, _ := ext.Execute(map[string]string{"wait": "true"})
	if !strings.Contains(out, "BRANCH=blocking") {
		t.Errorf("wait=true should produce BRANCH=blocking, got %q", out)
	}
	out, _ = ext.Execute(map[string]string{})
	if !strings.Contains(out, "BRANCH=missing") {
		t.Errorf("missing wait should produce BRANCH=missing, got %q", out)
	}
}

// TestLoadExternalSkill_CommandSubstitution covers the historical bug
// where a SKILL.md placing `{{var}}` directly inside a Command element
// (the typical pattern for one-line forge'd skills like weather's
// [curl, "-s", "https://wttr.in/{{location}}"]) silently leaked the
// literal template into the executed command.
func TestLoadExternalSkill_CommandSubstitution(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "echo_only")
	os.MkdirAll(skillDir, 0755)

	content := `---
name: echo_only
description: Substitution must work even with no args field at all
command: [echo, "hello {{who}}"]
schema:
  who:
    type: string
    required: true
---
Body
`
	skillPath := filepath.Join(skillDir, "SKILL.md")
	os.WriteFile(skillPath, []byte(content), 0644)

	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		t.Fatalf("LoadExternalSkill failed: %v", err)
	}
	result, err := ext.Execute(map[string]string{"who": "Lobster"})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result, "hello Lobster") {
		t.Errorf("expected 'hello Lobster' in output, got %q", result)
	}
	if strings.Contains(result, "{{who}}") {
		t.Errorf("template {{who}} should have been substituted, got %q", result)
	}
}

func TestLoadExternalSkills_Directory(t *testing.T) {
	dir := t.TempDir()

	// Create two valid skill dirs
	for _, name := range []string{"skill_a", "skill_b"} {
		skillDir := filepath.Join(dir, name)
		os.MkdirAll(skillDir, 0755)
		content := "---\nname: " + name + "\ndescription: " + name + "\ncommand: [echo]\n---\nBody"
		os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644)
	}

	// Create a dir without SKILL.md (should be skipped)
	os.MkdirAll(filepath.Join(dir, "no_skill"), 0755)

	skills, err := LoadExternalSkills(dir)
	if err != nil {
		t.Fatalf("LoadExternalSkills failed: %v", err)
	}
	if len(skills) != 2 {
		t.Errorf("expected 2 skills, got %d", len(skills))
	}
}

func TestLoadExternalSkills_NonexistentDir(t *testing.T) {
	skills, err := LoadExternalSkills("/nonexistent/dir")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got: %v", err)
	}
	if skills != nil {
		t.Errorf("expected nil skills, got %v", skills)
	}
}

// --- Forge Skill Tests ---

func TestForgeSkill(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	s := NewForgeSkill(dir, reg)

	if s.Name() != "forge" {
		t.Errorf("expected name 'forge', got %q", s.Name())
	}

	result, err := s.Execute(map[string]string{
		"name":        "my_skill",
		"description": "A forged skill",
		"command":     "echo hello",
	})
	if err != nil {
		t.Fatalf("Forge execute failed: %v", err)
	}
	if !strings.Contains(result, "Forged skill 'my_skill'") {
		t.Errorf("unexpected result: %q", result)
	}

	// Verify SKILL.md was created
	skillPath := filepath.Join(dir, "my_skill", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("SKILL.md not created: %v", err)
	}
	if !strings.Contains(string(data), "my_skill") {
		t.Error("SKILL.md missing skill name")
	}

	// Verify the skill was registered
	_, err = reg.Get("my_skill")
	if err != nil {
		t.Errorf("forged skill not registered: %v", err)
	}
}

func TestForgeSkill_MissingName(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	s := NewForgeSkill(dir, reg)

	_, err := s.Execute(map[string]string{
		"description": "desc",
		"command":     "echo",
	})
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestForgeSkill_WithScript(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	s := NewForgeSkill(dir, reg)

	_, err := s.Execute(map[string]string{
		"name":           "scripted",
		"description":    "A scripted skill",
		"command":        "python3 run.py",
		"script_content": "print('hello')",
	})
	if err != nil {
		t.Fatalf("Forge with script failed: %v", err)
	}

	scriptPath := filepath.Join(dir, "scripted", "run.py")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("script not created: %v", err)
	}
	if string(data) != "print('hello')" {
		t.Errorf("unexpected script content: %q", string(data))
	}
}

// TestForgeSkill_RejectsInternalSkillName covers the most common LLM
// foot-gun: forging a script that calls kinclaw skills (`ui`, `input`,
// `screen`, ...) as if they were shell binaries. Live observation:
// agent forged a `reminders_add` whose Python ran
// `subprocess.run(["ui", "action=click", ...])` — silently failed every
// call but the script's print said "success", producing a skill that
// lied about its work forever. Kernel now refuses up front.
func TestForgeSkill_RejectsInternalSkillName(t *testing.T) {
	internals := []string{"ui", "input", "screen", "record", "shell", "tts", "stt", "forge", "learn", "web_fetch"}
	for _, name := range internals {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			reg := NewRegistry()
			s := NewForgeSkill(dir, reg)
			_, err := s.Execute(map[string]string{
				"name":        "broken_" + name,
				"description": "tries to call kinclaw skill via subprocess",
				"command":     name + " action=foo",
			})
			if err == nil {
				t.Fatalf("expected forge rejection for command starting with %q, got nil", name)
			}
			if !strings.Contains(err.Error(), "forge rejected") {
				t.Errorf("expected 'forge rejected' in error, got %v", err)
			}
		})
	}
}

// TestForgeSkill_RejectsCommandNotInPath covers typos / hallucinated
// binary names — agent invents `reminderctl` or `foo-tool` that
// doesn't exist. Kernel rejects before writing the SKILL.md, so the
// agent gets a clear "not found in $PATH" message and can retry.
func TestForgeSkill_RejectsCommandNotInPath(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	s := NewForgeSkill(dir, reg)
	_, err := s.Execute(map[string]string{
		"name":        "bogus",
		"description": "uses a binary that doesn't exist",
		"command":     "totally_not_a_real_binary_xyz123 --foo",
	})
	if err == nil {
		t.Fatal("expected forge rejection for nonexistent command, got nil")
	}
	if !strings.Contains(err.Error(), "not found in $PATH") {
		t.Errorf("expected 'not found in $PATH' in error, got %v", err)
	}
}

// TestForgeSkill_AcceptsRealBinary — the happy path: command[0] is a
// real binary in $PATH. Pre-flight passes, SKILL.md gets written.
func TestForgeSkill_AcceptsRealBinary(t *testing.T) {
	cases := []string{
		"sh -c 'echo hi'",
		"osascript -e 'return 1'",
		"python3 -c 'pass'",
		"bc",
	}
	for _, cmd := range cases {
		t.Run(strings.Fields(cmd)[0], func(t *testing.T) {
			dir := t.TempDir()
			reg := NewRegistry()
			s := NewForgeSkill(dir, reg)
			_, err := s.Execute(map[string]string{
				"name":        "ok_" + strings.Fields(cmd)[0],
				"description": "uses a real binary",
				"command":     cmd,
			})
			if err != nil {
				t.Fatalf("expected accept for %q, got %v", cmd, err)
			}
		})
	}
}

// --- Parallel safety ---

type orderSkill struct {
	name  string
	mu    *sync.Mutex
	order *[]string
	delay time.Duration
}

func (o *orderSkill) Name() string             { return o.name }
func (o *orderSkill) Description() string      { return o.name }
func (o *orderSkill) ToolDef() json.RawMessage { return MakeToolDef(o.name, o.name, nil, nil) }
func (o *orderSkill) Execute(map[string]string) (string, error) {
	time.Sleep(o.delay)
	o.mu.Lock()
	*o.order = append(*o.order, o.name)
	o.mu.Unlock()
	return o.name, nil
}

func TestExecuteToolCalls_ExclusiveSkillsRunInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	r := NewRegistry()
	// "input" is exclusive and slow; "web_search" is fast. If the batch
	// ran in parallel, web_search would finish first.
	r.Register(&orderSkill{name: "input", mu: &mu, order: &order, delay: 40 * time.Millisecond})
	r.Register(&orderSkill{name: "web_search", mu: &mu, order: &order})
	res := ExecuteToolCalls(r, []ToolCallInfo{{ID: "1", Name: "input"}, {ID: "2", Name: "web_search"}})
	if len(res) != 2 || res[0].Output != "input" || res[1].Output != "web_search" {
		t.Fatalf("results out of order: %+v", res)
	}
	if len(order) != 2 || order[0] != "input" {
		t.Fatalf("exclusive batch should execute sequentially, got %v", order)
	}
}

func TestExecuteToolCalls_SafeSkillsRunConcurrently(t *testing.T) {
	var mu sync.Mutex
	var order []string
	r := NewRegistry()
	r.Register(&orderSkill{name: "kinbrain", mu: &mu, order: &order, delay: 40 * time.Millisecond})
	r.Register(&orderSkill{name: "web_search", mu: &mu, order: &order})
	start := time.Now()
	ExecuteToolCalls(r, []ToolCallInfo{{ID: "1", Name: "kinbrain"}, {ID: "2", Name: "web_search"}})
	if order[0] != "web_search" {
		t.Fatalf("safe batch should run in parallel (fast one finishes first), got %v", order)
	}
	if time.Since(start) > 70*time.Millisecond {
		t.Fatal("parallel batch took as long as sequential")
	}
}

func TestParallelSafe(t *testing.T) {
	for _, n := range []string{"ui", "input", "shell", "file_edit", "notes_pin", "music_play"} {
		if ParallelSafe(n) {
			t.Errorf("%s should be exclusive", n)
		}
	}
	for _, n := range []string{"web_search", "kinbrain", "spawn", "memory", "mcp_fs_read", "weather"} {
		if !ParallelSafe(n) {
			t.Errorf("%s should be parallel-safe", n)
		}
	}
}
