package skill

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKinBrainSkill_Identity(t *testing.T) {
	s := NewKinBrainSkill()
	if s.Name() != "kinbrain" {
		t.Errorf("Name() = %q, want %q", s.Name(), "kinbrain")
	}
	if !strings.Contains(s.Description(), "kinbrain") {
		t.Errorf("Description() should mention kinbrain")
	}
}

func TestKinBrainSkill_ToolDef(t *testing.T) {
	s := NewKinBrainSkill()
	def := s.ToolDef()

	// OpenAI function-calling format: top-level should have
	// "type": "function" + "function": { ... }
	var wrapper struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  struct {
				Type       string                            `json:"type"`
				Properties map[string]map[string]interface{} `json:"properties"`
				Required   []string                          `json:"required"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(def, &wrapper); err != nil {
		t.Fatalf("ToolDef unmarshal: %v", err)
	}

	if wrapper.Function.Name != "kinbrain" {
		t.Errorf("function.name = %q, want %q", wrapper.Function.Name, "kinbrain")
	}

	// All four params present.
	for _, want := range []string{"action", "query", "title", "body"} {
		if _, ok := wrapper.Function.Parameters.Properties[want]; !ok {
			t.Errorf("missing parameter %q", want)
		}
	}

	// Only 'action' should be required (query/title/body are
	// action-specific, gated by skill logic not schema).
	if len(wrapper.Function.Parameters.Required) != 1 ||
		wrapper.Function.Parameters.Required[0] != "action" {
		t.Errorf("required = %v, want [action]", wrapper.Function.Parameters.Required)
	}
}

func TestKinBrainSkill_ExecuteMissingBinary(t *testing.T) {
	// Force exec.LookPath to fail by clearing PATH for the test scope.
	t.Setenv("PATH", "")
	s := NewKinBrainSkill()
	_, err := s.Execute(map[string]string{"action": "recall", "query": "test"})
	if err == nil {
		t.Fatal("expected error when kinbrain binary is missing")
	}
	if !strings.Contains(err.Error(), "kinbrain CLI not found") {
		t.Errorf("error should explain how to install kinbrain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "go install") {
		t.Errorf("error should include install command, got: %v", err)
	}
}

func TestKinBrainSkill_UnknownAction(t *testing.T) {
	// Put kinbrain on PATH conceptually via a non-empty stub (this
	// test only validates the action validation, not the exec call).
	// We can't make exec.LookPath succeed without a real kinbrain on
	// PATH, so we check the missing-action and empty-action branches
	// which run BEFORE the lookup.
	s := NewKinBrainSkill()
	_, err := s.Execute(map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "action") {
		t.Errorf("empty action should error, got: %v", err)
	}
}
