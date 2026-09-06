package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// expandTilde resolves a leading "~" or "~/" in a path to the user's
// home dir. Without this, file_read / file_write / file_edit on a
// path like "~/Library/Caches/kinclaw/researcher/foo.md" treats "~"
// as a literal directory name relative to cwd — and helpers running
// from kinclaw-mac end up creating "~/Library/..." INSIDE the
// kinclaw-mac repo. Real bug observed: researcher's first successful
// report landed at .../kinclaw-mac/~/Library/Caches/... instead of
// $HOME/Library/Caches/...
//
// Mirrors the helper in cmd/kinclaw/serve.go (expandHome). Kept
// internal to this package — too small to warrant a shared util.
func expandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to original path; filepath.Abs will then create
		// a literal "~/foo" relative to cwd. Surfaced via tool result
		// so the model can see something went sideways and re-emit
		// an absolute path on retry.
		return p
	}
	if p == "~" {
		return home
	}
	if p[1] == '/' {
		return filepath.Join(home, p[2:])
	}
	// "~user/foo" form — we don't resolve other users' homes; pass
	// through unchanged. (Standard shells do, but our threat model
	// already restricts skill writes to permission-allowed dirs, so
	// not implementing this is safer than getting it subtly wrong.)
	return p
}

// envDenylist are exact env var names to strip from shell environment.
var envDenylist = map[string]bool{
	"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true,
	"AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true,
	"GITHUB_TOKEN": true, "GH_TOKEN": true,
	"GOOGLE_API_KEY": true, "AZURE_API_KEY": true,
	"DATABASE_URL": true, "REDIS_URL": true,
}

func SafeEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		k := strings.SplitN(e, "=", 2)[0]
		if envDenylist[k] {
			continue
		}
		env = append(env, e)
	}
	return env
}

type shellSkill struct {
	timeout   time.Duration
	workspace string
}

func NewShellSkill(timeoutSec int) Skill {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	return &shellSkill{timeout: time.Duration(timeoutSec) * time.Second}
}

// SetWorkspace makes commands run inside the workspace directory.
func (s *shellSkill) SetWorkspace(dir string) { s.workspace = dir }

func (s *shellSkill) Name() string { return "shell" }
func (s *shellSkill) Description() string {
	return "Execute a shell command and return stdout+stderr (max 128KB). Use for: running tests, git, grep/rg, build commands. Prefer file_read over cat. Prefer file_edit over sed. Dangerous commands are blocked."
}
func (s *shellSkill) ToolDef() json.RawMessage {
	return MakeToolDef("shell", s.Description(),
		map[string]map[string]string{
			"command": {"type": "string", "description": "Shell command to execute. Use && to chain commands."},
		}, []string{"command"})
}

