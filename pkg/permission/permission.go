// Package permission is the kernel's approval gate for tool calls.
//
// Souls already decide *which* skills a model can see (skills.enable)
// and the shell skill blocks a short list of catastrophic commands.
// What was missing is the layer Claude Code puts between "the model
// wants to run this" and "it runs": a deterministic decision — allow,
// ask the human, or deny — made by the harness, not by the model's
// good intentions. The soul prose can say "ask before destructive
// operations"; the gate makes it true.
//
// Two modes. `auto` is today's behaviour and stays the default, so
// every programmatic caller (spawn, harvest, macbench) is unchanged.
// `ask` routes matching calls to an Asker — a terminal prompt in the
// REPL, an approval card in the Mac app — and a session-scoped "always
// allow" remembers the answer so the user isn't nagged twice.
//
// Plan mode sits on top of both: while on, only read-only calls go
// through, and the denial text tells the model to finish investigating
// and present a plan instead.
package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Mode is the soul-level default.
type Mode string

const (
	// ModeAuto executes every exposed skill without asking (legacy).
	ModeAuto Mode = "auto"
	// ModeAsk routes calls matching the ask rules to the Asker.
	ModeAsk Mode = "ask"
)

// Answer is what an Asker returns.
type Answer int

const (
	Deny Answer = iota
	AllowOnce
	// AllowSession approves this call and every later call matching the
	// same skill for the life of the process — Claude Code's "yes, and
	// don't ask again for this tool".
	AllowSession
	// AllowAlways also writes a rule (SuggestAllowRule) to the persisted
	// allow list, so the answer survives restarts — Claude Code's
	// "don't ask again" that lands in settings.json.
	AllowAlways
)

// Request is what the human sees.
type Request struct {
	ID      string            `json:"id"`
	Skill   string            `json:"skill"`
	Params  map[string]string `json:"params,omitempty"`
	Summary string            `json:"summary"`
	// Reason says why the gate stopped here: which rule matched, or
	// that the command looked dangerous.
	Reason string `json:"reason"`
}

// Asker presents a Request and returns the human's Answer. It should
// honour ctx — an interrupted turn must not leave a prompt dangling.
type Asker interface {
	Ask(ctx context.Context, req Request) (Answer, error)
}

// AskerFunc adapts a function to Asker.
type AskerFunc func(ctx context.Context, req Request) (Answer, error)

// Ask implements Asker.
func (f AskerFunc) Ask(ctx context.Context, req Request) (Answer, error) { return f(ctx, req) }

// Verdict is the gate's decision for one call.
type Verdict struct {
	Allowed bool
	// Reason is model-facing when denied: it becomes the tool result so
	// the model can adapt rather than retry blindly.
	Reason string
	// Asked is true when a human was consulted.
	Asked bool
}

// Gate holds the rules and session state. Safe for concurrent use.
type Gate struct {
	mu       sync.Mutex
	mode     Mode
	ask      []rule
	allow    []rule
	asker    Asker
	session  map[string]bool // skill names approved for the session
	planMode bool
	seq      int

	// workspace, fsAllow and fsDeny scope file writes: writes under the
	// workspace or an fsAllow root pass silently (subject to rules);
	// writes elsewhere ask; writes under fsDeny are refused outright.
	workspace string
	fsAllow   []string
	fsDeny    []string
	// persist stores an AllowAlways rule; nil disables that answer.
	persist func(rule string) error
}

// New builds a gate. ask and allow use the rule grammar (see rule):
// a skill name, a `prefix*`, or `skill(param-prefix*)` to match on the
// skill's primary parameter — `shell(git push*)`, `ui(click*)`.
func New(mode Mode, ask, allow []string, asker Asker) *Gate {
	if mode == "" {
		mode = ModeAuto
	}
	return &Gate{
		mode: mode, ask: parseRules(ask), allow: parseRules(allow),
		asker: asker, session: map[string]bool{},
	}
}

// Mode returns the configured mode.
func (g *Gate) Mode() Mode { return g.mode }

// SetAsker installs (or replaces) the human-facing prompt.
func (g *Gate) SetAsker(a Asker) { g.mu.Lock(); g.asker = a; g.mu.Unlock() }

