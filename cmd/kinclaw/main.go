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

	"github.com/LocalKinAI/kinclaw/pkg/applifecycle"
	"github.com/LocalKinAI/kinclaw/pkg/auth"
	"github.com/LocalKinAI/kinclaw/pkg/brain"
	"github.com/LocalKinAI/kinclaw/pkg/memory"
	"github.com/LocalKinAI/kinclaw/pkg/skill"
	"github.com/LocalKinAI/kinclaw/pkg/soul"
)

const (
	version = "1.15.0"
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

	// Pending detached-spawn results, drained at the start of the next
	// turn and prepended to messages as synthetic user messages so the
	// parent (typically pilot) sees what the child returned without
	// the user having to re-narrate it. Guarded by spawnMu — written
	// from the spawn skill's goroutine, read under turnMu by chatHandler.
	spawnMu      sync.Mutex
	pendingSpawn []skill.SpawnResult
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
		}
	}

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `kinclaw — macOS computer-use agent (5 claws + soul + forge + spawn + harvest)

Usage:
  kinclaw -soul PATH [-exec MSG]    Run a soul (REPL or one-shot)
  kinclaw serve [-soul PATH]         Chat UI · 看着 5 爪干活 (split-pane in browser)
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
Subcommand help: kinclaw serve -h  /  kinclaw harvest -h  /  kinclaw probe -h
`)
	}

	soulPath := flag.String("soul", "", "Path to .soul.md file")
	execMsg := flag.String("exec", "", "Execute a single message and exit")
	debug := flag.Bool("debug", false, "Show debug output")
	showVersion := flag.Bool("version", false, "Show version")
	login := flag.Bool("login", false, "Login with Claude OAuth (free tier)")
	cleanup := flag.Bool("cleanup-apps", false, "On exit, quit any apps that weren't running when kinclaw started (leaves your pre-existing workspace alone)")
	flag.Parse()

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

	sess, err := newSession(path, *debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if sess.store != nil {
		defer sess.store.Close()
	}

	fmt.Fprintf(os.Stderr, "\033[2m  LocalKin %s\n  Soul:     %s (%s)\n  Brain:    %s / %s\n  Skills:   %d loaded\033[0m\n\n",
		version, sess.soul.Meta.Name, sess.soul.FilePath,
		sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model, len(sess.toolDefs))

	if *execMsg != "" {
		os.Exit(runOnce(sess, *execMsg))
	}

	InitHistory(filepath.Join(homeDir(), ".localkin", "readline_history"))
	runREPL(sess)
}

func newSession(soulPath string, debug bool) (*session, error) {
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

	reg := buildRegistry(s, store)

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
	if store != nil {
		// 50 messages is the same volume as before — just spans
		// multiple kinclaw runs now instead of one. Each row's
		// content is capped client-side via LoadHistory's truncation
		// to keep oversized tool outputs from blowing the prompt.
		history = store.LoadHistory(sessionID, 50)
	}

	return &session{
		soul: s, brain: b, registry: reg,
		toolDefs: reg.FilteredToolDefs(s.Meta.Skills.Enable),
		store: store, id: sessionID, history: history,
		debug: debug, soulPath: soulPath,
	}, nil
}

func buildRegistry(s *soul.Soul, store *memory.SQLiteStore) *skill.Registry {
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
	// Final list of skills the model will actually see, after the
	// soul's `skills.enable` allowlist filter. Catches the common
	// "I loaded N skills but pilot says it has no <X>" surprise:
	// the skill IS in the registry but isn't in the soul's enable
	// list, so the model never sees it. Listing both is one
	// glance to spot the gap.
	allRegistered := reg.AllNames()
	enabled := s.Meta.Skills.Enable
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
	return reg
}

// intersect returns the elements of `want` that exist in `have`,
// preserving the order of `want`. Used to print the actual list
// of tools the model will see (registry ∩ soul.skills.enable).
func intersect(have, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	out := make([]string, 0, len(want))
	for _, w := range want {
		if set[w] {
			out = append(out, w)
		}
	}
	return out
}