// blockedPatterns are regex patterns for dangerous shell commands.
var blockedPatterns = []*regexp.Regexp{
	// Destructive filesystem
	regexp.MustCompile(`rm\s+-[a-z]*r[a-z]*f[a-z]*\s+/`),
	regexp.MustCompile(`mkfs\.`),
	regexp.MustCompile(`dd\s+if=`),
	regexp.MustCompile(`:\(\)\s*\{`), // fork bomb

	// System control
	regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`),

	// Command obfuscation / injection
	regexp.MustCompile(`\bbash\s+-c\b`),
	regexp.MustCompile(`\beval\s+`),
	regexp.MustCompile(`\|\s*(bash|sh|python[23]?|perl|ruby|node)\b`),
	regexp.MustCompile(`\bcurl\s+.*\|\s*(ba)?sh\b`),
	regexp.MustCompile(`\bwget\s+.*\|\s*(ba)?sh\b`),

	// Reverse shells / data exfiltration
	regexp.MustCompile(`\bnc\s+-[a-z]*e\b`),
	regexp.MustCompile(`/dev/tcp/`),
	regexp.MustCompile(`\bbase64\s+--decode\b`),

	// Sensitive paths
	regexp.MustCompile(`\.(ssh|aws|gnupg)/`),
	regexp.MustCompile(`\.(env|bashrc|zshrc|bash_profile)\b`),
}

func (s *shellSkill) Execute(params map[string]string) (string, error) {
	command := params["command"]
	if command == "" {
		return "", fmt.Errorf("command is required")
	}
	for _, pat := range blockedPatterns {
		if pat.MatchString(command) {
			return "", fmt.Errorf("blocked: dangerous command pattern '%s'", pat.String())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = SafeEnv()
	if s.workspace != "" {
		cmd.Dir = s.workspace
	}
	out, err := cmd.CombinedOutput()
	const maxOutput = 128 * 1024
	result := string(out)
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n... (truncated)"
	}
	if err != nil {
		return result + "\nError: " + err.Error(), nil
	}
	return result, nil
}

type fileReadSkill struct{ workspace string }

func NewFileReadSkill() Skill                    { return &fileReadSkill{} }
func (s *fileReadSkill) SetWorkspace(dir string) { s.workspace = dir }
func (s *fileReadSkill) Name() string            { return "file_read" }
func (s *fileReadSkill) Description() string {
	return "Read file contents (max 64KB). Always read a file before editing it."
}
func (s *fileReadSkill) ToolDef() json.RawMessage {
	return MakeToolDef("file_read", s.Description(),
		map[string]map[string]string{
			"path": {"type": "string", "description": "File path to read"},
		}, []string{"path"})
}

func (s *fileReadSkill) Execute(params map[string]string) (string, error) {
	path := params["path"]
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := resolvePath(path, s.workspace)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	const maxSize = 64 * 1024
	content := string(data)
	if len(content) > maxSize {
		content = content[:maxSize] + "\n... (truncated, file too large)"
	}
	return content, nil
}

type fileWriteSkill struct{ workspace string }

func NewFileWriteSkill() Skill                    { return &fileWriteSkill{} }
func (s *fileWriteSkill) SetWorkspace(dir string) { s.workspace = dir }
func (s *fileWriteSkill) Name() string            { return "file_write" }
func (s *fileWriteSkill) Description() string {
	return "Write content to a file. OVERWRITES entire file. For partial edits use file_edit instead."
}
func (s *fileWriteSkill) ToolDef() json.RawMessage {
	return MakeToolDef("file_write", s.Description(),
		map[string]map[string]string{
			"path":    {"type": "string", "description": "File path. Parent dirs created automatically."},
			"content": {"type": "string", "description": "Complete file content (replaces everything)."},
		}, []string{"path", "content"})
}

func (s *fileWriteSkill) Execute(params map[string]string) (string, error) {
	path := params["path"]
	content := params["content"]
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := resolvePath(path, s.workspace)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return "", err
	}
	// Diff against what was there so the UI (and the model) sees what
	// changed, not just that something did. A brand-new file shows as
	// all additions.
	previous := ""
	if old, err := os.ReadFile(abs); err == nil {
		previous = string(old)
	}
	if err := os.WriteFile(abs, []byte(content), 0644); err != nil {
		return "", err
	}
	summary := fmt.Sprintf("Written %d bytes to %s", len(content), abs)
	if d := UnifiedDiff(previous, content); d != "" {
		return summary + "\n" + d, nil
	}
	return summary, nil
}

type fileEditSkill struct{ workspace string }

func NewFileEditSkill() Skill                    { return &fileEditSkill{} }
func (s *fileEditSkill) SetWorkspace(dir string) { s.workspace = dir }
func (s *fileEditSkill) Name() string            { return "file_edit" }
func (s *fileEditSkill) Description() string {
	return "Edit a file by replacing exact text. The old_text must appear exactly once. Read the file first to get the exact text."
}
func (s *fileEditSkill) ToolDef() json.RawMessage {
	return MakeToolDef("file_edit", s.Description(),
		map[string]map[string]string{
			"path":     {"type": "string", "description": "File path to edit"},
			"old_text": {"type": "string", "description": "Exact text to find (must be unique in file)"},
			"new_text": {"type": "string", "description": "Replacement text"},
		}, []string{"path", "old_text", "new_text"})
}

func (s *fileEditSkill) Execute(params map[string]string) (string, error) {
	path, oldText, newText := params["path"], params["old_text"], params["new_text"]
	if path == "" || oldText == "" {
		return "", fmt.Errorf("path and old_text are required")
	}
	abs, err := resolvePath(path, s.workspace)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return "", fmt.Errorf("old_text not found in file")
	}
	if count > 1 {
		return "", fmt.Errorf("old_text found %d times, must be unique (provide more context)", count)
	}
	edited := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(abs, []byte(edited), 0644); err != nil {
		return "", err
	}
	summary := fmt.Sprintf("Edited %s: replaced 1 occurrence", abs)
	if d := UnifiedDiff(content, edited); d != "" {
		return summary + "\n" + d, nil
	}
	return summary, nil
}

type webFetchSkill struct{}

func NewWebFetchSkill() Skill         { return &webFetchSkill{} }
func (s *webFetchSkill) Name() string { return "web_fetch" }
func (s *webFetchSkill) Description() string {
	return "Fetch a URL and return its content as readable text"
}
func (s *webFetchSkill) ToolDef() json.RawMessage {
	return MakeToolDef("web_fetch", s.Description(),
		map[string]map[string]string{
			"url": {"type": "string", "description": "The URL to fetch"},
		}, []string{"url"})
}

func (s *webFetchSkill) Execute(params map[string]string) (string, error) {
	rawURL := params["url"]
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}
	if isPrivateURL(rawURL) {
		return "", fmt.Errorf("blocked: cannot fetch private/internal URLs")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "LocalKin/1.0 (AI Agent Runtime)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return "", err
	}
	contentType := resp.Header.Get("Content-Type")
	content := string(body)
	if strings.Contains(contentType, "text/html") {
		content = htmlToText(content)
	}
	const maxOutput = 32 * 1024
	if len(content) > maxOutput {
		content = content[:maxOutput] + "\n... (truncated)"
	}
	return "---BEGIN UNTRUSTED WEB CONTENT---\n" + content + "\n---END UNTRUSTED WEB CONTENT---", nil
}

func isPrivateURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0" {
		return true
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
		if ip.Equal(net.ParseIP("169.254.169.254")) {
			return true // cloud metadata endpoint
		}
	}
	return false
}

func htmlToText(s string) string {
	for _, tag := range []string{"script", "style"} {
		for {
			lo := strings.ToLower(s)
			i := strings.Index(lo, "<"+tag)
			if i < 0 {
				break
			}
			j := strings.Index(lo[i:], "</"+tag+">")
			if j < 0 {
				break
			}
			s = s[:i] + s[i+j+len("</"+tag+">"):]
		}
	}
	var sb strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			sb.WriteByte(' ')
		case !inTag:
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

type MemoryBackend interface {
	Save(key, value string) (string, error)
	Recall(query string) (string, error)
	// RecallMessages searches the raw messages table (every chat
	// message ever, not just curated facts). limit caps the number
	// of returned excerpts so the prompt budget doesn't blow.
	RecallMessages(query string, limit int) (string, error)
}

type memorySkill struct{ store MemoryBackend }

func NewMemorySkill(store MemoryBackend) Skill { return &memorySkill{store: store} }
func (s *memorySkill) Name() string            { return "memory" }
func (s *memorySkill) Description() string {
	return "Save or recall persistent memories. " +
		"action=save: store a key-value fact. " +
		"action=recall: search saved facts by query (default scope). " +
		"action=recall scope=history: search the raw conversation log " +
		"(every message ever, across all past kinclaw sessions of this soul) " +
		"— use this when the user asks 'what did we say about X' / 'last time we discussed Y' " +
		"and the answer isn't in the curated facts. " +
		"\n\nKEY CONVENTION (important): " +
		"- DURABLE user facts (their name, daughter, city, preferences) → use a BARE key like 'daughter_name' or 'home_city'. These survive across sessions and are auto-injected into your system prompt next time. " +
		"- TRANSIENT working memory (mid-task drafts, search hits, scratch notes you'll cite from in this turn) → prefix the key with '_' like '_finding_1' or '_draft_intro'. These get wiped on 'New session' so they don't bleed into unrelated future conversations. " +
		"When in doubt: would the user want this in next week's context? Yes → bare key. No → '_' prefix."
}
func (s *memorySkill) ToolDef() json.RawMessage {
	return MakeToolDef("memory", s.Description(),
		map[string]map[string]string{
			"action": {"type": "string", "description": "save or recall"},
			"key":    {"type": "string", "description": "Memory key (for save) — use dotted path like 'friends.Sarah.housing'"},
			"value":  {"type": "string", "description": "Memory value (for save)"},
			"query":  {"type": "string", "description": "Search query (for recall)"},
			"scope":  {"type": "string", "description": "For recall: 'memories' (default, k-v facts) or 'history' (raw chat log via LIKE search). 'all' searches both."},
			"limit":  {"type": "string", "description": "For recall scope=history/all: max excerpts to return (default 10)"},
		}, []string{"action"})
}

func (s *memorySkill) Execute(params map[string]string) (string, error) {
	switch params["action"] {
	case "save":
		if params["key"] == "" || params["value"] == "" {
			return "", fmt.Errorf("key and value are required for save")
		}
		return s.store.Save(params["key"], params["value"])
	case "recall":
		if params["query"] == "" {
			return "", fmt.Errorf("query is required for recall")
		}
		// Parse optional limit; tolerate non-numeric (just default).
		limit := 10
		if l := params["limit"]; l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
				limit = n
			}
		}
		switch params["scope"] {
		case "", "memories":
			return s.store.Recall(params["query"])
		case "history", "msgs", "messages":
			return s.store.RecallMessages(params["query"], limit)
		case "all", "both":
			a, _ := s.store.Recall(params["query"])
			b, _ := s.store.RecallMessages(params["query"], limit)
			return "## memories k-v\n" + a + "\n\n## messages history\n" + b, nil
		default:
			return "", fmt.Errorf("scope must be 'memories' / 'history' / 'all' (got %q)", params["scope"])
		}
	default:
		return "", fmt.Errorf("action must be 'save' or 'recall'")
	}
}

type forgeSkill struct {
	skillsDir string
	registry  *Registry
}

func NewForgeSkill(skillsDir string, registry *Registry) Skill {
	return &forgeSkill{skillsDir: skillsDir, registry: registry}
}
func (s *forgeSkill) Name() string { return "forge" }
func (s *forgeSkill) Description() string {
	return "Create a new skill by generating a SKILL.md file in skills/<name>/. " +
		"The new skill becomes immediately available next round." +
		"\n\n" +
		"**WHEN to forge — narrow on purpose**: ONLY when the UI-claw path actually FAILED for this " +
		"app, or is fundamentally impossible. Forge produces a SCRIPT FALLBACK, not a fast lane. " +
		"KinClaw's whole thesis is `5 claws drive UI` — translating every successful UI flow into " +
		"AppleScript undermines that thesis on every reuse.\n" +
		"\n" +
		"Forge is justified when:\n" +
		"  - app has no AX surface (menubar-only Docker, immersive games, AX-hostile shells)\n" +
		"  - UI flow is reliably blocked (modal sheet that re-spawns, focus-stealing daemon)\n" +
		"  - `ui click` / `input keystroke` failed ≥ 2 times for the same task — kernel will " +
		"complain anyway, that's your signal\n" +
		"\n" +
		"Forge is NOT justified when:\n" +
		"  - UI worked, you just want the script for speed (drive UI again next time — it's the " +
		"hand you're supposed to be flexing)\n" +
		"  - the task is single-shot (\"today buy milk\" — `learn` it, don't forge it)\n" +
		"  - a `learn` line would capture the lesson without packing it into a skill\n" +
		"\n" +
		"A correctly-forged skill is a confession: \"the UI claw can't do this on this app, here's " +
		"the bypass.\" It's never \"I chose the faster path.\"\n" +
		"\n" +
		"**Script reference (for the legitimate fallback cases)** — when you DO need to forge a " +
		"script bypass, these are the cleanest patterns:\n" +
		"  - osascript: `osascript -e 'tell app \"X\" to <action>'` (works without focus)\n" +
		"  - shell:     `sh -c \"<your-script>\" _` (the trailing `_` makes user args $1, $2, ...)\n" +
		"  - python3:   pair with `script_content` so the .py is forged alongside the SKILL.md\n" +
		"\n" +
		"**HARD rules — kernel REJECTS forge calls that violate any of these**:\n" +
		"  1. command[0] MUST be a real binary in $PATH (`sh`, `bash`, `python3`, `osascript`, " +
		"`curl`, `jq`, `awk`, `mdfind`, `defaults`, `open`, `bc`, ...).\n" +
		"  2. command[0] must NEVER be a kinclaw skill name (`ui`, `input`, `screen`, `record`, " +
		"`shell`, `tts`, `stt`, `web_*`, `forge`, `learn`, `app_open_clean`). Calling them via " +
		"subprocess always fails silently and produces a skill that lies about success forever after.\n" +
		"  3. `args` MUST be a JSON array of strings — `[\"-e\", \"tell app...\"]`, NOT a hand-rolled " +
		"YAML flow array `[-e tell app...]` (which won't parse).\n" +
		"  4. NO hardcoded screen coordinates — `click at {760, 150}` works only on this machine, " +
		"this resolution, this window position. Use a keyboard shortcut (cmd+F, cmd+L, ...) or " +
		"AppleScript that targets the app directly.\n" +
		"  5. `osascript` `-e` flags MUST be paired — each `-e` followed by a script string, " +
		"not another flag, not nothing.\n" +
		"  6. Every `{{var}}` in args MUST appear in `schema` (otherwise substitution silently " +
		"strips it and the skill loses its parameterization).\n" +
		"  7. The produced SKILL.md is round-tripped through the YAML loader before registering. " +
		"If it doesn't reload, the forge is rejected and the dir is deleted.\n" +
		"\n" +
		"**Example syntax (for legitimate bypass cases)**:\n" +
		"```\n" +
		"command: osascript\n" +
		"args:    [\"-e\", \"tell application \\\"Reminders\\\" to make new reminder with properties {name:\\\"{{title}}\\\"}\"]\n" +
		"schema:  {\"title\": {\"type\": \"string\", \"required\": true}}\n" +
		"```\n" +
		"Use this shape when the UI path is genuinely blocked — not when you just want it faster." +
		"\n\n" +
		"If you get `forge rejected:` back, the kernel's quality gate caught a malformed recipe; " +
		"fix the specific issue it names and retry. Recovering from a rejection is normal — " +
		"forging blindly is not."
}
func (s *forgeSkill) ToolDef() json.RawMessage {
	return MakeToolDef("forge", s.Description(),
		map[string]map[string]string{
			"name":           {"type": "string", "description": "Skill name (alphanumeric and underscores)"},
			"description":    {"type": "string", "description": "What the skill does"},
			"command":        {"type": "string", "description": "Command to run (e.g. python3 script.py)"},
			"args":           {"type": "string", "description": "JSON array of argument templates (e.g. [\"{{prompt}}\"])"},
			"schema":         {"type": "string", "description": "JSON object of parameter definitions"},
			"script_content": {"type": "string", "description": "Content of the script file to create"},
		}, []string{"name", "description", "command"})
}

