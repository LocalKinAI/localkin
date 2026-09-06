package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/applifecycle"
	"github.com/LocalKinAI/kinclaw/pkg/auth"
	"github.com/LocalKinAI/kinclaw/pkg/brain"
	"github.com/LocalKinAI/kinclaw/pkg/compact"
	"github.com/LocalKinAI/kinclaw/pkg/hooks"
	"github.com/LocalKinAI/kinclaw/pkg/mcp"
	"github.com/LocalKinAI/kinclaw/pkg/memory"
	"github.com/LocalKinAI/kinclaw/pkg/permission"
	"github.com/LocalKinAI/kinclaw/pkg/skill"
	"github.com/LocalKinAI/kinclaw/pkg/soul"
)

const (
	version = "1.18.0"
	// maxToolRounds caps the tool-call sequence per user turn. 20 was
	// fine for kernel-only flows but compound demos (record start + tts
	// + multi-step ui find/click/verify + tts + record stop) easily
	// burn 30+ rounds. Bumped to 50; the circuit breaker + ambiguity
	// guards still catch genuine runaways.
	maxToolRounds = 50
)

// session holds the mutable runtime state for the REPL.
type session struct {
	soul     *soul.Soul
	brain    brain.Brain
	registry *skill.Registry
	toolDefs []json.RawMessage
	store    *memory.SQLiteStore
	id       string
	history  []brain.Message
	debug    bool
	soulPath string

	// Live MCP server subprocesses backing the registry's mcp_* skills.
	// Held on the session because buildRegistry runs again on every soul
	// reload: without closing the previous set first, each reload would
	// leave a full set of orphaned server processes behind.
	mcpClients []*mcp.Client
	// Load outcomes and the config they came from, kept so GET /api/mcp can
	// report what the settings UI needs: not just which servers are running,
	// but which were configured and failed — the state worth surfacing.
	mcpResults []mcp.LoadResult
	mcpConfig  *mcp.Config

	// Pending detached-spawn results, drained at the start of the next
	// turn and prepended to messages as synthetic user messages so the
	// parent (typically pilot) sees what the child returned without
	// the user having to re-narrate it. Guarded by spawnMu — written
	// from the spawn skill's goroutine, read under turnMu by chatHandler.
	spawnMu      sync.Mutex
	pendingSpawn []skill.SpawnResult

	// gate decides allow / ask / deny per tool call (pkg/permission).
	// Always non-nil; in auto mode it only enforces plan mode.
	gate *permission.Gate
	// hooks runs the soul's pre_tool / post_tool / stop commands. nil
	// when the soul declares none.
	hooks *hooks.Runner
	// lastUsage is the provider-reported size of the most recent
	// prompt — the number compaction trusts. totalIn / totalOut are
	// the running sums for this process (/usage, /api/state).
	lastUsage         brain.Usage
	totalIn, totalOut int
}

// permissionModeFlag overrides the soul's permissions.mode when set
// ("auto" for scripts driving an ask-mode soul, "ask" to gate a soul
// that never opted in).
var permissionModeFlag string

// newGate builds the session's permission gate from the soul plus the
// CLI override, attaching the terminal approver when a human can
// answer. serve mode swaps in its own asker once the server exists.
func newGate(s *soul.Soul) *permission.Gate {
	mode := permission.Mode(s.Meta.Permissions.Mode)
	if permissionModeFlag != "" {
		mode = permission.Mode(permissionModeFlag)
	}
	var asker permission.Asker
	if stdinIsTerminal() {
		asker = cliAsker{}
	}
	return permission.New(mode, s.Meta.Permissions.Ask, s.Meta.Permissions.Allow, asker)
}

// newHooks builds the hook runner, or nil when the soul has none.
func newHooks(s *soul.Soul) *hooks.Runner {
	if s.Meta.Hooks.Empty() {
		return nil
	}
	return hooks.New(s.Meta.Hooks, filepath.Dir(s.FilePath), s.Meta.Name, skill.SafeEnv())
}