// SetPlanMode toggles the read-only gate.
func (g *Gate) SetPlanMode(on bool) { g.mu.Lock(); g.planMode = on; g.mu.Unlock() }

// PlanMode reports whether the read-only gate is on.
func (g *Gate) PlanMode() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.planMode }

// AllowSession approves a skill for the rest of the process (the
// `/permissions allow X` command, or an AllowSession answer).
func (g *Gate) AllowSession(skill string) { g.mu.Lock(); g.session[skill] = true; g.mu.Unlock() }

// AddAllowRule appends a rule to the live allow list.
func (g *Gate) AddAllowRule(spec string) {
	g.mu.Lock()
	g.allow = append(g.allow, parseRules([]string{spec})...)
	g.mu.Unlock()
}

// SetWorkspace sets the directory file writes may touch without asking.
func (g *Gate) SetWorkspace(dir string) { g.mu.Lock(); g.workspace = dir; g.mu.Unlock() }

// Workspace returns the current workspace directory.
func (g *Gate) Workspace() string { g.mu.Lock(); defer g.mu.Unlock(); return g.workspace }

// SetFilesystem installs the soul's permissions.filesystem allow / deny
// roots. `~` is expanded.
func (g *Gate) SetFilesystem(allow, deny []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fsAllow = g.fsAllow[:0]
	for _, p := range allow {
		g.fsAllow = append(g.fsAllow, expandTilde(strings.TrimSpace(p)))
	}
	g.fsDeny = g.fsDeny[:0]
	for _, p := range deny {
		g.fsDeny = append(g.fsDeny, expandTilde(strings.TrimSpace(p)))
	}
}

// SetPersist installs the writer for AllowAlways answers.
func (g *Gate) SetPersist(fn func(rule string) error) { g.mu.Lock(); g.persist = fn; g.mu.Unlock() }