// internalSkillNames is the set of names that must NEVER appear as
// command[0] in a forged SKILL.md. They are kinclaw's own internal
// skills — there's no executable by these names in $PATH, so a forge
// that puts e.g. `command: ["ui", ...]` produces a SKILL.md that
// silently fails on every call. Pre-flight check below rejects it.
var internalSkillNames = map[string]bool{
	"ui": true, "input": true, "screen": true, "record": true,
	"shell": true, "tts": true, "stt": true, "forge": true,
	"learn": true, "memory": true, "clone": true,
	"file_read": true, "file_write": true, "file_edit": true,
	"web_fetch": true, "web_search": true, "web_browse": true,
	"app_open_clean": true,
}

// validateForgeCommand catches the most common LLM mistake when
// forging skills: putting an internal kinclaw skill name as
// command[0] (e.g. `command: ["ui", "action=click", ...]`). Those
// don't exist in $PATH; the resulting SKILL.md would be broken from
// day 1 and silently lie about success forever after.
//
// Also catches typos that don't resolve to anything in $PATH so the
// agent gets a clear "command not found" message instead of shipping
// a broken skill.
func validateForgeCommand(cmdParts []string) error {
	if len(cmdParts) == 0 {
		return fmt.Errorf("command must not be empty")
	}
	cmd0 := cmdParts[0]
	if internalSkillNames[cmd0] {
		return fmt.Errorf(
			"command[0] = %q is a kinclaw internal skill, not a shell binary. "+
				"Forged skills run in a subprocess and can't call kinclaw skills directly. "+
				"Use real OS tools instead: `osascript` for AppleScript, `sh -c` for shell, "+
				"`python3` for scripts, `curl` for HTTP, `mdfind` for Spotlight, etc.",
			cmd0)
	}
	if _, err := exec.LookPath(cmd0); err != nil {
		return fmt.Errorf(
			"command[0] = %q not found in $PATH. Use a real executable "+
				"(sh, bash, python3, osascript, curl, jq, awk, mdfind, open, bc, ...).",
			cmd0)
	}
	return nil
}