// missingFromEnabled returns names in the soul's enable list that
// have no matching registered skill. Surfaces typos / gone-stale
// references so the user can spot "soul says enable: ['locaton']
// but skill is named 'location'" in 1 second instead of 1 hour.
func missingFromEnabled(enabled, registered []string) []string {
	set := make(map[string]bool, len(registered))
	for _, r := range registered {
		set[r] = true
	}
	var out []string
	for _, e := range enabled {
		if !set[e] {
			out = append(out, e)
		}
	}
	return out
}

func runREPL(sess *session) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Boot message: auto-execute if configured
	if msg := sess.soul.Meta.Boot.Message; msg != "" {
		fmt.Fprintf(os.Stderr, "\033[2m[boot] %s\033[0m\n", msg)
		handleUserMessage(ctx, sess, msg)
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
			if handleCommand(ctx, sess, input) {
				return
			}
			continue
		}
		handleUserMessage(ctx, sess, input)
	}
}

func runOnce(sess *session, input string) int {
	ctx := context.Background()
	handleUserMessage(ctx, sess, input)
	if len(sess.history) > 0 {
		last := sess.history[len(sess.history)-1]
		if last.Role == brain.RoleAssistant && last.Content != "" {
			return 0
		}
	}
	return 0
}

func handleUserMessage(ctx context.Context, sess *session, input string) {
	userMsg := brain.Message{Role: brain.RoleUser, Content: input}
	sess.history = append(sess.history, userMsg)
	if sess.store != nil {
		sess.store.SaveMessage(sess.id, userMsg)
	}

	// Pre-LLM grep route: if the soul opts in, try kinthink first.
	// Hit path = ~50-100ms (grep + cerebellum exec), 0 LLM tokens.
	// Miss path falls through to the LLM chatLoop unchanged. This is
	// the operational core of paper #1+#11: most user intents map to
	// a known cerebellum action, and grep finds the match fast.
	if sess.soul != nil && sess.soul.Meta.Cerebellum.GrepRoute {
		if reply, ok := tryGrepRoute(sess, input); ok {
			fmt.Print(reply)
			if !strings.HasSuffix(reply, "\n") {
				fmt.Println()
			}
			assistantMsg := brain.Message{Role: brain.RoleAssistant, Content: reply}
			sess.history = append(sess.history, assistantMsg)
			if sess.store != nil {
				sess.store.SaveMessage(sess.id, assistantMsg)
			}
			return
		}
	}

	messages := buildMessages(sess.soul, sess.history)
	onChunk := func(chunk string, thinking bool) error {
		if thinking {
			fmt.Fprint(os.Stderr, "\033[2m"+chunk+"\033[0m")
		} else {
			fmt.Print(chunk)
		}
		return nil
	}

	reply, toolHistory, err := chatLoop(ctx, sess, messages, onChunk)
	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[31mError: %v\033[0m\n", err)
		// Persist the partial tool history + a synthesized abort note
		// so the next user turn doesn't see back-to-back user messages
		// (which the brain reads as "keep going on the prior task" and
		// promptly reruns the failed loop). The note also gives the
		// human something to react to: "continue" to resume or rephrase
		// to start fresh.
		for _, msg := range toolHistory {
			if sess.store != nil {
				sess.store.SaveMessage(sess.id, msg)
			}
			sess.history = append(sess.history, msg)
		}
		abortMsg := brain.Message{
			Role:    brain.RoleAssistant,
			Content: fmt.Sprintf("(Turn aborted: %v. Reply 'continue' to resume or rephrase to start fresh.)", err),
		}
		sess.history = append(sess.history, abortMsg)
		if sess.store != nil {
			sess.store.SaveMessage(sess.id, abortMsg)
		}
		return
	}

	for _, msg := range toolHistory {
		if sess.store != nil {
			sess.store.SaveMessage(sess.id, msg)
		}
		sess.history = append(sess.history, msg)
	}
	assistantMsg := brain.Message{Role: brain.RoleAssistant, Content: reply}
	sess.history = append(sess.history, assistantMsg)
	if sess.store != nil {
		sess.store.SaveMessage(sess.id, assistantMsg)
	}

	// Check if forge created new skills
	for _, msg := range toolHistory {
		if msg.Role == brain.RoleAssistant {
			for _, tc := range msg.ToolCalls {
				if tc.Function.Name == "forge" {
					sess.toolDefs = sess.registry.FilteredToolDefs(sess.soul.Meta.Skills.Enable)
					return
				}
			}
		}
	}
}

func chatLoop(ctx context.Context, sess *session, messages []brain.Message, onChunk brain.StreamFunc) (string, []brain.Message, error) {
	var intermediateHistory []brain.Message
	cb := skill.NewCircuitBreaker()

	for round := 0; round < maxToolRounds; round++ {
		result, err := sess.brain.Chat(ctx, messages, sess.toolDefs, onChunk)
		if err != nil {
			return "", nil, err
		}
		if len(result.ToolCalls) == 0 {
			return result.Content, intermediateHistory, nil
		}
		assistantMsg := brain.Message{
			Role: brain.RoleAssistant, Content: result.Content, ToolCalls: result.ToolCalls,
		}
		messages = append(messages, assistantMsg)
		intermediateHistory = append(intermediateHistory, assistantMsg)

		var callInfos []skill.ToolCallInfo
		for _, tc := range result.ToolCalls {
			params, err := tc.ParseArguments()
			if err != nil {
				toolMsg := brain.Message{Role: brain.RoleTool, Content: "Error: " + err.Error(), ToolCallID: tc.ID}
				messages = append(messages, toolMsg)
				intermediateHistory = append(intermediateHistory, toolMsg)
				continue
			}
			if sess.debug {
				fmt.Fprintf(os.Stderr, "\033[2m[tool: %s %v]\033[0m\n", tc.Function.Name, params)
			}
			callInfos = append(callInfos, skill.ToolCallInfo{ID: tc.ID, Name: tc.Function.Name, Params: params})
		}

		results := skill.ExecuteToolCalls(sess.registry, callInfos)

		// Circuit breaker check
		if tripped, tripMsg := cb.Record(results); tripped {
			fmt.Fprintf(os.Stderr, "\033[33m%s\033[0m\n", tripMsg)
			cbMsg := brain.Message{Role: brain.RoleUser, Content: tripMsg}
			messages = append(messages, cbMsg)
			intermediateHistory = append(intermediateHistory, cbMsg)
		}

		for _, r := range results {
			if sess.debug {
				display := r.Output
				if len(display) > 200 {
					display = display[:200] + "..."
				}
				fmt.Fprintf(os.Stderr, "\033[2m[%s -> %s]\033[0m\n", r.Name, strings.ReplaceAll(display, "\n", " "))
			}
			toolMsg := brain.Message{
				Role:       brain.RoleTool,
				Content:    r.Output,
				ToolCallID: r.ToolCallID,
				Images:     r.Images, // brain inlines image bytes for vision-capable models
			}
			messages = append(messages, toolMsg)
			intermediateHistory = append(intermediateHistory, toolMsg)
		}

		// Cerebellum short-circuit: if the soul opts in via
		// `cerebellum.exit_on_ok: true` and the round had a single
		// tool call whose output's first non-empty line starts with
		// "ok:", terminate the agent loop here rather than spending
		// another LLM round trip on a "yes I'm done" confirmation.
		// Designed for benchmarks / single-task one-shot runs where
		// the cerebellum's `ok: …` line is itself a sufficient
		// success signal.
		if sess.soul != nil && sess.soul.Meta.Cerebellum.ExitOnOK &&
			len(results) == 1 && isOkLine(results[0].Output) {
			if sess.debug {
				fmt.Fprintf(os.Stderr, "\033[2m[cerebellum.exit_on_ok: short-circuit after %s]\033[0m\n", results[0].Name)
			}
			return results[0].Output, intermediateHistory, nil
		}
	}
	return "", intermediateHistory, fmt.Errorf("too many tool call rounds (max %d)", maxToolRounds)
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
//   0  → match + executed (KINTHINK_EXEC=1)
//   10 → no-match (caller falls back to LLM)
//   other → script error (treated like no-match for safety)
func tryGrepRoute(sess *session, prompt string) (string, bool) {
	script := sess.soul.Meta.Cerebellum.GrepRouteScript
	if script == "" {
		// Default location: alongside the kinclaw soul or in the
		// shipped skills tree. Look in a few obvious places.
		candidates := []string{
			filepath.Join(filepath.Dir(sess.soulPath), "..", "skills", "kinthink", "kinthink.sh"),
			"/Users/jackysun/Documents/Workspace/kinclaw/skills/kinthink/kinthink.sh",
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
func handleCommand(ctx context.Context, sess *session, input string) bool {
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
			"/quit      Exit\n" +
			"/skills    List available skills\n" +
			"/clear     Clear conversation history\n" +
			"/info      Show soul, model, and token stats\n" +
			"/reload    Reload current soul file\n" +
			"/soul      List or switch soul files\n" +
			"\033[0m")

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
		sess.history = nil
		fmt.Println("\033[2mConversation cleared.\033[0m")

	case "/info":
		tokens := estimateTokens(sess.history)
		fmt.Printf("\033[2m"+
			"  Version:  %s\n"+
			"  Soul:     %s (%s)\n"+
			"  Brain:    %s / %s\n"+
			"  Skills:   %d loaded\n"+
			"  History:  %d messages (~%d tokens)\n"+
			"\033[0m", version, sess.soul.Meta.Name, sess.soul.FilePath,
			sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model,
			len(sess.toolDefs), len(sess.history), tokens)

	case "/reload":
		s, err := soul.LoadSoul(sess.soulPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[31mReload failed: %v\033[0m\n", err)
			break
		}
		sess.soul = s
		sess.id = s.Meta.Name // keep in sync — soul rename would otherwise misroute saves
		sess.registry = buildRegistry(s, sess.store)
		sess.toolDefs = sess.registry.FilteredToolDefs(s.Meta.Skills.Enable)
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
			sess.registry = buildRegistry(s, sess.store)
			sess.toolDefs = sess.registry.FilteredToolDefs(s.Meta.Skills.Enable)
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

func buildMessages(s *soul.Soul, history []brain.Message) []brain.Message {
	messages := []brain.Message{{Role: brain.RoleSystem, Content: s.SystemPrompt}}
	return append(messages, history...)
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
//	1. Extra dirs from $KINCLAW_SKILL_DIRS env var (colon-separated).
//	   Set by KinClaw Mac's Makefile to point at the dev repo's
//	   skills/, so a user who pulled kinclaw-mac and ran `make run`
//	   gets all built-in skills without a copy step.
//	2. Extra dirs from ~/.localkin/skill-sources.txt (one path per
//	   line, # comments, blank lines OK). Persistent equivalent of
//	   the env var — written by install.sh or set by the user.
//	3. ~/.localkin/skills/ — family-shared user customizations,
//	   survives reinstalls.
//	4. The skill dir from the soul's frontmatter (or "./skills"
//	   default) — repo-local, so `cd dev-repo && kinclaw serve`
//	   picks up source-tree skills automatically.
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
	var a struct{ AccessToken string `json:"access_token"` }
	if json.Unmarshal(data, &a) != nil {
		return ""
	}
	return a.AccessToken
}

func estimateTokens(messages []brain.Message) int {
	total := 0
	for _, m := range messages {
		total += len(strings.Fields(m.Content))
	}
	return int(float64(total) * 1.3)
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