// SessionAllowed lists skills approved for the session, sorted.
func (g *Gate) SessionAllowed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.session))
	for k := range g.session {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writesFiles reports whether a skill changes a file the user owns.
func writesFiles(skill string) bool { return skill == "file_write" || skill == "file_edit" }

// under reports whether path is root or inside it.
func under(path, root string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Check decides one call. It blocks while an Asker is consulted.
func (g *Gate) Check(ctx context.Context, skill string, params map[string]string) Verdict {
	g.mu.Lock()
	planMode, mode, asker := g.planMode, g.mode, g.asker
	sessionOK := g.session[skill]
	workspace, fsAllow, fsDeny := g.workspace, g.fsAllow, g.fsDeny
	allowRules := g.allow
	g.mu.Unlock()

	if planMode && !ReadOnly(skill, params) {
		return Verdict{Allowed: false, Reason: PlanModeDenial(skill)}
	}

	// permissions.filesystem.deny is absolute — it holds in auto mode
	// too, and no allow rule or session approval overrides it. Reads
	// are not gated: the shell skill already refuses the secret dirs,
	// and a read-only agent that can't look at /etc is not safer.
	target := ""
	if writesFiles(skill) {
		target = absPath(params["path"], workspace)
		for _, d := range fsDeny {
			if under(target, d) {
				return Verdict{Allowed: false, Reason: fmt.Sprintf(
					"Refused: %s is under %s, which permissions.filesystem.deny puts off limits.", target, d)}
			}
		}
	}
	if mode != ModeAsk {
		return Verdict{Allowed: true}
	}
	if matchAny(allowRules, skill, params) || sessionOK {
		return Verdict{Allowed: true}
	}

	reason := ""
	switch {
	case len(g.ask) > 0 && matchAny(g.ask, skill, params):
		reason = "matches a permissions.ask rule"
	case len(g.ask) == 0 && defaultAsk(skill):
		reason = "default ask set (shell / file writes / forge / MCP)"
	case skill == "shell" && DangerousShell(params["command"]):
		reason = "shell command looks irreversible or system-wide"
	case target != "" && !under(target, workspace) && !underAny(target, fsAllow):
		reason = "writes outside the workspace"
	default:
		return Verdict{Allowed: true}
	}

	if asker == nil {
		return Verdict{Allowed: false, Asked: false, Reason: fmt.Sprintf(
			"Permission required for %s (%s) but no interactive approver is attached. "+
				"Run kinclaw interactively, or grant it in the soul with permissions.allow (e.g. %q).",
			skill, reason, suggestAllowRule(skill, params))}
	}

	g.mu.Lock()
	g.seq++
	id := fmt.Sprintf("perm-%d", g.seq)
	g.mu.Unlock()

	ans, err := asker.Ask(ctx, Request{
		ID: id, Skill: skill, Params: params, Summary: Summary(skill, params), Reason: reason,
	})
	if err != nil {
		return Verdict{Allowed: false, Asked: true, Reason: "Permission request failed: " + err.Error()}
	}
	switch ans {
	case AllowOnce:
		return Verdict{Allowed: true, Asked: true}
	case AllowSession:
		g.AllowSession(skill)
		return Verdict{Allowed: true, Asked: true}
	case AllowAlways:
		rule := SuggestAllowRule(skill, params)
		g.AddAllowRule(rule)
		g.mu.Lock()
		persist := g.persist
		g.mu.Unlock()
		if persist != nil {
			if err := persist(rule); err != nil {
				return Verdict{Allowed: true, Asked: true, Reason: "allowed; could not persist rule: " + err.Error()}
			}
		}
		return Verdict{Allowed: true, Asked: true}
	default:
		return Verdict{Allowed: false, Asked: true, Reason: fmt.Sprintf(
			"Permission denied by the user for %s. Do not retry the same call; "+
				"explain what you wanted to do and ask how to proceed, or choose a different approach.", Summary(skill, params))}
	}
}

// defaultAsk is the ask set when a soul says mode: ask but lists no
// rules — the skills that change the machine or the repo outright.
// GUI claws are deliberately not here: in Cowork the clicking *is* the
// work, and a prompt per click makes the agent useless. Souls that
// want that add `ui(click*)` / `input` to permissions.ask.
func defaultAsk(skill string) bool {
	switch skill {
	case "shell", "file_write", "file_edit", "forge":
		return true
	}
	return strings.HasPrefix(skill, "mcp_")
}

// SuggestAllowRule is the rule an "always" answer persists: the skill
// plus the first word of its primary parameter (`shell(git*)`,
// `file_write(/Users/me/notes*)`), never the bare skill for shell — one
// "always" on `ls` must not silently approve `rm -rf` forever.
func SuggestAllowRule(skill string, params map[string]string) string {
	if p := primaryParam(skill); p != "" {
		if v := strings.TrimSpace(params[p]); v != "" {
			if p == "path" {
				return fmt.Sprintf("%s(%s*)", skill, filepath.Dir(absPath(v, "")))
			}
			f := strings.Fields(v)
			if len(f) > 0 {
				return fmt.Sprintf("%s(%s*)", skill, f[0])
			}
		}
	}
	return skill
}

func suggestAllowRule(skill string, params map[string]string) string {
	return SuggestAllowRule(skill, params)
}

// absPath expands ~ and resolves a relative path against the workspace
// (or cwd), for comparisons against roots.
func absPath(p, workspace string) string {
	p = expandTilde(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		base := workspace
		if base == "" {
			base, _ = os.Getwd()
		}
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if under(path, r) {
			return true
		}
	}
	return false
}

// ─── Rules ────────────────────────────────────────────────────────────

// rule is one entry of permissions.ask / permissions.allow.
//
//	shell             exact skill name
//	mcp_*             skill-name prefix
//	shell(git push*)  skill + primary-parameter prefix
//	ui(tree)          skill + primary-parameter exact
type rule struct {
	skill       string
	skillPrefix bool
	param       string // "" = no parameter constraint
	paramPrefix bool
	hasParam    bool
}

func parseRules(specs []string) []rule {
	out := make([]rule, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var r rule
		if i := strings.IndexByte(s, '('); i > 0 && strings.HasSuffix(s, ")") {
			r.skill = s[:i]
			r.param = s[i+1 : len(s)-1]
			r.hasParam = true
			if strings.HasSuffix(r.param, "*") {
				r.param = strings.TrimSuffix(r.param, "*")
				r.paramPrefix = true
			}
		} else {
			r.skill = s
		}
		if strings.HasSuffix(r.skill, "*") {
			r.skill = strings.TrimSuffix(r.skill, "*")
			r.skillPrefix = true
		}
		out = append(out, r)
	}
	return out
}

func (r rule) matches(skill string, params map[string]string) bool {
	if r.skillPrefix {
		if !strings.HasPrefix(skill, r.skill) {
			return false
		}
	} else if skill != r.skill {
		return false
	}
	if !r.hasParam {
		return true
	}
	key := primaryParam(skill)
	if key == "" {
		return false
	}
	v := strings.TrimSpace(params[key])
	want := r.param
	if key == "path" {
		// Models emit absolute paths; users write rules with `~`.
		// Compare both in expanded form so `file_write(~/.kinclaw*)`
		// covers /Users/me/.kinclaw/notes.md.
		v, want = expandTilde(v), expandTilde(want)
	}
	if r.paramPrefix {
		return strings.HasPrefix(v, want)
	}
	return v == want
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home + p[1:]
		}
	}
	return p
}