// hardcodedCoordPattern matches `click at {N, M}` — the AX anti-pattern that
// the agent occasionally shells out to via osascript "click at {760, 150}".
// Such coordinates depend on screen size + window position + display scaling,
// so the skill works once on the agent's bench and is broken everywhere else.
// We refuse it at forge time so the agent has to find a real solution
// (keyboard shortcut, AX-relative click, search-by-title).
var hardcodedCoordPattern = regexp.MustCompile(`(?i)click\s+at\s*\{\s*\d+\s*,\s*\d+\s*\}`)

// templateVarPattern matches `{{name}}` placeholders inside args. Used to
// check that every variable the args reference is actually declared in the
// schema — otherwise the agent will pass an unrecognized param at runtime
// and the substitution leaves a literal `{{name}}` in the command line.
var templateVarPattern = regexp.MustCompile(`\{\{(\w+)\}\}`)

// validateForgeArgs catches LLM mistakes one level deeper than command[0]:
// the args themselves. Three categories of bad-skill we observed in the
// wild and now refuse:
//
//  1. osascript with broken -e pairing: `-e` must be followed by a script
//     string, not another flag, not nothing. Without this check, a skill
//     forged as `args: ["-e", "tell ...", "-e"]` errors at every call.
//  2. Hardcoded screen coordinates in `click at {N, M}`: works once on
//     this agent's screen, broken on any other resolution / window size.
//     Force the agent to find a robust path (keystroke / AX-relative).
//  3. Template variables ({{x}}) referenced in args but not declared in
//     schema: the kinclaw template engine strips unknown vars to ""
//     (intentional — see external.go), so the args silently lose their
//     parameterization. Forge should fail loudly here, not silently degrade.
func validateForgeArgs(cmdParts []string, args []string, schema map[string]interface{}) error {
	// (1) osascript -e flag pairing
	if len(cmdParts) > 0 && filepath.Base(cmdParts[0]) == "osascript" {
		for i, a := range args {
			if a != "-e" {
				continue
			}
			if i+1 >= len(args) {
				return fmt.Errorf("osascript -e flag at args[%d] has no script after it (must be `-e <script>` pairs)", i)
			}
			next := args[i+1]
			if next == "-e" || (strings.HasPrefix(next, "-") && len(next) <= 3) {
				return fmt.Errorf("osascript -e flag at args[%d] is followed by another flag (%q) instead of a script", i, next)
			}
		}
	}

	// (2) hardcoded screen coordinates
	for i, a := range args {
		if hardcodedCoordPattern.MatchString(a) {
			return fmt.Errorf(
				"args[%d] contains hardcoded screen coordinates: %q. "+
					"These work only on this exact screen size + window layout — "+
					"use a robust alternative: `keystroke` for typed-text fields, "+
					"a keyboard shortcut (cmd+F, cmd+L, etc.) for menu/search bars, "+
					"or AX-relative click via the `ui` claw with role/title matchers.",
				i, a)
		}
	}

	// (3) template-var ↔ schema consistency
	used := map[string]bool{}
	for _, a := range args {
		for _, m := range templateVarPattern.FindAllStringSubmatch(a, -1) {
			used[m[1]] = true
		}
	}
	for v := range used {
		if _, ok := schema[v]; !ok {
			return fmt.Errorf(
				"args reference template variable {{%s}} but the schema doesn't declare a parameter named %q. "+
					"Add %s to the schema (with type/description), or remove the placeholder from args.",
				v, v, v)
		}
	}

	return nil
}

