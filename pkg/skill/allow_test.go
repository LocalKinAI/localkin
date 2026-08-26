package skill

import (
	"encoding/json"
	"testing"
)

func TestMatchesAllow(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		skill string
		want  bool
	}{
		// Existing behaviour must not change.
		{"exact hit", []string{"screen", "input"}, "screen", true},
		{"exact miss", []string{"screen", "input"}, "shell", false},
		{"empty list allows everything", nil, "anything", true},

		// Wildcards: what MCP needs, since tool names are only knowable
		// after connecting to the server.
		{"prefix matches server", []string{"mcp_github_*"}, "mcp_github_create_issue", true},
		{"prefix excludes other server", []string{"mcp_github_*"}, "mcp_slack_post", false},
		{"broad prefix matches all mcp", []string{"mcp_*"}, "mcp_slack_post", true},
		{"mcp prefix does not leak to builtins", []string{"mcp_*"}, "shell", false},
		{"mixed exact and wildcard", []string{"screen", "mcp_github_*"}, "screen", true},
		{"mixed, wildcard side", []string{"screen", "mcp_github_*"}, "mcp_github_x", true},
		{"mixed, neither", []string{"screen", "mcp_github_*"}, "shell", false},

		// A bare "*" enables everything — worth having behave sanely rather
		// than being a special case someone discovers by accident.
		{"bare star", []string{"*"}, "anything", true},

		// Prefix is a prefix, not a fuzzy match.
		{"not a suffix match", []string{"github_*"}, "mcp_github_x", false},
	}

	for _, tc := range cases {
		if got := MatchesAllow(tc.allow, tc.skill); got != tc.want {
			t.Errorf("%s: MatchesAllow(%v, %q) = %v, want %v",
				tc.name, tc.allow, tc.skill, got, tc.want)
		}
	}
}

// TestFilteredToolDefsHonoursWildcards checks the matcher is actually wired
// into the path that decides what the model sees, not just exported for
// callers to use.
func TestFilteredToolDefsHonoursWildcards(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubSkill{name: "mcp_mock_alpha"})
	reg.Register(&stubSkill{name: "mcp_mock_beta"})
	reg.Register(&stubSkill{name: "shell"})

	if got := len(reg.FilteredToolDefs([]string{"mcp_mock_*"})); got != 2 {
		t.Errorf("wildcard exposed %d tools, want 2", got)
	}
	if got := len(reg.FilteredToolDefs([]string{"shell"})); got != 1 {
		t.Errorf("exact name exposed %d tools, want 1", got)
	}
	if got := len(reg.FilteredToolDefs(nil)); got != 3 {
		t.Errorf("empty allowlist exposed %d tools, want all 3", got)
	}
}

type stubSkill struct{ name string }

func (s *stubSkill) Name() string        { return s.name }
func (s *stubSkill) Description() string { return "stub" }
func (s *stubSkill) ToolDef() json.RawMessage { return json.RawMessage(`{"type":"function"}`) }
func (s *stubSkill) Execute(map[string]string) (string, error) {
	return "", nil
}
