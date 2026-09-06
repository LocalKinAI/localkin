package hooks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based hook tests")
	}
}

func TestPreTool_BlockOnExit2WithStderr(t *testing.T) {
	skipOnWindows(t)
	r := New(Config{PreTool: []Hook{
		{Match: "input", Run: `read p; echo "no typing during meetings: $p" >&2; exit 2`},
	}}, t.TempDir(), "test", os.Environ())
	out := r.PreTool(context.Background(), "input", map[string]string{"action": "type"})
	if !out.Blocked || !strings.Contains(out.Message, "no typing during meetings") {
		t.Fatalf("expected block with stderr, got %+v", out)
	}
	// Payload reached stdin as JSON with the skill + params.
	if !strings.Contains(out.Message, `"skill":"input"`) || !strings.Contains(out.Message, `"action":"type"`) {
		t.Fatalf("payload not delivered: %q", out.Message)
	}
	// A non-matching skill is untouched.
	if o := r.PreTool(context.Background(), "ui", nil); o.Blocked {
		t.Fatal("ui should not match an input hook")
	}
}

func TestPreTool_OtherExitCodesAreIgnored(t *testing.T) {
	skipOnWindows(t)
	var logged []string
	r := New(Config{PreTool: []Hook{{Match: "*", Run: "exit 7"}}}, t.TempDir(), "s", os.Environ())
	r.Log = func(s string) { logged = append(logged, s) }
	if out := r.PreTool(context.Background(), "shell", nil); out.Blocked {
		t.Fatalf("exit 7 must not block: %+v", out)
	}
	if len(logged) != 1 {
		t.Fatalf("expected one log line, got %v", logged)
	}
}

func TestPostTool_FeedbackAndRelativeCwd(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "check.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ncat > out.json\necho 'output too long' >&2\nexit 2\n"), 0o755)
	r := New(Config{PostTool: []Hook{{Match: "shell", Run: "./check.sh"}}}, dir, "s", os.Environ())
	out := r.PostTool(context.Background(), "shell", map[string]string{"command": "ls"}, "a\nb")
	if !out.Blocked || out.Message != "output too long" {
		t.Fatalf("got %+v", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.json"))
	if err != nil || !strings.Contains(string(data), `"output":"a\nb"`) {
		t.Fatalf("hook should run in soul dir and see output: %v %s", err, data)
	}
}

func TestTimeout(t *testing.T) {
	skipOnWindows(t)
	var logged []string
	r := New(Config{PreTool: []Hook{{Run: "sleep 5", Timeout: 1}}}, t.TempDir(), "s", os.Environ())
	r.Log = func(s string) { logged = append(logged, s) }
	if out := r.PreTool(context.Background(), "x", nil); out.Blocked {
		t.Fatal("timeout must not block")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "timed out") {
		t.Fatalf("expected timeout log, got %v", logged)
	}
}

func TestEmptyRunnerIsNoop(t *testing.T) {
	var r *Runner
	if !r.Empty() || r.PreTool(context.Background(), "x", nil).Blocked {
		t.Fatal("nil runner should be a no-op")
	}
	r.Stop(context.Background(), "bye") // must not panic
}

func TestMatches(t *testing.T) {
	if !matches("", "anything") || !matches("*", "x") || !matches("mcp_*", "mcp_fs_read") || matches("ui", "input") {
		t.Fatal("grammar")
	}
}
