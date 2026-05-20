package skill

import (
	"encoding/json"
	"os"
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

// TestKinBrowserSkill_FetchFailureReturnsContent — the key fix from
// the California-fire incident. When kinbrowser CLI exits non-zero on
// a URL fetch (timeout, 404, paywall, etc.), the skill MUST return
// markdown content explaining the failure, NOT a Go error — otherwise
// KinClaw's circuit breaker counts it as a skill failure and blocks
// the whole task after 3 unrelated URL failures.
//
// We simulate this by pointing the skill at a /usr/bin/false that
// exits 1 (substituting for kinbrowser); the test verifies the skill
// gracefully returns content, not error.
func TestKinBrowserSkill_FetchFailureReturnsContent(t *testing.T) {
	// Build a tiny shim binary that mimics kinbrowser by exiting 1
	// with a plausible error message on stderr.
	shimDir := t.TempDir()
	shim := shimDir + "/kinbrowser"
	if err := writeShim(shim); err != nil {
		t.Fatal(err)
	}

	// Put the shim first on PATH so exec.LookPath finds it.
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))

	s := NewKinBrowserSkill()
	out, err := s.Execute(map[string]string{
		"action": "open",
		"url":    "https://example.invalid/some-url",
	})

	if err != nil {
		t.Fatalf("expected nil error (fetch failure → content), got: %v", err)
	}
	for _, want := range []string{
		"Fetch failed",
		"example.invalid",
		"PER-URL failure",
		"Try a different URL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("content should contain %q; got:\n%s", want, out)
		}
	}
}

// writeShim creates a tiny executable that exits 1 with a fake
// kinbrowser error message on stderr. Cross-platform-ish — on macOS/
// Linux we use /bin/sh; Windows we'd need a .bat but the tests don't
// run there.
func writeShim(path string) error {
	script := "#!/bin/sh\necho 'kinbrowser: all backends failed; chromedp last error: net/http: timeout' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	return nil
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
