package skill

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKinBrowserSkill_Identity(t *testing.T) {
	s := NewKinBrowserSkill()
	if s.Name() != "kinbrowser" {
		t.Errorf("Name() = %q, want kinbrowser", s.Name())
	}
	if !strings.Contains(s.Description(), "markdown") {
		t.Error("Description should mention markdown")
	}
}

func TestKinBrowserSkill_ToolDef(t *testing.T) {
	s := NewKinBrowserSkill()
	def := s.ToolDef()

	var wrapper struct {
		Type     string `json:"type"`
		Function struct {
			Name       string `json:"name"`
			Parameters struct {
				Properties map[string]map[string]interface{} `json:"properties"`
				Required   []string                          `json:"required"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(def, &wrapper); err != nil {
		t.Fatalf("ToolDef unmarshal: %v", err)
	}
	if wrapper.Function.Name != "kinbrowser" {
		t.Errorf("function.name = %q, want kinbrowser", wrapper.Function.Name)
	}
	for _, want := range []string{"action", "url"} {
		if _, ok := wrapper.Function.Parameters.Properties[want]; !ok {
			t.Errorf("missing parameter %q", want)
		}
	}
	if len(wrapper.Function.Parameters.Required) != 2 {
		t.Errorf("required = %v, want [action url]", wrapper.Function.Parameters.Required)
	}
}

func TestKinBrowserSkill_MissingBinary(t *testing.T) {
	t.Setenv("PATH", "")
	s := NewKinBrowserSkill()
	_, err := s.Execute(map[string]string{"action": "open", "url": "https://example.com"})
	if err == nil {
		t.Fatal("expected error when kinbrowser binary is missing")
	}
	if !strings.Contains(err.Error(), "kinbrowser CLI not found") {
		t.Errorf("error should mention missing CLI, got: %v", err)
	}
	if !strings.Contains(err.Error(), "go install") {
		t.Errorf("error should include install command, got: %v", err)
	}
}

func TestKinBrowserSkill_ValidationOrder(t *testing.T) {
	// Action validation runs BEFORE LookPath so the LLM gets the right
	// error in the right order.
	s := NewKinBrowserSkill()
	_, err := s.Execute(map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "action") {
		t.Errorf("empty action should produce action error, got: %v", err)
	}
	_, err = s.Execute(map[string]string{"action": "weird"})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("bad action should produce action error, got: %v", err)
	}
	_, err = s.Execute(map[string]string{"action": "open"})
	if err == nil || !strings.Contains(err.Error(), "url") {
		t.Errorf("missing url should produce url error, got: %v", err)
	}
}