func main() {
	// Subcommand dispatch — runs BEFORE flag.Parse so subcommands own their
	// own flag sets without polluting top-level kinclaw flags. The
	// distinguishing rule: first arg is a known verb that doesn't start with
	// "-". Adding more subcommands later (memory / doctor / forge) follows
	// this same shape — no shared flags to leak between modes.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "probe":
			runProbe(os.Args[2:])
			return
		case "harvest":
			runHarvest(os.Args[2:])
			return
		case "serve":
			runServe(os.Args[2:])
			return
		case "memory":
			runMemory(os.Args[2:])
			return
		}
	}

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `kinclaw — macOS computer-use agent (5 claws + soul + forge + spawn + harvest)

Usage:
  kinclaw -soul PATH [-exec MSG]    Run a soul (REPL or one-shot)
  kinclaw serve [-soul PATH]         Chat UI · 看着 5 爪干活 (split-pane in browser)
  kinclaw memory [list|search|...]   See and curate what the agent remembers
  kinclaw harvest                    Pull external skill libraries; coder forges
                                     KinClaw versions of good ideas; stage for review
  kinclaw harvest --review           Show staged candidates
  kinclaw harvest --accept ID        Copy one staged candidate into ./skills/
  kinclaw probe APP                  Audit one app's AX surface (1-second verdict)
  kinclaw -login                     Claude OAuth (free tier)
  kinclaw -version                   Show version

Top-level flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), `
Subcommand help: kinclaw serve -h  /  kinclaw memory -h  /  kinclaw harvest -h  /  kinclaw probe -h
`)
	}

	soulPath := flag.String("soul", "", "Path to .soul.md file")
	execMsg := flag.String("exec", "", "Execute a single message and exit")
	debug := flag.Bool("debug", false, "Show debug output")
	showVersion := flag.Bool("version", false, "Show version")
	login := flag.Bool("login", false, "Login with Claude OAuth (free tier)")
	cleanup := flag.Bool("cleanup-apps", false, "On exit, quit any apps that weren't running when kinclaw started (leaves your pre-existing workspace alone)")
	flag.StringVar(&permissionModeFlag, "permissions", "", "Override the soul's permissions.mode: auto | ask")
	flag.Parse()
	if permissionModeFlag != "" && permissionModeFlag != "auto" && permissionModeFlag != "ask" {
		fmt.Fprintln(os.Stderr, "Error: -permissions must be auto or ask")
		os.Exit(2)
	}

	if *showVersion {
		fmt.Printf("kinclaw %s\n", version)
		return
	}
	if *login {
		if err := auth.Login(); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Snapshot running apps BEFORE the agent starts opening anything new,
	// so cleanup at exit only quits things kinclaw caused. Capturing here
	// (not in newSession) ensures the snapshot covers the whole process
	// lifetime including any pre-soul-load activity.
	var preexistingApps []string
	if *cleanup {
		apps, err := applifecycle.RunningApps()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: -cleanup-apps couldn't snapshot running apps: %v\n", err)
		} else {
			preexistingApps = apps
		}
		// Best-effort cleanup on Ctrl-C as well as natural exit. The signal
		// handler doesn't try to drain the chat loop — kinclaw was going down
		// anyway, we just want apps closed.
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		go func() {
			<-c
			runCleanup(preexistingApps)
			os.Exit(130) // 128 + SIGINT
		}()
		defer runCleanup(preexistingApps)
	}

	path := findSoulFile(*soulPath)
	if path == "" {
		fmt.Fprintln(os.Stderr, "Error: no soul file found. Use -soul flag or place a .soul.md in ./souls/")
		fmt.Fprintln(os.Stderr, "       Run `kinclaw -h` to see all commands (incl. `kinclaw harvest` for absorbing external skill libraries).")
		os.Exit(1)
	}

	sess, err := newSession(path, *debug, *execMsg != "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if sess.store != nil {
		defer sess.store.Close()
	}
	// MCP servers are subprocesses; they don't die with us unless told to.
	defer sess.closeMCP()

	fmt.Fprintf(os.Stderr, "\033[2m  LocalKin %s\n  Soul:     %s (%s)\n  Brain:    %s / %s\n  Skills:   %d loaded\n  Gate:     %s%s\033[0m\n\n",
		version, sess.soul.Meta.Name, sess.soul.FilePath,
		sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model, len(sess.toolDefs),
		sess.gate.Mode(), gateSuffix(sess))

	if *execMsg != "" {
		os.Exit(runOnce(sess, *execMsg))
	}

	InitHistory(filepath.Join(homeDir(), ".localkin", "readline_history"))
	runREPL(sess)
}

func newSession(soulPath string, debug bool, ephemeral bool) (*session, error) {
	s, err := soul.LoadSoul(soulPath)
	if err != nil {
		return nil, err
	}

	apiKey := s.Meta.Brain.APIKey
	if apiKey == "" {
		switch s.Meta.Brain.Provider {
		case "claude":
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
			if apiKey == "" {
				apiKey = loadOAuthToken()
			}
		case "openai":
			apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	if apiKey == "" && s.Meta.Brain.Provider != "ollama" {
		msg := "Error: API key not set. Set brain.api_key in soul file or $ANTHROPIC_API_KEY / $OPENAI_API_KEY"
		if s.Meta.Brain.Provider == "claude" {
			msg += "\n  Tip: run 'kinclaw -login' to authenticate with Claude (free tier)"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	b := brain.NewBrain(s.Meta.Brain.Provider, s.Meta.Brain.Endpoint,
		s.Meta.Brain.Model, apiKey, s.Meta.Brain.Temperature)

	store, err := memory.OpenMemory(memory.DefaultDBPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: memory unavailable: %v\n", err)
	}

	// Auto-inject durable user facts (memories k-v table) into the
	// soul's system prompt. Without this, pilot has to remember to
	// call `memory action=recall` — and often won't, because there's
	// no signal in the user's question that says "go check memory".
	// Same idea as learned.md: the kernel reads it once at boot and
	// makes it part of the agent's working knowledge.
	if store != nil {
		if mems, err := store.AllMemories(); err == nil && len(mems) > 0 {
			var b strings.Builder
			b.WriteString("\n\n## 用户长期记忆 (across sessions, from memory.db k-v)\n\n")
			b.WriteString("**这些是你跟用户已建立的长期事实,直接据此回答 — 别再 memory.recall 重复查同一件事**\n\n")
			for _, m := range mems {
				b.WriteString("- ")
				b.WriteString(m.Key)
				b.WriteString(": ")
				b.WriteString(m.Value)
				b.WriteString("\n")
			}
			s.SystemPrompt += b.String()
			if debug {
				fmt.Fprintf(os.Stderr, "\033[2m[memory injected %d facts into prompt]\033[0m\n", len(mems))
			}
		}
	}

	reg, mcpClients, mcpResults, mcpConfig := buildRegistry(s, store)

	// session_id used to be "<soul-name>-<pid>" — every kinclaw process
	// got its own bucket, so restarting kinclaw = empty history. That
	// kept concurrent kinclaw runs isolated but threw away cross-
	// process continuity (the actual feature users want: "remember
	// what I said yesterday").
	//
	// Switch to soul-name only. Two consequences:
	//   1. Restarting kinclaw resumes the same conversation thread
	//   2. Concurrent kinclaw runs of the same soul share history —
	//      messages interleave by id (auto-increment). Unusual to run
	//      two pilots at once; if you do, they share a chat log.
	//
	// Old PID-suffixed sessions stay in the DB (still recallable via
	// memory action=recall query="..."). Forward-only: from now on
	// every new message lands in the clean key.
	sessionID := s.Meta.Name
	var history []brain.Message

	// One-shot runs get their own session and start with no history.
	//
	// The shared per-soul key is right for the REPL, where continuing a
	// conversation across restarts is the point. It is wrong for -exec, which
	// is documented as "execute a single message and exit" and is how every
	// programmatic caller drives kinclaw: spawn, harvest's curator, harvest's
	// coder. Those fire the same soul hundreds of times against unrelated
	// inputs, and under one key each run inherited every previous one.
	//
	// Measured on this machine before the fix: the curator session held 812
	// messages and macbench 11,579. Triaging 161 harvest candidates, the
	// curator was judging candidate N with candidates 1..N-1 still in context,
	// and its verdicts bled — it described `songwriting-and-ai-music` as
	// wrapping FindMy.app, and `maps` as wrapping the HuggingFace CLI. Both
	// were verbatim descriptions of earlier candidates.
	//
	// Still written to the store, under a unique key, so a one-shot run
	// remains recallable; it just no longer contaminates the next one.
	if ephemeral {
		sessionID = fmt.Sprintf("%s#once-%d-%d", s.Meta.Name, os.Getpid(), time.Now().UnixNano())
	} else if store != nil {
		// 50 messages is the same volume as before — just spans
		// multiple kinclaw runs now instead of one. Each row's
		// content is capped client-side via LoadHistory's truncation
		// to keep oversized tool outputs from blowing the prompt.
		history = store.LoadHistory(sessionID, 50)
	}

	return &session{
		soul: s, brain: b, registry: reg,
		toolDefs: reg.FilteredToolDefs(effectiveEnable(s)),
		store:    store, id: sessionID, history: history,
		debug: debug, soulPath: soulPath,
		mcpClients: mcpClients, mcpResults: mcpResults, mcpConfig: mcpConfig,
		gate:  newGate(s),
		hooks: newHooks(s),
	}, nil
}

// gateSuffix annotates the boot banner's Gate line.
func gateSuffix(sess *session) string {
	var parts []string
	if sess.gate.Mode() == permission.ModeAsk && len(sess.soul.Meta.Permissions.Ask) == 0 {
		parts = append(parts, "default ask set")
	}
	if sess.hooks != nil {
		parts = append(parts, "hooks on")
	}
	if sess.soul.Meta.Context.Disabled {
		parts = append(parts, "auto-compact off")
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func buildRegistry(s *soul.Soul, store *memory.SQLiteStore) (*skill.Registry, []*mcp.Client, []mcp.LoadResult, *mcp.Config) {
	reg := skill.NewRegistry()
	skillsDir := "./skills"
	if s.Meta.Skills.Dir != "" {
		skillsDir = s.Meta.Skills.Dir
	}
	if s.Meta.Permissions.Shell {
		reg.Register(skill.NewShellSkill(s.Meta.Permissions.ShellTimeout))
		reg.Register(skill.NewForgeSkill(skillsDir, reg))
	}
	reg.Register(skill.NewFileReadSkill())
	reg.Register(skill.NewFileWriteSkill())
	reg.Register(skill.NewFileEditSkill())
	if s.Meta.Permissions.Network {
		reg.Register(skill.NewWebFetchSkill())
		reg.Register(skill.NewWebSearchSkill())
	}
	// Persistent k-v memory across sessions. Only registers if the
	// SQLite store opened cleanly (otherwise the soul falls back to
	// learned.md doctrine + per-session message history). Pilot etc.
	// gates access via permissions.skills.enable: ["memory"].
	if store != nil {
		reg.Register(skill.NewMemorySkill(store))
	}
	// KinBrain — read-mostly view across Jacky's accumulated knowledge
	// (~/.kinbrain/notes/ + localkin/output/ + localkin/knowledge/ +
	// localkin/input/). Shells out to the `kinbrain` CLI; degrades
	// gracefully with a clear "install kinbrain" error if absent.
	// Registered unconditionally — recall is read-only, save writes
	// one Markdown file. Souls opt-in via permissions.skills.enable.
	reg.Register(skill.NewKinBrainSkill())
	// KinBrowser — markdown-native browser. Shells out to the
	// `kinbrowser` CLI (from LocalKinAI/kinbrowser). Returns extracted
	// main content as clean markdown via 3-layer escalation
	// (HTTP+readability → Lightpanda → chromedp). Designed to replace
	// the older web_fetch/web_search/web/browser_session/web_scrape
	// skills — one tool, one contract (URL → markdown). Souls opt-in.
	reg.Register(skill.NewKinBrowserSkill())
	// KinClaw computer-use claws — macOS only; each gated by its own bit.
	// On non-darwin builds these register no-op skills that return a clean
	// "macOS-only" error.
	reg.Register(skill.NewScreenSkill(s.Meta.Permissions.Screen, s.Meta.Skills.OutputDir))
	reg.Register(skill.NewInputSkill(s.Meta.Permissions.Input))
	reg.Register(skill.NewUISkill(s.Meta.Permissions.UI))
	reg.Register(skill.NewRecordSkill(s.Meta.Permissions.Record, s.Meta.Skills.OutputDir))
	// smart_click is a cross-claw composite (OCR + click) — soul must
	// grant BOTH `screen` and `input` for it to work, so we AND the
	// gates here. Either one disabled → skill registered but disabled.
	reg.Register(skill.NewSmartClickSkill(
		s.Meta.Permissions.Screen && s.Meta.Permissions.Input))
	// Sub-agent dispatch — gated by permissions.spawn. Kernel-side
	// recursion limit (env guard) means child agents can't themselves
	// spawn even if they declared the permission, so this is safe to
	// register unconditionally; the skill self-disables when the
	// permission bit is off.
	reg.Register(skill.NewSpawnSkill(s.Meta.Permissions.Spawn, soulDirs()))
	// todo_write — structured plan visible to the desktop shell as a
	// checklist. Mirrors kincode's surface so the same Mac component
	// renders both. Always registered (no soul flag) — soul controls
	// access via permissions.skills.enable: ["todo_write"].
	reg.Register(skill.NewTodoSkill())
	// External skill discovery: scan an ordered list of directories,
	// loading SKILL.md files from each. Same name in two dirs = the
	// LATER dir wins (Registry's `r.skills[name] = s` last-write-wins),
	// so the priority order matters. We go from least-specific to
	// most-specific so user customizations override defaults:
	//
	//   1. Extra dirs from $KINCLAW_SKILL_DIRS env or
	//      ~/.localkin/skill-sources.txt (dev repos, package paths, etc.)
	//   2. ~/.localkin/skills/  (family-shared, persists across reinstalls)
	//   3. ./skills              (cwd-relative; e.g. "cd kinclaw && kinclaw
	//                             serve" picks up the dev repo's skills)
	//
	// Avoids the install-time copy hack: the dev repo's skills/ stays
	// the source of truth, edits are immediately live, no
	// re-install.sh needed when a new skill lands.
	dirs := skillSearchDirs(skillsDir)
	for _, dir := range dirs {
		exts, _ := skill.LoadExternalSkills(dir)
		for _, ext := range exts {
			reg.Register(ext)
		}
		if len(exts) > 0 {
			names := make([]string, len(exts))
			for i, e := range exts {
				names[i] = e.Name()
			}
			sort.Strings(names)
			fmt.Fprintf(os.Stderr, "[skills] %2d from %s: %s\n",
				len(exts), dir, strings.Join(names, ", "))
		}
	}
	// MCP servers from ~/.localkin/mcp.json, registered as ordinary
	// skills. Same format Claude Desktop and kincode use, so an existing
	// config can be copied over and a server's published install snippet
	// pasted in unchanged.
	//
	// Registered here — after external skills, before the allowlist
	// summary — so MCP tools appear in the same "registered vs enabled"
	// accounting as everything else. They are subject to the soul's
	// enable list like any other skill; nothing reaches the model just
	// because a server was configured.
	// Returned rather than closed here: these are subprocesses that must
	// outlive this function and stay up for as long as the registry does.
	// The caller owns them.
	mcpClients, mcpResults, mcpConfig := loadMCPServers(reg)

	// Final list of skills the model will actually see, after the
	// soul's `skills.enable` allowlist filter. Catches the common
	// "I loaded N skills but pilot says it has no <X>" surprise:
	// the skill IS in the registry but isn't in the soul's enable
	// list, so the model never sees it. Listing both is one
	// glance to spot the gap.
	allRegistered := reg.AllNames()
	enabled := effectiveEnable(s)
	if len(enabled) > 0 {
		fmt.Fprintf(os.Stderr,
			"[skills] %d registered total; soul enables %d → exposed to model: %s\n",
			len(allRegistered), len(enabled),
			strings.Join(intersect(allRegistered, enabled), ", "))
		// Surface gaps so misconfigured souls don't silently drop skills.
		gaps := missingFromEnabled(enabled, allRegistered)
		if len(gaps) > 0 {
			fmt.Fprintf(os.Stderr,
				"[skills] ⚠ soul enables %d skill(s) NOT registered: %s\n",
				len(gaps), strings.Join(gaps, ", "))
		}
	} else {
		fmt.Fprintf(os.Stderr,
			"[skills] %d registered, soul has empty enable → ALL exposed to model\n",
			len(allRegistered))
	}
	return reg, mcpClients, mcpResults, mcpConfig
}

// intersect returns the elements of `want` that exist in `have`,
// preserving the order of `want`. Used to print the actual list
// of tools the model will see (registry ∩ soul.skills.enable).
// intersect lists the registered skills the soul's enable list actually
// admits. It resolves wildcards through skill.MatchesAllow so this summary
// matches FilteredToolDefs — reporting the literal string "mcp_github_*" as
// exposed, or omitting the tools it admits, would make the log lie about
// what the model can see.
func intersect(have, want []string) []string {
	out := make([]string, 0, len(want))
	for _, h := range have {
		if skill.MatchesAllow(want, h) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// missingFromEnabled returns names in the soul's enable list that
// have no matching registered skill. Surfaces typos / gone-stale
// references so the user can spot "soul says enable: ['locaton']
// but skill is named 'location'" in 1 second instead of 1 hour.
// missingFromEnabled lists enable entries that match nothing registered —
// usually a typo, or a skill that failed to load. A wildcard counts as
// matched if any registered skill has that prefix; reporting `mcp_github_*`
// as missing whenever that server happens to be down would train the user to
// ignore this warning.
func missingFromEnabled(enabled, registered []string) []string {
	var out []string
	for _, e := range enabled {
		matched := false
		for _, r := range registered {
			if skill.MatchesAllow([]string{e}, r) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, e)
		}
	}
	return out
}

func runREPL(sess *session) {
	// Boot message: auto-execute if configured
	if msg := sess.soul.Meta.Boot.Message; msg != "" {
		fmt.Fprintf(os.Stderr, "\033[2m[boot] %s\033[0m\n", msg)
		handleUserMessage(sess, msg)
	}

	prompt := fmt.Sprintf("\033[1;36m%s>\033[0m ", sess.soul.Meta.Name)
	for {
		input, err := readLine(prompt)
		if err != nil {
			break
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if handleCommand(sess, input) {
				return
			}
			continue
		}
		handleUserMessage(sess, input)
	}
}

func runOnce(sess *session, input string) int {
	handleUserMessage(sess, input)
	if len(sess.history) > 0 {
		last := sess.history[len(sess.history)-1]
		if last.Role == brain.RoleAssistant && last.Content != "" {
			return 0
		}
	}
	return 0
}

// turnContext gives each turn its own interrupt scope. Ctrl-C while the
// brain or a tool is running cancels that turn and returns to the
// prompt; the next turn starts with a fresh context. (One process-wide
// NotifyContext used to be cancelled by the first Ctrl-C and stay
// cancelled, so every later turn failed instantly with "context
// canceled" until the REPL was restarted.)
func turnContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// replSink prints a turn to the terminal.
type replSink struct{ sess *session }

func (r replSink) text(chunk string, thinking bool) {
	if thinking {
		fmt.Fprint(os.Stderr, "\033[2m"+chunk+"\033[0m")
	} else {
		fmt.Print(chunk)
	}
}
func (r replSink) toolCall(id, name string, params map[string]string) {
	if !r.sess.debug {
		fmt.Fprintf(os.Stderr, "\033[2m  ⚙ %s\033[0m\n", permission.Summary(name, params))
	}
}
func (r replSink) toolResult(res skill.ToolResult) {}
func (r replSink) notice(msg string) {
	fmt.Fprintf(os.Stderr, "\033[33m%s\033[0m\n", msg)
}
func (r replSink) usage(u brain.Usage, ctxLen int) {
	if r.sess.debug && ctxLen > 0 {
		fmt.Fprintf(os.Stderr, "\033[2m[usage: %d in / %d out — %d%% of %d context]\033[0m\n",
			u.Total(), u.OutputTokens, u.Total()*100/ctxLen, ctxLen)
	}
}
func (r replSink) compacted(res compact.Result) {
	fmt.Fprintf(os.Stderr, "\033[2m[context compacted: %d messages folded, ~%d → ~%d tokens]\033[0m\n",
		res.Summarized, res.BeforeTokens, res.AfterTokens)
}

func handleUserMessage(sess *session, input string) {
	ctx, cancel := turnContext()
	defer cancel()

	// Pre-LLM grep route: if the soul opts in, try kinthink first.
	// Hit path = ~50-100ms (grep + cerebellum exec), 0 LLM tokens.
	// Miss path falls through to the LLM loop unchanged. This is the
	// operational core of paper #1+#11: most user intents map to a
	// known cerebellum action, and grep finds the match fast.
	if sess.soul != nil && sess.soul.Meta.Cerebellum.GrepRoute {
		if reply, ok := tryGrepRoute(sess, input); ok {
			fmt.Print(reply)
			if !strings.HasSuffix(reply, "\n") {
				fmt.Println()
			}
			sess.appendMessage(brain.Message{Role: brain.RoleUser, Content: input})
			sess.appendMessage(brain.Message{Role: brain.RoleAssistant, Content: reply})
			return
		}
	}

	_, err := runTurnCore(ctx, sess, input, replSink{sess})
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31mError: %v\033[0m\n", err)
		// The abort note keeps the next user turn from reading as
		// "keep going on the prior task" (back-to-back user messages)
		// and gives the human something to react to.
		sess.appendMessage(abortNote(err))
	}
}

// tryGrepRoute runs the kinthink router (a small shell script) against
// the user's prompt. If the router finds a high-confidence match in
// its NL→cerebellum index, it executes the matched cerebellum action
// directly and returns its stdout — without consuming any LLM tokens.
//
// Returns (reply, true) on a successful hit + execution.
// Returns ("", false) if no match, the script is missing, or it errored.
//
// The router's exit code semantics:
//
//	0  → match + executed (KINTHINK_EXEC=1)
//	10 → no-match (caller falls back to LLM)
//	other → script error (treated like no-match for safety)
func tryGrepRoute(sess *session, prompt string) (string, bool) {
	script := sess.soul.Meta.Cerebellum.GrepRouteScript
	if script == "" {
		// Default location: the kinthink skill dir, wherever skills
		// were loaded from — next to the soul's repo, ~/.localkin/
		// skills, $KINCLAW_SKILL_DIRS. Same search order as boot.
		candidates := []string{
			filepath.Join(filepath.Dir(sess.soulPath), "..", "skills", "kinthink", "kinthink.sh"),
		}
		skillsDir := "./skills"
		if sess.soul.Meta.Skills.Dir != "" {
			skillsDir = sess.soul.Meta.Skills.Dir
		}
		for _, d := range skillSearchDirs(skillsDir) {
			candidates = append(candidates, filepath.Join(d, "kinthink", "kinthink.sh"))
		}
		for _, c := range candidates {
			if abs, err := filepath.Abs(c); err == nil {
				if info, err := os.Stat(abs); err == nil && !info.IsDir() {
					script = abs
					break
				}
			}
		}
		if script == "" {
			if sess.debug {
				fmt.Fprintln(os.Stderr, "\033[2m[grep_route: kinthink.sh not found — skipping]\033[0m")
			}
			return "", false
		}
	}

	minScore := sess.soul.Meta.Cerebellum.GrepRouteMinScore
	if minScore <= 0 {
		minScore = 1.5
	}

	cmd := exec.Command(script, prompt)
	cmd.Env = append(os.Environ(),
		"KINTHINK_EXEC=1",
		fmt.Sprintf("KINTHINK_MIN_SCORE=%g", minScore),
	)
	out, err := cmd.CombinedOutput()
	output := string(out)

	if sess.debug {
		fmt.Fprintf(os.Stderr, "\033[2m[grep_route: %s → rc=%v]\033[0m\n", script, err)
	}

	if err != nil {
		// rc != 0 → no match or script error. Fall through to LLM.
		if sess.debug && len(output) > 0 {
			snippet := output
			if len(snippet) > 200 {
				snippet = snippet[:200] + "…"
			}
			fmt.Fprintf(os.Stderr, "\033[2m[grep_route stderr: %s]\033[0m\n", strings.ReplaceAll(snippet, "\n", " "))
		}
		return "", false
	}
	// A match that didn't *finish* is not an answer. The router can
	// score a free-form prompt above threshold, fill zero slots, and
	// run a cerebellum action that fails with "ERR: missing argument"
	// — observed with "用 shell 执行 echo …" landing on
	// terminal-create-file. Cerebellum success is always an "ok:" line;
	// anything else goes to the LLM, which is what would have happened
	// without the router.
	if !isOkLine(output) {
		if sess.debug {
			fmt.Fprintln(os.Stderr, "\033[2m[grep_route: matched but execution did not report ok: — falling through to LLM]\033[0m")
		}
		return "", false
	}
	return output, true
}

// isOkLine reports whether out contains a line starting with "ok:"
// (case-sensitive) AND no line starting with "ERR:" or "FAIL:".
// Cerebellum actions emit "ok: …" as the canonical success line,
// usually as the LAST line — but `osascript` output and other
// diagnostics often precede it on earlier lines, so we scan the
// whole output rather than just the first line.
func isOkLine(out string) bool {
	sawOK := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t, "ERR:"), strings.HasPrefix(t, "FAIL:"):
			return false
		case strings.HasPrefix(t, "ok:"):
			sawOK = true
		}
	}
	return sawOK
}

// ─── Commands ─────────────────────────────────────────────

// handleCommand processes slash commands. Returns true if REPL should exit.
func handleCommand(sess *session, input string) bool {
	parts := strings.Fields(input)
	cmd := parts[0]
	arg := ""
	if len(parts) > 1 {
		arg = strings.Join(parts[1:], " ")
	}

	switch cmd {
	case "/quit", "/exit":
		fmt.Fprintln(os.Stderr, "Goodbye.")
		return true

	case "/help":
		fmt.Print("\033[2m" +
			"/quit          Exit\n" +
			"/skills        List available skills\n" +
			"/clear         Start a new session (old one archived, not deleted)\n" +
			"/info          Show soul, model, and context stats\n" +
			"/usage         Token totals for this process\n" +
			"/compact       Fold older conversation into a summary now\n" +
			"/plan [on|off] Toggle plan mode (read-only tools until you approve)\n" +
			"/permissions   Show the gate: mode, rules, session approvals\n" +
			"/permissions allow <skill>   Approve a skill for the rest of this session\n" +
			"/memory        What the agent remembers (also: kinclaw memory)\n" +
			"/reload        Reload current soul file\n" +
			"/soul          List or switch soul files\n" +
			"\033[0m")

	case "/usage":
		ctxLen := sess.soul.Meta.Brain.ContextLength
		pct := 0
		if ctxLen > 0 {
			pct = sess.lastUsage.Total() * 100 / ctxLen
		}
		fmt.Printf("\033[2m  Last prompt:  %d tokens (%d%% of %d)\n  This process: %d in / %d out\n  History:      %d messages\033[0m\n",
			sess.lastUsage.Total(), pct, ctxLen, sess.totalIn, sess.totalOut, len(sess.history))

	case "/compact":
		ctx, cancel := turnContext()
		res, err := compactNow(ctx, sess)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[31m%v\033[0m\n", err)
			break
		}
		fmt.Printf("\033[2mCompacted: %d messages folded, %d kept, ~%d → ~%d tokens\033[0m\n",
			res.Summarized, res.Kept, res.BeforeTokens, res.AfterTokens)
		fmt.Println(res.Summary)

	case "/plan":
		switch strings.ToLower(arg) {
		case "on":
			sess.gate.SetPlanMode(true)
		case "off":
			sess.gate.SetPlanMode(false)
		default:
			sess.gate.SetPlanMode(!sess.gate.PlanMode())
		}
		if sess.gate.PlanMode() {
			fmt.Println("\033[2mPlan mode ON — the agent can look but not act until you turn it off.\033[0m")
		} else {
			fmt.Println("\033[2mPlan mode OFF.\033[0m")
		}

	case "/permissions":
		if strings.HasPrefix(arg, "allow ") {
			name := strings.TrimSpace(strings.TrimPrefix(arg, "allow "))
			sess.gate.AllowSession(name)
			fmt.Printf("\033[2m%s approved for this session.\033[0m\n", name)
			break
		}
		p := sess.soul.Meta.Permissions
		fmt.Printf("\033[2m  Mode:     %s\n  Plan:     %v\n  Ask:      %s\n  Allow:    %s\n  Session:  %s\033[0m\n",
			sess.gate.Mode(), sess.gate.PlanMode(),
			orNone(strings.Join(p.Ask, ", "), "(default set: shell, file_write, file_edit, forge, mcp_*)"),
			orNone(strings.Join(p.Allow, ", "), "-"),
			orNone(strings.Join(sess.gate.SessionAllowed(), ", "), "-"))

	case "/memory":
		runMemory(nil)

	case "/skills":
		for _, def := range sess.toolDefs {
			var tool struct {
				Function struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"function"`
			}
			json.Unmarshal(def, &tool)
			fmt.Printf("  \033[1m%-15s\033[0m %s\n", tool.Function.Name, truncate(tool.Function.Description, 60))
		}

	case "/clear":
		archived := ""
		if sess.store != nil && len(sess.history) > 0 {
			if key, n, err := sess.store.ArchiveSession(sess.id); err == nil && n > 0 {
				archived = fmt.Sprintf(" (%d messages archived as %q)", n, key)
			}
		}
		sess.history = nil
		sess.lastUsage = brain.Usage{}
		fmt.Printf("\033[2mNew session%s.\033[0m\n", archived)

	case "/info":
		tokens := sess.lastUsage.Total()
		src := "provider-reported"
		if tokens == 0 {
			tokens = brain.EstimateText(sess.soul.SystemPrompt) + brain.EstimateTokens(sess.history)
			src = "estimated"
		}
		fmt.Printf("\033[2m"+
			"  Version:  %s\n"+
			"  Soul:     %s (%s)\n"+
			"  Brain:    %s / %s (%d context)\n"+
			"  Skills:   %d loaded\n"+
			"  Gate:     %s, plan=%v\n"+
			"  History:  %d messages, prompt ~%d tokens (%s)\n"+
			"\033[0m", version, sess.soul.Meta.Name, sess.soul.FilePath,
			sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model, sess.soul.Meta.Brain.ContextLength,
			len(sess.toolDefs), sess.gate.Mode(), sess.gate.PlanMode(),
			len(sess.history), tokens, src)

	case "/reload":
		s, err := soul.LoadSoul(sess.soulPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[31mReload failed: %v\033[0m\n", err)
			break
		}
		sess.soul = s
		sess.id = s.Meta.Name // keep in sync — soul rename would otherwise misroute saves
		sess.swapRegistry(buildRegistry(s, sess.store))
		sess.toolDefs = sess.registry.FilteredToolDefs(effectiveEnable(s))
		sess.gate = newGate(s)
		sess.hooks = newHooks(s)
		fmt.Printf("\033[2mReloaded %s (%d skills)\033[0m\n", s.Meta.Name, len(sess.toolDefs))

	case "/soul":
		if arg == "" {
			listSouls()
		} else {
			path := findSoulByName(arg)
			if path == "" {
				fmt.Fprintf(os.Stderr, "\033[31mSoul not found: %s\033[0m\n", arg)
				break
			}
			s, err := soul.LoadSoul(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\033[31mFailed to load: %v\033[0m\n", err)
				break
			}
			sess.soul = s
			sess.soulPath = path
			sess.id = s.Meta.Name // route saves under the new soul's bucket
			sess.swapRegistry(buildRegistry(s, sess.store))
			sess.toolDefs = sess.registry.FilteredToolDefs(effectiveEnable(s))
			sess.gate = newGate(s)
			sess.hooks = newHooks(s)
			sess.lastUsage = brain.Usage{}
			// Load the new soul's prior history — switching is opting
			// into that soul's accumulated memory, not starting fresh.
			// /reset is the path for "wipe and start over."
			if sess.store != nil {
				sess.history = sess.store.LoadHistory(sess.id, 50)
			} else {
				sess.history = nil
			}
			fmt.Printf("\033[2mSwitched to %s (%s, %d msgs of history)\033[0m\n",
				s.Meta.Name, path, len(sess.history))
		}

	default:
		fmt.Fprintf(os.Stderr, "\033[31mUnknown command: %s (try /help)\033[0m\n", cmd)
	}
	return false
}

// ─── Helpers ──────────────────────────────────────────────

func orNone(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func findSoulFile(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, dir := range soulDirs() {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".soul.md") {
					return filepath.Join(dir, e.Name())
				}
			}
		}
	}
	return ""
}