func matchAny(rules []rule, skill string, params map[string]string) bool {
	for _, r := range rules {
		if r.matches(skill, params) {
			return true
		}
	}
	return false
}

// Matches reports whether any spec in specs matches the call. Exported
// for callers that reuse the grammar (hooks, status UIs).
func Matches(specs []string, skill string, params map[string]string) bool {
	return matchAny(parseRules(specs), skill, params)
}

// primaryParam names the parameter a `skill(...)` rule constrains.
func primaryParam(skill string) string {
	switch skill {
	case "shell":
		return "command"
	case "cerebellum", "kinthink":
		return "cmd"
	case "file_read", "file_write", "file_edit":
		return "path"
	case "app_open_clean":
		return "app"
	case "spawn":
		return "soul"
	case "web_fetch", "kinbrowser", "web":
		return "url"
	case "web_search":
		return "query"
	case "ui", "input", "screen", "record", "kinbrain", "memory", "smart_click":
		return "action"
	}
	return ""
}

// ─── Read-only classification (plan mode) ────────────────────────────

// ReadOnly reports whether a call observes without changing anything
// the user would notice: no clicks, keystrokes, file writes, shell,
// or outbound messages. Reading the screen, the AX tree, files, the
// web, and the agent's own notes all count as read-only; the agent's
// memory and todo list are its notebook and stay writable in plan mode.
// Unknown skills (external SKILL.md, MCP tools) are treated as mutating.
func ReadOnly(skill string, params map[string]string) bool {
	action := params["action"]
	switch skill {
	case "file_read", "web_search", "web_fetch", "kinbrowser", "kinbrain",
		"memory", "learn", "todo_write", "location", "weather", "translate",
		"summarize", "stt", "ask_user", "tool_search":
		return true
	case "screen":
		switch action {
		case "list_displays", "screenshot", "ocr", "ocr_regions", "diff_screenshots",
			"list_windows", "list_apps", "screenshot_app", "color_at_point":
			return true
		}
	case "ui":
		switch action {
		case "focused_app", "tree", "find", "read", "at_point", "watch", "actions",
			"app_state", "wait_until", "shortcut", "attribute", "spatial_find":
			return true
		case "select_text":
			return params["mode"] != "replace"
		}
	case "input":
		switch action {
		case "cursor", "screen_size":
			return true
		}
	case "record":
		switch action {
		case "list", "stats", "list_mics":
			return true
		}
	}
	return false
}

// PlanModeDenial is the tool result a blocked call receives in plan mode.
func PlanModeDenial(skill string) string {
	return fmt.Sprintf("PLAN MODE: %q changes the user's machine and is not allowed while planning. "+
		"Only observation is permitted (screen screenshot/ocr, ui tree/find/read/watch, file_read, web_search, "+
		"web_fetch, kinbrowser, kinbrain, memory, todo_write). Finish investigating, then end your reply with a "+
		"numbered plan of the exact actions you would take, and wait for the user to approve it.", skill)
}

// PlanModeDirective is appended to the system prompt while plan mode is on.
const PlanModeDirective = `

## PLAN MODE (active)

You are in plan mode. Investigate freely with read-only tools (screen, ui tree/find/read, file_read, web, kinbrain, memory), but do NOT click, type, run shell commands, write files, send anything, or call skills that change state — the kernel will refuse them. Keep todo_write updated with the plan. End your reply with a numbered, concrete plan of the actions you would take and ask the user to approve before acting.`

