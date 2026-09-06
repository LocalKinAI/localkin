package soul

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitFrontmatter_Valid(t *testing.T) {
	data := []byte("---\nname: test\n---\nHello body")
	yamlBlock, body, err := SplitFrontmatter(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(yamlBlock), "name: test") {
		t.Errorf("expected yaml to contain 'name: test', got %q", string(yamlBlock))
	}
	if !strings.Contains(body, "Hello body") {
		t.Errorf("expected body to contain 'Hello body', got %q", body)
	}
}

func TestSplitFrontmatter_LeadingNewlines(t *testing.T) {
	data := []byte("\n\n---\nname: test\n---\nbody")
	_, body, err := SplitFrontmatter(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "body") {
		t.Errorf("expected body 'body', got %q", body)
	}
}

func TestSplitFrontmatter_NoOpeningDelim(t *testing.T) {
	data := []byte("no frontmatter here")
	_, _, err := SplitFrontmatter(data)
	if err == nil {
		t.Fatal("expected error for missing opening ---")
	}
	if !strings.Contains(err.Error(), "must start with ---") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSplitFrontmatter_NoClosingDelim(t *testing.T) {
	data := []byte("---\nname: test\nno closing")
	_, _, err := SplitFrontmatter(data)
	if err == nil {
		t.Fatal("expected error for missing closing ---")
	}
	if !strings.Contains(err.Error(), "missing closing ---") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSplitFrontmatter_EmptyBody(t *testing.T) {
	data := []byte("---\nname: test\n---\n")
	_, body, err := SplitFrontmatter(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
}

func TestParseSoul_MinimalValid(t *testing.T) {
	data := []byte("---\nname: testsoul\n---\nYou are a test assistant.")
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Name != "testsoul" {
		t.Errorf("expected name 'testsoul', got %q", s.Meta.Name)
	}
	if s.Meta.Brain.Provider != "claude" {
		t.Errorf("expected default provider 'claude', got %q", s.Meta.Brain.Provider)
	}
	if s.Meta.Brain.Endpoint != "https://api.anthropic.com" {
		t.Errorf("expected default claude endpoint, got %q", s.Meta.Brain.Endpoint)
	}
	if s.Meta.Brain.Temperature != 0.7 {
		t.Errorf("expected default temperature 0.7, got %f", s.Meta.Brain.Temperature)
	}
	if s.Meta.Brain.ContextLength != 8192 {
		t.Errorf("expected default context_length 8192, got %d", s.Meta.Brain.ContextLength)
	}
	if s.Meta.Skills.OutputDir != "./output" {
		t.Errorf("expected default output_dir './output', got %q", s.Meta.Skills.OutputDir)
	}
	if !strings.Contains(s.SystemPrompt, "You are a test assistant.") {
		t.Errorf("expected system prompt to contain body text")
	}
	if !strings.Contains(s.SystemPrompt, "## Security") {
		t.Errorf("expected system prompt to contain security suffix")
	}
}

func TestParseSoul_MissingName(t *testing.T) {
	data := []byte("---\nversion: 1.0\n---\nBody")
	_, err := ParseSoul(data)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "missing required field: name") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseSoul_ProviderDefaults(t *testing.T) {
	tests := []struct {
		provider string
		endpoint string
	}{
		{"claude", "https://api.anthropic.com"},
		{"openai", "https://api.openai.com"},
		{"ollama", "http://localhost:11434"},
	}
	for _, tt := range tests {
		data := []byte("---\nname: test\nbrain:\n  provider: " + tt.provider + "\n---\nBody")
		s, err := ParseSoul(data)
		if err != nil {
			t.Fatalf("provider %s: %v", tt.provider, err)
		}
		if s.Meta.Brain.Endpoint != tt.endpoint {
			t.Errorf("provider %s: expected endpoint %q, got %q", tt.provider, tt.endpoint, s.Meta.Brain.Endpoint)
		}
	}
}

func TestParseSoul_CustomEndpoint(t *testing.T) {
	data := []byte("---\nname: test\nbrain:\n  endpoint: http://custom:1234\n---\nBody")
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Brain.Endpoint != "http://custom:1234" {
		t.Errorf("expected custom endpoint, got %q", s.Meta.Brain.Endpoint)
	}
}

func TestParseSoul_EnvAPIKey(t *testing.T) {
	t.Setenv("TEST_API_KEY_XYZ", "sk-test-secret")
	data := []byte("---\nname: test\nbrain:\n  api_key: $TEST_API_KEY_XYZ\n---\nBody")
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Brain.APIKey != "sk-test-secret" {
		t.Errorf("expected API key from env, got %q", s.Meta.Brain.APIKey)
	}
}

func TestParseSoul_TemplateReplacement(t *testing.T) {
	data := []byte("---\nname: test\n---\nToday is {{current_date}}.")
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(s.SystemPrompt, "{{current_date}}") {
		t.Error("expected {{current_date}} to be replaced")
	}
}

func TestParseSoul_FullFields(t *testing.T) {
	data := []byte(`---
name: fulltest
version: "2.0"
brain:
  provider: openai
  model: gpt-4
  endpoint: https://api.openai.com
  temperature: 0.5
  context_length: 4096
  api_key: sk-inline-key
permissions:
  shell: true
  shell_timeout: 60
  network: true
  filesystem:
    allow: ["/tmp"]
    deny: ["/etc"]
  screen: true
  input: true
  ui: true
  record: true
skills:
  enable: [shell, file_read]
  output_dir: /tmp/output
  dir: /tmp/skills
---
You are a full test soul.`)
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Brain.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %q", s.Meta.Brain.Model)
	}
	if s.Meta.Brain.Temperature != 0.5 {
		t.Errorf("expected temperature 0.5, got %f", s.Meta.Brain.Temperature)
	}
	if s.Meta.Brain.ContextLength != 4096 {
		t.Errorf("expected context_length 4096, got %d", s.Meta.Brain.ContextLength)
	}
	if !s.Meta.Permissions.Shell {
		t.Error("expected shell permission to be true")
	}
	if s.Meta.Permissions.ShellTimeout != 60 {
		t.Errorf("expected shell_timeout 60, got %d", s.Meta.Permissions.ShellTimeout)
	}
	if !s.Meta.Permissions.Network {
		t.Error("expected network permission to be true")
	}
	if len(s.Meta.Permissions.Filesystem.Allow) != 1 || s.Meta.Permissions.Filesystem.Allow[0] != "/tmp" {
		t.Errorf("unexpected filesystem allow: %v", s.Meta.Permissions.Filesystem.Allow)
	}
	if s.Meta.Skills.OutputDir != "/tmp/output" {
		t.Errorf("expected output_dir '/tmp/output', got %q", s.Meta.Skills.OutputDir)
	}
	if s.Meta.Skills.Dir != "/tmp/skills" {
		t.Errorf("expected skills dir '/tmp/skills', got %q", s.Meta.Skills.Dir)
	}
	// All four KinKit claws should be parsed.
	if !s.Meta.Permissions.Screen {
		t.Error("expected screen permission to be true")
	}
	if !s.Meta.Permissions.Input {
		t.Error("expected input permission to be true")
	}
	if !s.Meta.Permissions.UI {
		t.Error("expected ui permission to be true")
	}
	if !s.Meta.Permissions.Record {
		t.Error("expected record permission to be true")
	}
}

// TestParseSoul_ClawPermissions exercises the four computer-use
// permission bits in isolation, including the default-false behavior
// when a soul omits a bit (older souls written before the claw was
// added must continue to parse cleanly).
func TestParseSoul_ClawPermissions(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want struct{ screen, input, ui, record bool }
	}{
		{
			name: "all_off_default",
			yaml: "---\nname: x\n---\nbody",
			want: struct{ screen, input, ui, record bool }{false, false, false, false},
		},
		{
			name: "all_on",
			yaml: "---\nname: x\npermissions:\n  screen: true\n  input: true\n  ui: true\n  record: true\n---\nbody",
			want: struct{ screen, input, ui, record bool }{true, true, true, true},
		},
		{
			name: "record_only",
			yaml: "---\nname: x\npermissions:\n  record: true\n---\nbody",
			want: struct{ screen, input, ui, record bool }{false, false, false, true},
		},
		{
			name: "legacy_no_record_key",
			// Older soul written before record existed — must still parse,
			// record defaults to false.
			yaml: "---\nname: x\npermissions:\n  screen: true\n  input: true\n  ui: true\n---\nbody",
			want: struct{ screen, input, ui, record bool }{true, true, true, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := ParseSoul([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseSoul: %v", err)
			}
			got := struct{ screen, input, ui, record bool }{
				s.Meta.Permissions.Screen,
				s.Meta.Permissions.Input,
				s.Meta.Permissions.UI,
				s.Meta.Permissions.Record,
			}
			if got != tt.want {
				t.Errorf("permissions = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLocationContextSubstitution covers $KINCLAW_LOCATION env var
// → {{location}} / {{lat}} / {{lon}} / {{city}} / {{country}}
// substitutions in the soul prompt. All four supported formats:
//
//	"lat,lon"
//	"lat,lon,city"
//	"lat,lon,city,country"
//	"" (unset → empty substitutions)
func TestLocationContextSubstitution(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		wantLoc  string
		wantLat  string
		wantLon  string
		wantCity string
		wantCC   string
	}{
		{"unset", "", "", "", "", "", ""},
		{"lat_lon_only", "39.9042,116.4074", "39.9042, 116.4074", "39.9042", "116.4074", "", ""},
		{"with_city", "37.7749,-122.4194,SF", "SF (37.7749, -122.4194)", "37.7749", "-122.4194", "SF", ""},
		{"full", "37.7749,-122.4194,SF,USA", "SF, USA (37.7749, -122.4194)", "37.7749", "-122.4194", "SF", "USA"},
		{"chinese_city", "39.9042,116.4074,北京,中国", "北京, 中国 (39.9042, 116.4074)", "39.9042", "116.4074", "北京", "中国"},
		{"whitespace_tolerated", " 1.0 , 2.0 , City ", "City (1.0, 2.0)", "1.0", "2.0", "City", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KINCLAW_LOCATION", tt.env)
			loc, lat, lon, city, country := locationContext()
			if loc != tt.wantLoc {
				t.Errorf("location = %q, want %q", loc, tt.wantLoc)
			}
			if lat != tt.wantLat {
				t.Errorf("lat = %q, want %q", lat, tt.wantLat)
			}
			if lon != tt.wantLon {
				t.Errorf("lon = %q, want %q", lon, tt.wantLon)
			}
			if city != tt.wantCity {
				t.Errorf("city = %q, want %q", city, tt.wantCity)
			}
			if country != tt.wantCC {
				t.Errorf("country = %q, want %q", country, tt.wantCC)
			}
		})
	}
}

// TestLocationFullPipeline — verify the env var really makes it into
// the rendered system prompt. Smoke-test for the integration.
func TestLocationFullPipeline(t *testing.T) {
	t.Setenv("KINCLAW_LOCATION", "39.9042,116.4074,北京,中国")
	dir := t.TempDir()
	path := dir + "/test.soul.md"
	body := []byte("---\nname: t\n---\nbody.\n位置: {{location}}\n经纬: {{lat}}, {{lon}}")
	os.WriteFile(path, body, 0644)
	s, err := LoadSoul(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.SystemPrompt, "北京, 中国 (39.9042, 116.4074)") {
		t.Errorf("expected formatted location in prompt, got %q", s.SystemPrompt)
	}
	if !strings.Contains(s.SystemPrompt, "经纬: 39.9042, 116.4074") {
		t.Errorf("expected raw lat/lon substitution, got %q", s.SystemPrompt)
	}
}

func TestTimezoneSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.soul.md"
	os.WriteFile(path, []byte("---\nname: t\n---\nTZ={{tz}}"), 0644)
	s, _ := LoadSoul(path)
	// Just verify the placeholder was replaced (don't assert specific TZ —
	// depends on the test machine).
	if strings.Contains(s.SystemPrompt, "{{tz}}") {
		t.Error("{{tz}} was not substituted")
	}
	if !strings.Contains(s.SystemPrompt, "TZ=") {
		t.Error("TZ= prefix missing")
	}
}

// TestLoadSoul_LearnedNotebookInjection covers the kernel-level
// injection of ~/.kinclaw/learned.md into every soul's system
// prompt. This is the persistence layer for Genesis Protocol — the
// agent's notebook of learnings travels with it across sessions.
func TestLoadSoul_LearnedNotebookInjection(t *testing.T) {
	// Redirect $HOME so we don't touch the user's real notebook.
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if err := os.MkdirAll(tempHome+"/.kinclaw", 0755); err != nil {
		t.Fatal(err)
	}
	notebook := "## com.apple.testapp\n- foo\n- bar\n"
	if err := os.WriteFile(tempHome+"/.kinclaw/learned.md", []byte(notebook), 0644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	soulPath := dir + "/test.soul.md"
	os.WriteFile(soulPath, []byte("---\nname: tester\n---\nyou are tester."), 0644)

	s, err := LoadSoul(soulPath)
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if !strings.Contains(s.SystemPrompt, "已学到的") {
		t.Errorf("system prompt should include '已学到的' header, got %q", s.SystemPrompt)
	}
	if !strings.Contains(s.SystemPrompt, "com.apple.testapp") {
		t.Errorf("system prompt should include notebook content, got %q", s.SystemPrompt)
	}
}

// TestLoadSoul_NoLearnedNotebook — soul still loads cleanly when
// learned.md doesn't exist (typical first-time-running state).
func TestLoadSoul_NoLearnedNotebook(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	// Note: no learned.md file created.

	dir := t.TempDir()
	soulPath := dir + "/test.soul.md"
	os.WriteFile(soulPath, []byte("---\nname: tester\n---\nclean state."), 0644)

	s, err := LoadSoul(soulPath)
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if strings.Contains(s.SystemPrompt, "已学到的") {
		t.Errorf("system prompt should NOT have learned section when notebook missing, got %q", s.SystemPrompt)
	}
}

// TestLoadSoul_LearnedNotebookCappedAtSize — runaway notebooks get
// truncated to the most recent 8KB so they can't blow context.
func TestLoadSoul_LearnedNotebookCappedAtSize(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	os.MkdirAll(tempHome+"/.kinclaw", 0755)

	// Generate ~12KB notebook with marker lines at the start and end.
	var buf strings.Builder
	buf.WriteString("MARKER_START_OLD\n")
	for i := 0; i < 1200; i++ {
		buf.WriteString("padding line that should be truncated\n")
	}
	buf.WriteString("MARKER_END_RECENT\n")
	os.WriteFile(tempHome+"/.kinclaw/learned.md", []byte(buf.String()), 0644)

	dir := t.TempDir()
	soulPath := dir + "/test.soul.md"
	os.WriteFile(soulPath, []byte("---\nname: tester\n---\nbody."), 0644)

	s, err := LoadSoul(soulPath)
	if err != nil {
		t.Fatalf("LoadSoul: %v", err)
	}
	if strings.Contains(s.SystemPrompt, "MARKER_START_OLD") {
		t.Error("oldest content should have been truncated")
	}
	if !strings.Contains(s.SystemPrompt, "MARKER_END_RECENT") {
		t.Error("most recent content should be preserved")
	}
}

func TestParseSoul_BootMessage(t *testing.T) {
	data := []byte(`---
name: booter
boot:
  message: "hello boot"
---
Boot test.`)
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Boot.Message != "hello boot" {
		t.Errorf("expected boot message 'hello boot', got %q", s.Meta.Boot.Message)
	}
}

func TestLoadSoul_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.soul.md")
	content := "---\nname: fromfile\n---\nBody text."
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSoul(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Meta.Name != "fromfile" {
		t.Errorf("expected name 'fromfile', got %q", s.Meta.Name)
	}
	if s.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, s.FilePath)
	}
}

func TestLoadSoul_FileNotFound(t *testing.T) {
	_, err := LoadSoul("/nonexistent/path.soul.md")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadSoul_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.soul.md")
	content := "---\n: : invalid\n---\nBody"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSoul(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseSoul_PermissionGateAndHooksAndContext(t *testing.T) {
	data := []byte(`---
name: gated
permissions:
  mode: ask
  ask: ["shell", "ui(click*)"]
  allow: ["shell(git status*)"]
hooks:
  pre_tool:
    - match: "input"
      run: "./guard.sh"
      timeout: 3
  stop:
    - run: "afplay done.aiff"
context:
  compact_at: 0.6
  keep_recent: 12
---
body`)
	s, err := ParseSoul(data)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Meta.Permissions
	if p.Mode != "ask" || len(p.Ask) != 2 || p.Ask[1] != "ui(click*)" || len(p.Allow) != 1 {
		t.Fatalf("permissions: %+v", p)
	}
	h := s.Meta.Hooks
	if len(h.PreTool) != 1 || h.PreTool[0].Match != "input" || h.PreTool[0].Timeout != 3 || len(h.Stop) != 1 {
		t.Fatalf("hooks: %+v", h)
	}
	if s.Meta.Context.CompactAt != 0.6 || s.Meta.Context.KeepRecent != 12 {
		t.Fatalf("context: %+v", s.Meta.Context)
	}
}

func TestParseSoul_DefaultsLeaveGateAuto(t *testing.T) {
	s, err := ParseSoul([]byte("---\nname: plain\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Meta.Permissions.Mode != "" || !s.Meta.Hooks.Empty() || s.Meta.Context.CompactAt != 0 {
		t.Fatalf("unexpected defaults: %+v", s.Meta)
	}
}

func TestUserInstructionsAndLearnedNotebook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(cwd)
	t.Cleanup(func() { os.Chdir(old) })

	os.MkdirAll(filepath.Join(home, ".kinclaw"), 0o755)
	os.MkdirAll(filepath.Join(home, ".localkin"), 0o755)
	os.WriteFile(filepath.Join(home, ".kinclaw", "KINCLAW.md"), []byte("Always reply in 中文."), 0o644)
	os.WriteFile(filepath.Join(cwd, "KINCLAW.md"), []byte("This repo: run make test before commit."), 0o644)
	// Canonical + legacy notebooks, with one duplicated bullet.
	os.WriteFile(LearnedPath(), []byte("# notes\n\n## com.apple.Notes\n- cmd+N works\n"), 0o644)
	os.WriteFile(legacyLearnedPath(), []byte("# notes\n\n## Common: time\n- check date first\n\n## com.apple.Notes\n- cmd+N works\n"), 0o644)

	s, err := ParseSoul([]byte("---\nname: x\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Always reply in 中文.", "run make test before commit", "cmd+N works", "check date first"} {
		if !strings.Contains(s.SystemPrompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Count(s.SystemPrompt, "cmd+N works") != 1 {
		t.Errorf("legacy duplicate bullet should be deduplicated")
	}
	if !strings.Contains(s.SystemPrompt, "用户的固定指令") {
		t.Errorf("KINCLAW.md section header missing")
	}
}

func TestLearnedNotebook_IndexWhenOversized(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".kinclaw"), 0o755)
	var sb strings.Builder
	sb.WriteString("# notes\n")
	for i := 0; i < 40; i++ {
		sb.WriteString(fmt.Sprintf("\n## com.example.app%d\n", i))
		for j := 0; j < 5; j++ {
			sb.WriteString(fmt.Sprintf("- app%d lesson %d: %s\n", i, j, strings.Repeat("detail ", 8)))
		}
	}
	os.WriteFile(LearnedPath(), []byte(sb.String()), 0o644)

	got := readLearnedNotebook()
	if len(got) > maxLearnedInPrompt+512 {
		t.Fatalf("notebook injection exceeds budget: %d", len(got))
	}
	if !strings.Contains(got, "Notebook index") || !strings.Contains(got, "com.example.app0 (5)") {
		t.Fatalf("index should list the oldest topic even though its body was cut: %s", got[:400])
	}
	if !strings.Contains(got, "com.example.app39") {
		t.Fatal("tail should keep the newest topic")
	}
}