// soulDirs returns the search path for *.soul.md lookup. Used by:
//   - the spawn skill (resolving "researcher" → researcher.soul.md)
//   - /api/souls listing
//   - /api/soul switching by name
//
// Order (first match wins for name resolution; later dirs visible in
// the listing for completeness):
//
//  1. $KINCLAW_SOUL_DIRS env var (colon-separated). Set by kinclaw-mac's
//     Makefile/Supervisor to point at the repo's souls/ so dev edits
//     are immediately discoverable for spawn — without this, pilot's
//     `spawn(soul="researcher")` falls back to ./souls (cwd-relative,
//     usually empty) → ~/.localkin/souls (we deleted that on purpose
//     since souls now live in the repo) → "soul not found" error.
//  2. ./souls (cwd-relative, for `kinclaw serve` from repo root)
//  3. ~/.localkin/souls (.app users without dev repo)
func soulDirs() []string {
	dirs := []string{}
	if env := os.Getenv("KINCLAW_SOUL_DIRS"); env != "" {
		for _, p := range strings.Split(env, ":") {
			p = strings.TrimSpace(p)
			if p != "" {
				dirs = append(dirs, p)
			}
		}
	}
	dirs = append(dirs, "./souls", filepath.Join(homeDir(), ".localkin", "souls"))
	return dirs
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func homeSkillsDir() string {
	return filepath.Join(homeDir(), ".localkin", "skills")
}

// skillSearchDirs builds the ordered list of directories to scan for
// SKILL.md files at boot. Order is least-to-most specific; later
// dirs override earlier ones (Registry's `r.skills[name] = s`
// last-write-wins).
//
//  1. Extra dirs from $KINCLAW_SKILL_DIRS env var (colon-separated).
//     Set by KinClaw Mac's Makefile to point at the dev repo's
//     skills/, so a user who pulled kinclaw-mac and ran `make run`
//     gets all built-in skills without a copy step.
//  2. Extra dirs from ~/.localkin/skill-sources.txt (one path per
//     line, # comments, blank lines OK). Persistent equivalent of
//     the env var — written by install.sh or set by the user.
//  3. ~/.localkin/skills/ — family-shared user customizations,
//     survives reinstalls.
//  4. The skill dir from the soul's frontmatter (or "./skills"
//     default) — repo-local, so `cd dev-repo && kinclaw serve`
//     picks up source-tree skills automatically.
//
// Missing dirs are skipped silently. Duplicate paths across sources
// are deduped so we don't load the same dir twice.
func skillSearchDirs(soulSkillsDir string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		// Resolve to absolute so two spellings of the same dir
		// (e.g. "./skills" and "/abs/repo/skills") don't both load.
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	// 1. env var
	if env := os.Getenv("KINCLAW_SKILL_DIRS"); env != "" {
		for _, p := range strings.Split(env, ":") {
			add(strings.TrimSpace(p))
		}
	}
	// 2. ~/.localkin/skill-sources.txt
	srcFile := filepath.Join(homeDir(), ".localkin", "skill-sources.txt")
	if data, err := os.ReadFile(srcFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Expand ~/ to home for convenience.
			if strings.HasPrefix(line, "~/") {
				line = filepath.Join(homeDir(), line[2:])
			}
			add(line)
		}
	}
	// 3. user family-shared
	add(homeSkillsDir())
	// 4. soul-specified or cwd-relative
	add(soulSkillsDir)
	return out
}