// forgeSkillFile is the on-disk shape we marshal to. Mirrors the YAML
// front-matter of an external SKILL.md (see external.go ExternalSkill).
// Using a struct + yaml.Marshal (rather than hand-concatenated strings)
// is what makes the produced YAML always parse cleanly — the agent
// passes JSON-shaped inputs, we marshal to canonical block-list YAML.
type forgeSkillFile struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description"`
	Command     []string               `yaml:"command"`
	Args        []string               `yaml:"args,omitempty"`
	Schema      map[string]interface{} `yaml:"schema,omitempty"`
}

func (s *forgeSkill) Execute(params map[string]string) (string, error) {
	name := params["name"]
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if !forgeNamePattern.MatchString(name) {
		return "", fmt.Errorf("forge rejected: name %q must be alphanumeric + underscore (regex: %s)", name, forgeNamePattern.String())
	}

	cmdParts := strings.Fields(params["command"])
	if err := validateForgeCommand(cmdParts); err != nil {
		return "", fmt.Errorf("forge rejected: %w", err)
	}

	// Parse args as JSON array of strings. The contract advertised in
	// ToolDef() is "JSON array" — we used to dump the raw string into
	// YAML which the agent often hand-rolled with broken flow-array
	// syntax. Round-tripping through JSON → []string → yaml.Marshal
	// produces canonical block-list YAML that always parses.
	var args []string
	if a := strings.TrimSpace(params["args"]); a != "" {
		if err := json.Unmarshal([]byte(a), &args); err != nil {
			return "", fmt.Errorf(
				"forge rejected: args must be a JSON array of strings, e.g. [\"-e\", \"tell app \\\"X\\\" to play\"]. Parse error: %w (got %q)",
				err, a)
		}
	}

	// Parse schema as JSON object.
	var schema map[string]interface{}
	if sc := strings.TrimSpace(params["schema"]); sc != "" {
		if err := json.Unmarshal([]byte(sc), &schema); err != nil {
			return "", fmt.Errorf("forge rejected: schema must be a JSON object: %w", err)
		}
	}

	// Domain checks: osascript -e pairing, no hardcoded coords, schema
	// covers all template vars referenced by args.
	if err := validateForgeArgs(cmdParts, args, schema); err != nil {
		return "", fmt.Errorf("forge rejected: %w", err)
	}

	// Build the SKILL.md content via yaml.Marshal — guarantees well-
	// formed YAML even when args contain double-quotes, backslashes,
	// braces, AppleScript syntax, etc.
	sf := forgeSkillFile{
		Name:        name,
		Description: params["description"],
		Command:     cmdParts,
		Args:        args,
		Schema:      schema,
	}
	yamlBytes, err := yaml.Marshal(sf)
	if err != nil {
		return "", fmt.Errorf("forge: yaml marshal: %w", err)
	}
	var content strings.Builder
	content.WriteString("---\n")
	content.Write(yamlBytes)
	content.WriteString("---\n\n# ")
	content.WriteString(name)
	content.WriteString("\n\nForged by LocalKin agent.\n")

	dir := filepath.Join(s.skillsDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Optional companion script (e.g. python3 run.py + run.py file).
	if script := params["script_content"]; script != "" {
		scriptName := "run.py"
		if len(cmdParts) > 1 {
			scriptName = cmdParts[len(cmdParts)-1]
		}
		scriptPath := filepath.Join(dir, scriptName)
		if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}

	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(content.String()), 0644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}

	// Round-trip validate: read back the just-written SKILL.md via the
	// same loader the registry uses at boot. If it doesn't parse here
	// it'll print a warning at every boot — refuse the forge instead so
	// the agent retries with a corrected recipe.
	ext, err := LoadExternalSkill(skillPath)
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf(
			"forge rejected: produced SKILL.md doesn't reload — %w. The skill was written then deleted; nothing was registered.",
			err)
	}
	s.registry.Register(ext)
	return fmt.Sprintf("Forged skill '%s' at %s", name, skillPath), nil
}

// forgeNamePattern restricts forged skill names to identifiers — keeps
// the on-disk dir / file names predictable and avoids weird shell-quoting
// situations downstream when paths get echoed in logs.
var forgeNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)