// ─── Dangerous shell heuristics ──────────────────────────────────────

// dangerousShell lists shell shapes that are irreversible, system-wide,
// or leave the machine in a state the user didn't ask for. Deliberately
// narrow: this decides whether to *ask*, not whether to block, and a
// prompt on every `rm` of a temp file trains the user to click through.
var dangerousShell = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[a-zA-Z]*[rR]`), // recursive delete
	regexp.MustCompile(`\bsudo\b`),              // privilege escalation
	regexp.MustCompile(`\bgit\s+push\b`),        // publishes
	regexp.MustCompile(`\bgit\s+(reset\s+--hard|clean\s+-[a-zA-Z]*f|checkout\s+--\s|branch\s+-D|tag\s+-d)\b`),
	regexp.MustCompile(`(?:^|[;&|]\s*)(?:sudo\s+)?(?:kill|killall|pkill)\b`), // stops the user's processes
	regexp.MustCompile(`\blaunchctl\s+(unload|bootout|remove|disable)\b`),
	regexp.MustCompile(`\bdefaults\s+(delete|write)\b`), // system preferences
	regexp.MustCompile(`\bdiskutil\s+(erase|reformat|partition|apfs\s+delete)`),
	regexp.MustCompile(`>\s*/dev/`),            // raw device writes
	regexp.MustCompile(`\bch(mod|own)\s+-R\b`), // recursive permission changes
	regexp.MustCompile(`\bcrontab\s+-r\b`),
	regexp.MustCompile(`\b(tccutil|csrutil|nvram|spctl)\b`), // security posture
	regexp.MustCompile(`\bosascript\b.*\b(delete|empty\s+trash|quit)\b`),
	regexp.MustCompile(`\b(npm|cargo|gem|pip)\s+publish\b`),
	regexp.MustCompile(`\bcurl\b[^|]*\|\s*(ba|z)?sh\b`),
}

// DangerousShell reports whether a shell command matches a shape that
// warrants a human look before it runs.
func DangerousShell(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	for _, re := range dangerousShell {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// ─── Human-facing summary ────────────────────────────────────────────

// Summary renders a call as one line a person can approve or refuse at
// a glance: the skill, its primary parameter, and a few other params.
func Summary(skill string, params map[string]string) string {
	const max = 160
	var sb strings.Builder
	sb.WriteString(skill)
	prim := primaryParam(skill)
	if v := params[prim]; prim != "" && v != "" {
		sb.WriteString(": " + v)
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == prim || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	extra := make([]string, 0, len(keys))
	for _, k := range keys {
		extra = append(extra, k+"="+params[k])
	}
	if len(extra) > 0 {
		sb.WriteString("  (" + strings.Join(extra, ", ") + ")")
	}
	s := strings.ReplaceAll(sb.String(), "\n", " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ─── Persisted allow rules ───────────────────────────────────────────

// PersistedFile is the on-disk store for "always allow" answers:
//
//	{"souls": {"KinClaw Pilot": {"allow": ["shell(git*)", "file_write(/Users/me/notes*)"]}}}
//
// Per soul, because "always" was answered while a particular soul was
// driving and a researcher soul should not inherit pilot's shell rules.
type PersistedFile struct {
	Souls map[string]struct {
		Allow []string `json:"allow"`
	} `json:"souls"`
}

// DefaultPersistPath is ~/.kinclaw/permissions.json.
func DefaultPersistPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kinclaw", "permissions.json")
}

// LoadPersisted returns the saved allow rules for a soul (nil if none).
func LoadPersisted(path, soul string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f PersistedFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Souls[soul].Allow
}

// SavePersisted appends one rule for a soul, creating the file as needed.
// Duplicates are ignored.
func SavePersisted(path, soul, rule string) error {
	if path == "" {
		return fmt.Errorf("no permissions file path")
	}
	f := PersistedFile{Souls: map[string]struct {
		Allow []string `json:"allow"`
	}{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &f)
		if f.Souls == nil {
			f.Souls = map[string]struct {
				Allow []string `json:"allow"`
			}{}
		}
	}
	entry := f.Souls[soul]
	for _, r := range entry.Allow {
		if r == rule {
			return nil
		}
	}
	entry.Allow = append(entry.Allow, rule)
	f.Souls[soul] = entry
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