func loadOAuthToken() string {
	data, err := os.ReadFile(filepath.Join(homeDir(), ".localkin", "auth.json"))
	if err != nil {
		return ""
	}
	var a struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(data, &a) != nil {
		return ""
	}
	return a.AccessToken
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func listSouls() {
	found := false
	for _, dir := range soulDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".soul.md") {
				name := strings.TrimSuffix(e.Name(), ".soul.md")
				fmt.Printf("  %s  \033[2m(%s)\033[0m\n", name, filepath.Join(dir, e.Name()))
				found = true
			}
		}
	}
	if !found {
		fmt.Println("  \033[2mNo soul files found.\033[0m")
	}
}

func findSoulByName(name string) string {
	if strings.HasSuffix(name, ".soul.md") {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	for _, dir := range soulDirs() {
		path := filepath.Join(dir, name+".soul.md")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// runCleanup quits any apps that started during this kinclaw process. Pre-
// existing apps (anything in `preexisting`) are left alone. This is the
// hook for `-cleanup-apps`: snapshot at start, diff at end.
//
// Soft on errors — cleanup is a courtesy, not a contract. If an app refuses
// to quit (unsaved-work modal, AE timeout), we report it and move on.
func runCleanup(preexisting []string) {
	if len(preexisting) == 0 {
		// Either snapshot failed at startup, or cleanup was never armed.
		// Either way, nothing safe to do — bail rather than risk quitting
		// the user's whole desktop.
		return
	}
	quit, failed := applifecycle.QuitNew(preexisting)
	if len(quit) > 0 {
		fmt.Fprintf(os.Stderr, "\033[2m  Cleanup: quit %d app(s) opened during this session: %s\033[0m\n",
			len(quit), strings.Join(quit, ", "))
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "\033[2m  Cleanup: %d app(s) refused to quit (unsaved work or AE timeout): %s\033[0m\n",
			len(failed), strings.Join(failed, ", "))
	}
}

// loadMCPServers connects the MCP servers listed in ~/.localkin/mcp.json and
// registers their tools. Returns the live clients so main can close them.
//
// Every outcome is logged, including the boring ones. An MCP server that
// fails to start is invisible otherwise — the agent simply lacks tools it
// was supposed to have, with nothing to distinguish that from the server
// never having been configured. The log line is the only place that
// difference shows up until there's a settings UI.
func loadMCPServers(reg *skill.Registry) ([]*mcp.Client, []mcp.LoadResult, *mcp.Config) {
	path := mcp.DefaultConfigPath()
	if path == "" {
		return nil, nil, nil
	}

	cfg, err := mcp.LoadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] %v\n", err)
		return nil, nil, nil
	}
	if len(cfg.MCPServers) == 0 {
		return nil, nil, cfg
	}

	clients, results := mcp.Connect(cfg, reg)
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Fprintf(os.Stderr, "[mcp] ✗ %s: %v\n", r.Name, r.Err)
		case r.Disabled:
			fmt.Fprintf(os.Stderr, "[mcp] – %s: disabled\n", r.Name)
		default:
			fmt.Fprintf(os.Stderr, "[mcp] ✓ %s: %d tool(s)\n", r.Name, r.ToolCount)
		}
	}
	return clients, results, cfg
}

