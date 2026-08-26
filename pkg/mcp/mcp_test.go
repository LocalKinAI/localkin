package mcp

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/LocalKinAI/kinclaw/pkg/skill"
)

func mockServer(t *testing.T) *Client {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script, err := filepath.Abs("testdata/mock_server.py")
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}

	c, err := connect("mock", py, []string{script}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)

	if _, err := c.ListTools(); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	return c
}

func TestConnectAndListTools(t *testing.T) {
	c := mockServer(t)
	tools := c.Tools()
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].Name != "echo_types" {
		t.Errorf("tool name = %q, want echo_types", tools[0].Name)
	}
}

// TestSkillNameIsNamespaced guards against an MCP server shadowing a built-in
// skill. Server tool names come from third parties and "read"/"run"/"search"
// are all plausible; a collision would silently replace a built-in in the
// registry map.
func TestSkillNameIsNamespaced(t *testing.T) {
	skills := SkillsFromClient(mockServer(t))
	if len(skills) != 1 {
		t.Fatalf("got %d skills, want 1", len(skills))
	}
	if got, want := skills[0].Name(), "mcp_mock_echo_types"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// TestToolDefIsValidFunctionCalling checks the shape the brain feeds to the
// model, including that the server's nested inputSchema survives — that is
// the part skill.MakeToolDef cannot express.
func TestToolDefIsValidFunctionCalling(t *testing.T) {
	skills := SkillsFromClient(mockServer(t))

	var def struct {
		Type     string `json:"type"`
		Function struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(skills[0].ToolDef(), &def); err != nil {
		t.Fatalf("ToolDef is not valid JSON: %v", err)
	}
	if def.Type != "function" {
		t.Errorf("type = %q, want function", def.Type)
	}
	props, ok := def.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("parameters.properties missing")
	}
	if _, ok := props["items"]; !ok {
		t.Error("nested array property was dropped from the schema")
	}
}

// TestExecuteRestoresTypes is the load-bearing one.
//
// kinclaw hands skills map[string]string — ParseArguments stringifies
// primitives and JSON-encodes structures. MCP servers validate against their
// declared schema, so a tool wanting {"type":"integer"} and receiving "5"
// rejects the call. This asserts each value arrives as the declared type.
func TestExecuteRestoresTypes(t *testing.T) {
	skills := SkillsFromClient(mockServer(t))

	// Exactly what ParseArguments would produce for these values.
	out, err := skills[0].Execute(map[string]string{
		"text":    "hello",
		"count":   "42",
		"ratio":   "0.5",
		"flag":    "true",
		"items":   `["a","b"]`,
		"opts":    `{"k":"v"}`,
		"untyped": `{"nested":1}`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("server returned non-JSON %q: %v", out, err)
	}

	want := map[string]string{
		"text":    "str",
		"count":   "int",
		"ratio":   "float",
		"flag":    "bool",
		"items":   "list",
		"opts":    "dict",
		"untyped": "dict", // no declared type, but unambiguously JSON
	}
	for k, wantType := range want {
		if got[k] != wantType {
			t.Errorf("%s arrived as %s, want %s", k, got[k], wantType)
		}
	}
}

// TestCoerceLeavesAmbiguousStringsAlone: a string field holding digits must
// stay a string. Guessing by content would corrupt zip codes, IDs with
// leading zeros, and version numbers.
func TestCoerceLeavesAmbiguousStringsAlone(t *testing.T) {
	cases := []struct {
		raw    string
		schema map[string]any
		want   any
	}{
		{"01234", map[string]any{"type": "string"}, "01234"},
		{"123", nil, "123"},              // no schema — do not guess
		{"true", nil, "true"},            // no schema — do not guess
		{"not json", map[string]any{"type": "object"}, "not json"}, // unparseable passes through
	}
	for _, tc := range cases {
		if got := coerce(tc.raw, tc.schema); got != tc.want {
			t.Errorf("coerce(%q, %v) = %#v, want %#v", tc.raw, tc.schema, got, tc.want)
		}
	}
}

// TestConnectRegistersIntoRegistry covers the config-driven path, and that a
// broken server costs only its own tools.
func TestConnectRegistersIntoRegistry(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script, _ := filepath.Abs("testdata/mock_server.py")

	cfg := &Config{MCPServers: map[string]ServerConfig{
		"mock":     {Command: py, Args: []string{script}},
		"broken":   {Command: "/nonexistent/binary-that-does-not-exist"},
		"disabled": {Command: py, Args: []string{script}, Disabled: true},
	}}

	reg := skill.NewRegistry()
	clients, results := Connect(cfg, reg)
	t.Cleanup(func() {
		for _, c := range clients {
			c.Close()
		}
	})

	if len(clients) != 1 {
		t.Errorf("got %d live clients, want 1", len(clients))
	}

	byName := map[string]LoadResult{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if byName["mock"].Err != nil || byName["mock"].ToolCount != 1 {
		t.Errorf("mock: %+v, want 1 tool and no error", byName["mock"])
	}
	if byName["broken"].Err == nil {
		t.Error("broken server reported success")
	}
	if !byName["disabled"].Disabled {
		t.Error("disabled server was not reported as disabled")
	}

	// The working server's tool must be registered despite the broken peer.
	if _, err := reg.Get("mcp_mock_echo_types"); err != nil {
		t.Errorf("tool not registered (%v); a failing peer took down a working server", err)
	}
}