// swapRegistry installs a freshly built registry, shutting down the MCP
// servers the old one was using.
//
// Soul reload rebuilds the registry, which reconnects every configured MCP
// server. The previous subprocesses have no other owner, so without closing
// them here each reload would strand a full set — invisible until the user
// notices a pile of node/python processes.
func (s *session) swapRegistry(reg *skill.Registry, clients []*mcp.Client, results []mcp.LoadResult, cfg *mcp.Config) {
	for _, c := range s.mcpClients {
		c.Close()
	}
	s.registry = reg
	s.mcpClients = clients
	s.mcpResults = results
	s.mcpConfig = cfg
}

// closeMCP shuts down all MCP server subprocesses.
func (s *session) closeMCP() {
	for _, c := range s.mcpClients {
		c.Close()
	}
	s.mcpClients = nil
}

// effectiveEnable is a soul's own enable list plus any extras the user added
// from the settings UI.
//
// Read from disk on each call rather than cached: the overlay is edited by a
// separate process (the Mac app), so a value captured at startup would go
// stale the moment the user ticks a box.
func effectiveEnable(s *soul.Soul) []string {
	if s == nil {
		return nil
	}
	extras := skill.LoadExtras(skill.ExtrasPath())
	return skill.EffectiveEnable(s.Meta.Skills.Enable, extras.For(s.Meta.Name))
}
