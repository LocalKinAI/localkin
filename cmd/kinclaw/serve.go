// serve.go — `kinclaw serve` subcommand.
//
// Spins up the chat-UI HTTP server (pkg/server) and bridges browser
// chat → kernel turn → SSE events. The turn itself is runTurnCore in
// turn.go — shared with the REPL — driven here through sseSink, which
// turns every text delta, tool call and tool result into a UI event,
// and serverAsker, which routes permission prompts to the connected
// UIs instead of a stderr nobody is watching.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/brain"
	"github.com/LocalKinAI/kinclaw/pkg/compact"
	"github.com/LocalKinAI/kinclaw/pkg/harvest"
	"github.com/LocalKinAI/kinclaw/pkg/mcp"
	"github.com/LocalKinAI/kinclaw/pkg/permission"
	"github.com/LocalKinAI/kinclaw/pkg/routine"
	"github.com/LocalKinAI/kinclaw/pkg/server"
	"github.com/LocalKinAI/kinclaw/pkg/skill"
	"github.com/LocalKinAI/kinclaw/pkg/soul"
)

func runServe(args []string) {
	// When kinclaw runs as a subprocess (typically spawned by KinClaw
	// Mac), watch for our parent dying and exit cleanly instead of
	// being orphaned to launchd. Standalone CLI runs (parent = shell,
	// or already pid 1) get a no-op.
	startOrphanWatch()

	// Preflight TCC permissions (Accessibility + Screen Recording on
	// macOS; no-op on other OSes — Linux/Windows enforce per-call).
	// Logs ✓ / ✗ to stderr so external tooling (`make doctor`) can
	// see whether the 5 claws will work before any actual tool fires.
	preflightPermissions()

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	soulPath := fs.String("soul", "", "Path to .soul.md file (defaults to ./souls/pilot.soul.md)")
	// 8020 not 8019 — localkin (sibling project, "always running") sits
	// on 8019 IPv6 wildcard and the collision is a footgun even though
	// our IPv4 bind technically wins. Pick a neighbour port instead.
	addr := fs.String("addr", "127.0.0.1:8020", "HTTP listen address (host:port)")
	// -port is the common case shortcut. If non-zero, it overrides the
	// port portion of -addr (host stays 127.0.0.1). For LAN binding
	// (-addr 0.0.0.0:9000) use -addr directly.
	port := fs.Int("port", 0, "Port shortcut (overrides -addr port; host stays 127.0.0.1)")
	// -replay PATH plays a recorded session log instead of running a
	// real soul. Useful for showing demos without spending tokens or
	// for reviewing a past run frame-by-frame.
	replay := fs.String("replay", "", "Replay a recorded session JSONL file (read-only mode)")
	// -no-record disables the per-server-run JSONL log. Default is on
	// because recordings are tiny and let you replay later.
	noRecord := fs.Bool("no-record", false, "Disable session JSONL recording")
	debug := fs.Bool("debug", false, "Show kernel debug output on stderr (browser stays clean)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `kinclaw serve — chat UI · 看着 5 爪干活

Usage:
  kinclaw serve [-soul PATH] [-port N | -addr HOST:PORT] [-debug]

Examples:
  kinclaw serve                              # 127.0.0.1:8020 (default)
  kinclaw serve -port 9000                   # 127.0.0.1:9000
  kinclaw serve -addr 0.0.0.0:9000           # bind LAN, accept remote tabs
  kinclaw serve -soul ./souls/marketer.soul.md -port 8888

Opens an HTTP server with a single-page UI:
  · left:  chat box (你说话,kinclaw 流式回)
  · right: live screen flipbook + tool result cards (5 爪每帧都在)

Open the printed URL in a browser. Ctrl-C to quit.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// -port short form wins if set (typed it explicitly), else fall
	// through to -addr's value.
	if *port > 0 {
		if *port < 1 || *port > 65535 {
			fmt.Fprintf(os.Stderr, "Error: -port must be 1..65535 (got %d)\n", *port)
			os.Exit(2)
		}
		*addr = fmt.Sprintf("127.0.0.1:%d", *port)
	}

	// Replay mode short-circuits the entire soul/session pipeline —
	// it just plays back recorded events at original timing.
	if *replay != "" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		runReplayServer(ctx, *addr, *replay)
		return
	}

	path := findSoulFile(*soulPath)
	if path == "" {
		fmt.Fprintln(os.Stderr, "Error: no soul file found. Use -soul flag or place a .soul.md in ./souls/")
		os.Exit(1)
	}

	sess, err := newSession(path, *debug, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if sess.store != nil {
		defer sess.store.Close()
	}

	// /file allow-list. Anywhere a skill might write a screenshot or
	// recording. ~/Library/Caches/kinclaw is the default OutputDir for
	// screen + record; ~/.kinclaw holds product-specific state (serve
	// recordings, harvest artifacts, learned.md); ~/.localkin holds
	// holds shared family runtime (memory.db, souls, audio caches —
	// some of those skills emit /file URLs from there); ./output is
	// where marketing demos and similar land.
	home := homeDir()
	allowed := []string{
		filepath.Join(home, "Library", "Caches", "kinclaw"),
		filepath.Join(home, ".kinclaw"),
		filepath.Join(home, ".localkin"),
		"./output",
	}
	// If the soul declared a custom output_dir, allow that too.
	if od := sess.soul.Meta.Skills.OutputDir; od != "" {
		allowed = append(allowed, expandHome(od))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Serialize turns — only one in flight at a time. The UI prevents
	// a second submit from getting through but defense-in-depth: if
	// somebody POSTs /api/chat directly while a turn is running we
	// reply with a "busy" event rather than racing on sess.history.
	var turnMu sync.Mutex
	var srv *server.Server

	// Track the in-flight turn's cancel func so DELETE /api/chat (the
	// "Esc / interrupt" path) can stop it. Guarded by cancelMu — set
	// when a turn starts, cleared on exit, called from the interrupt
	// handler. nil = no turn in flight, interrupt is a no-op.
	var cancelMu sync.Mutex
	var currentCancel context.CancelFunc

	// currentSess is swappable behind sessMu so the soul switcher can
	// hot-replace it. chatHandler / runTurn deref it on each call so
	// in-flight nothing-happens-here turns will use the OLD session
	// (we hold turnMu through the swap, so this is consistent).
	var sessMu sync.Mutex
	currentSess := sess

	chatHandler := func(_ context.Context, message string) {
		if !turnMu.TryLock() {
			srv.Push(server.Event{Type: "error", Message: "已有任务在跑,等当前回合结束"})
			return
		}
		defer turnMu.Unlock()

		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()

		// Drain detached-spawn results that arrived since the last
		// turn ended. Each becomes a synthetic user message inserted
		// BEFORE the user's actual new message, so the parent agent
		// (typically pilot) sees what the child returned and can refer
		// to it in this turn's reply. Without this drain, pilot would
		// only know "I dispatched researcher" and lose the report.
		s.spawnMu.Lock()
		drained := s.pendingSpawn
		s.pendingSpawn = nil
		s.spawnMu.Unlock()
		for _, res := range drained {
			body := res.Output
			if res.Err != nil {
				body = fmt.Sprintf("ERROR: %v\n\n%s", res.Err, res.Output)
			}
			synthetic := brain.Message{
				Role: brain.RoleUser,
				Content: fmt.Sprintf(
					"[Detached spawn `%s` (job %s) finished after %s]\n\n%s",
					res.Soul, res.JobID, res.Duration.Round(time.Second), body,
				),
			}
			s.history = append(s.history, synthetic)
			if s.store != nil {
				_ = s.store.SaveMessage(s.id, synthetic)
			}
		}

		turnCtx, cancel := context.WithCancel(ctx)
		cancelMu.Lock()
		currentCancel = cancel
		cancelMu.Unlock()
		defer func() {
			cancelMu.Lock()
			currentCancel = nil
			cancelMu.Unlock()
			cancel()
		}()

		runTurn(turnCtx, s, srv, message)
	}

	interruptHandler := func() {
		cancelMu.Lock()
		c := currentCancel
		cancelMu.Unlock()
		if c != nil {
			c()
		}
	}

	soulListHandler := func() []server.SoulInfo {
		sessMu.Lock()
		activePath, _ := filepath.Abs(currentSess.soulPath)
		sessMu.Unlock()
		out := listAvailableSouls()
		for i := range out {
			if out[i].Path == activePath {
				out[i].Active = true
			}
		}
		return out
	}

	soulSwitchHandler := func(newPath string) error {
		// Refuse mid-turn — turn loop holds sess.history; swapping
		// underneath would either lose pending tool results or apply
		// them to the wrong soul's context.
		if !turnMu.TryLock() {
			return fmt.Errorf("turn in progress, cancel first (Esc) then switch")
		}
		defer turnMu.Unlock()

		newSess, err := newSession(newPath, *debug, false)
		if err != nil {
			return err
		}

		sessMu.Lock()
		oldSess := currentSess
		currentSess = newSess
		sessMu.Unlock()

		// Old session's sqlite handle stays valid for its history but
		// we don't need to read from it anymore. Closing it now releases
		// the file lock so a future "switch back" can reopen cleanly.
		if oldSess.store != nil {
			oldSess.store.Close()
		}

		// Repoint hello so any new SSE subscribers (or page reloads)
		// see the right soul up front, and notify currently-connected
		// clients via a soul_switched event.
		srv.SetHello(server.Event{
			Type: "hello",
			Name: newSess.soul.Meta.Name,
			Params: map[string]string{
				"brain":  fmt.Sprintf("%s/%s", newSess.soul.Meta.Brain.Provider, newSess.soul.Meta.Brain.Model),
				"skills": fmt.Sprintf("%d", len(newSess.toolDefs)),
			},
		})
		srv.Push(server.Event{
			Type: "soul_switched",
			Name: newSess.soul.Meta.Name,
			Params: map[string]string{
				"brain":  fmt.Sprintf("%s/%s", newSess.soul.Meta.Brain.Provider, newSess.soul.Meta.Brain.Model),
				"skills": fmt.Sprintf("%d", len(newSess.toolDefs)),
			},
		})
		return nil
	}

	// Brain switching: same soul (Pilot stays Pilot — its prompt,
	// skills, permissions all unchanged) but swap the underlying
	// brain.Brain to a different provider/model. Lets the user flip
	// from kimi-k2.5:cloud → claude-sonnet-4-6 → qwen3:8b live
	// without restarting kinclaw or rewriting souls.
	//
	// API-key resolution: caller's req.APIKey wins when set; else
	// fall back to the soul's brain.api_key; else env var. ollama
	// needs no key. Mirrors newSession's resolution order.
	brainSwitchHandler := func(req server.BrainSwitchRequest) error {
		if !turnMu.TryLock() {
			return fmt.Errorf("turn in progress, cancel first (Esc) then switch")
		}
		defer turnMu.Unlock()

		sessMu.Lock()
		curSoul := currentSess.soul
		sessMu.Unlock()

		apiKey := req.APIKey
		if apiKey == "" {
			apiKey = curSoul.Meta.Brain.APIKey
		}
		if apiKey == "" {
			switch req.Provider {
			case "claude":
				apiKey = os.Getenv("ANTHROPIC_API_KEY")
				if apiKey == "" {
					apiKey = loadOAuthToken()
				}
			case "openai":
				apiKey = os.Getenv("OPENAI_API_KEY")
			}
		}
		if apiKey == "" && req.Provider != "ollama" {
			return fmt.Errorf("API key required for %s; set in request body, soul, or env", req.Provider)
		}

		// Default the endpoint based on provider — same logic
		// soul.LoadSoul uses at boot. Without this, picking
		// "ollama/<model>" from the Mac dropdown sends an empty
		// endpoint, brain.NewBrain falls through to OpenAI's
		// default api.openai.com, the request hits OpenAI without
		// a key, and the user sees a confusing 401 on a brain
		// they expected to be local. Same source-of-truth
		// (soul.DefaultEndpointFor) used in both paths so they
		// can't drift.
		endpoint := req.Endpoint
		if endpoint == "" {
			endpoint = soul.DefaultEndpointFor(req.Provider)
		}

		newBrain := brain.NewBrain(req.Provider, endpoint,
			req.Model, apiKey, curSoul.Meta.Brain.Temperature)

		sessMu.Lock()
		currentSess.brain = newBrain
		// Mutate the soul's brain meta so /api/souls reflects truth
		// (the soul object is shared, so this changes what Active
		// rows display in the souls list — not persisted to disk).
		currentSess.soul.Meta.Brain.Provider = req.Provider
		currentSess.soul.Meta.Brain.Model = req.Model
		currentSess.soul.Meta.Brain.Endpoint = endpoint
		newProv := req.Provider
		newModel := req.Model
		newSkillCount := len(currentSess.toolDefs)
		sessMu.Unlock()

		// Repoint hello so reconnects see the new brain. Push event
		// so the live UI updates its dropdown without polling.
		srv.SetHello(server.Event{
			Type: "hello",
			Name: curSoul.Meta.Name,
			Params: map[string]string{
				"brain":  fmt.Sprintf("%s/%s", newProv, newModel),
				"skills": fmt.Sprintf("%d", newSkillCount),
			},
		})
		srv.Push(server.Event{
			Type: "brain_switched",
			Params: map[string]string{
				"brain": fmt.Sprintf("%s/%s", newProv, newModel),
			},
		})
		return nil
	}

	// Session reset: start a new conversation on the running session
	// without touching soul/brain/skills/permissions. The Mac UI's
	// "New session" button hits this so a stuck mid-task tool-call
	// loop from a previous conversation can't bleed into the next
	// "你好,你都能做什么" by having the model continue the old turn's
	// plan. The old rows are archived under "<soul>@<timestamp>", not
	// deleted — still searchable, listed by `kinclaw memory sessions`.
	// memories table (durable key/value) is intentionally NOT cleared —
	// those are long-lived facts about the user that survive sessions.
	sessionResetHandler := func() error {
		if !turnMu.TryLock() {
			return fmt.Errorf("turn in progress, cancel first (Esc) then reset")
		}
		defer turnMu.Unlock()

		sessMu.Lock()
		s := currentSess
		// Drop in-memory history. Reassign rather than truncate so
		// any stale slice header captured elsewhere can't observe
		// length changes mid-flight (turnMu held = no readers in
		// flight, but defense-in-depth).
		s.history = nil
		s.lastUsage = brain.Usage{}
		sessMu.Unlock()

		if s.store != nil {
			if _, _, err := s.store.ArchiveSession(s.id); err != nil {
				return fmt.Errorf("archive session: %w", err)
			}
			// Also drop transient working memory ("_" prefix).
			// Without this, AllMemories() at next-turn prompt-build
			// time still re-injects every `_finding_<n>` from the
			// previous task, and "你好" wakes a researcher that
			// thinks it's still mid-apartment-hunt. Durable user
			// facts (bare keys) are preserved by design.
			if err := s.store.ClearTransientMemories(); err != nil {
				// Don't fail the whole reset — messages were already
				// cleared, partial success is the right call. Log
				// and move on.
				fmt.Fprintf(os.Stderr,
					"[session-reset] transient memory clear failed: %v\n", err)
			}
		}

		srv.Push(server.Event{
			Type: "session_reset",
			Name: s.soul.Meta.Name,
		})
		return nil
	}

	srv = server.New(*addr, allowed, chatHandler)
	srv.SetInterruptHandler(interruptHandler)
	srv.SetSoulHandlers(soulListHandler, soulSwitchHandler)
	srv.SetBrainSwitchHandler(brainSwitchHandler)
	srv.SetSessionResetHandler(sessionResetHandler)

	// Permission prompts and ask_user questions go to the UI, not the
	// (invisible) stderr of a subprocess. Installed here because they
	// need the server, which doesn't exist when newSession runs.
	currentSess.gate.SetAsker(serverAsker{srv})
	currentSess.questioner = serverQuestioner{srv}
	srv.AllowDir(currentSess.workspace)

	// POST /api/workspace — the folder picker.
	srv.SetWorkspaceHandler(func(path string) (string, error) {
		sessMu.Lock()
		defer sessMu.Unlock()
		return currentSess.setWorkspace(path)
	})

	// /api/search/* — the search-health indicator. Status is the record
	// of the last real web_search (no traffic); probe is one restricted
	// search on demand, never on a timer.
	srv.SetSearchHandlers(&server.SearchHandlers{
		Status: func() any {
			return map[string]any{
				"endpoint": skill.SearchEndpoint(),
				"last":     skill.LastSearchStatus(),
			}
		},
		Probe: func() any { return skill.ProbeSearXNG(skill.SearchEndpoint()) },
	})

	// /api/routines — scheduled one-shot runs, installed as LaunchAgents
	// with this helper's discovery env so they see the same skills.
	rm := routine.DefaultManager()
	routineEnv := func() routine.RunEnv {
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		return routineRunEnv(s.soulPath, s.workspace)
	}
	srv.SetRoutineHandlers(&server.RoutineHandlers{
		List: func() ([]server.RoutineInfo, error) {
			list, err := rm.List()
			if err != nil {
				return nil, err
			}
			out := make([]server.RoutineInfo, 0, len(list))
			for _, r := range list {
				out = append(out, routineInfo(rm, r))
			}
			return out, nil
		},
		Add: func(name, prompt, schedule, soulFile string) (server.RoutineInfo, error) {
			env := routineEnv()
			if soulFile == "" {
				soulFile = env.SoulPath
			}
			r, err := rm.Add(routine.Routine{Name: name, Prompt: prompt, Soul: mustAbs(soulFile), Schedule: routine.Schedule{Raw: schedule}}, env)
			info := server.RoutineInfo{}
			if r.ID != "" {
				info = routineInfo(rm, r)
			}
			return info, err
		},
		Remove: rm.Remove,
		Run: func(id string) error {
			r, ok := rm.Get(id)
			if !ok {
				return fmt.Errorf("no routine %q", id)
			}
			return runRoutineNow(rm, r, routineEnv())
		},
		SetEnabled: func(id string, on bool) error { return rm.SetEnabled(id, on, routineEnv()) },
		Log:        func(id string) (string, error) { return routineLogTail(rm, id) },
	})

	// GET /api/state — header data for a UI that just connected.
	srv.SetStateHandler(func() server.State {
		sessMu.Lock()
		defer sessMu.Unlock()
		s := currentSess
		return server.State{
			Soul:           s.soul.Meta.Name,
			Brain:          fmt.Sprintf("%s/%s", s.soul.Meta.Brain.Provider, s.soul.Meta.Brain.Model),
			Skills:         len(s.toolDefs),
			PermissionMode: string(s.gate.Mode()),
			PlanMode:       s.gate.PlanMode(),
			SessionAllowed: s.gate.SessionAllowed(),
			Messages:       len(s.history),
			InputTokens:    s.lastUsage.Total(),
			ContextLength:  s.soul.Meta.Brain.ContextLength,
			TotalInput:     s.totalIn,
			TotalOutput:    s.totalOut,
			Workspace:      s.workspace,
			Deferred:       s.pendingDeferred(),
			Loaded:         s.loadedSkills(),
		}
	})

	// POST /api/plan_mode — flip the read-only gate. Allowed mid-turn:
	// it takes effect at the next tool call, which is exactly when a
	// user who just saw the agent head somewhere unexpected wants it.
	srv.SetPlanModeHandler(func(on bool) bool {
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		s.gate.SetPlanMode(on)
		return s.gate.PlanMode()
	})

	// POST /api/compact — fold now. Refused mid-turn: the turn loop owns
	// sess.history while it runs.
	srv.SetCompactHandler(func() (string, error) {
		if !turnMu.TryLock() {
			return "", fmt.Errorf("turn in progress, cancel first (Esc) then compact")
		}
		defer turnMu.Unlock()
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		res, err := compactNow(ctx, s)
		if err != nil {
			return "", err
		}
		sseSink{srv}.compacted(res)
		return fmt.Sprintf("%d messages folded, %d kept (~%d → ~%d tokens)",
			res.Summarized, res.Kept, res.BeforeTokens, res.AfterTokens), nil
	})

	// GET /api/mcp — what the settings UI renders.
	//
	// Reads through sessMu because a soul switch rebuilds the registry and
	// reconnects every server, so this list changes under us.
	// GET /api/harvest — skill sources + what they've staged.
	//
	// Read from disk on each request rather than cached at startup: harvest
	// runs from a LaunchAgent at 03:00, so the files change while this
	// process is running and a snapshot would go stale by morning.
	srv.SetHarvestStatusHandler(harvestStatus)

	// POST /api/harvest/accept — forge a staged candidate into skills/.
	//
	// The one write operation in the settings surface, and it writes *code*
	// into the user's repo via the coder agent. Kept behind an explicit call
	// rather than folded into a status endpoint so it can never fire as a side
	// effect of the UI merely displaying something.
	srv.SetAcceptHandler(acceptStagedCandidate)

	// POST /api/skills/extras — grant the active soul an extra skill without
	// touching its file.
	srv.SetSkillExtrasHandler(func(pattern string, enable bool) error {
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		if s == nil {
			return fmt.Errorf("no active session")
		}
		if err := toggleSkillExtra(s.soul.Meta.Name, pattern, enable); err != nil {
			return err
		}
		// Recompute immediately: the whole point of an overlay is that it
		// takes effect without a restart, unlike editing the soul.
		sessMu.Lock()
		s.computeDeferred()
		s.refreshToolDefs()
		sessMu.Unlock()
		return nil
	})

	// GET /api/skills — what the *active* soul can actually use.
	srv.SetSkillStatusHandler(func() server.SkillStatus {
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		return skillStatusFor(s)
	})

	srv.SetMCPStatusHandler(func() []server.MCPServerStatus {
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		return mcpStatusFor(s)
	})

	// Detached-spawn delivery: when a child kinclaw subprocess finishes
	// in the background (pilot dispatched it with `spawn(...)` while
	// the user kept chatting), we get the result here. Two deliveries:
	//   1. SSE event `spawn_done` so the UI can render the result as
	//      a separate message bubble (lobster icon for the child soul).
	//   2. Append to the active session's `pendingSpawn` queue so the
	//      NEXT turn drains it as a synthetic user message — this lets
	//      pilot reference the child's report ("you said researcher's
	//      finding was…") without the user having to copy-paste.
	// The callback is invoked from the spawn skill's goroutine, so
	// pendingSpawn writes go through s.spawnMu (zero contention with
	// turn-loop reads, which happen under turnMu at chatHandler entry).
	spawnResultCallback := func(res skill.SpawnResult) {
		// Body for the SSE event + history message. Includes timing
		// + first-line summary so a 50-line report still gives a
		// readable preview in the chat surface.
		summary := res.Output
		if res.Err != nil {
			summary = fmt.Sprintf("ERROR: %v\n\n%s", res.Err, res.Output)
		}

		srv.Push(server.Event{
			Type: "spawn_done",
			Name: res.Soul,
			ID:   res.JobID,
			Params: map[string]string{
				"duration_s": fmt.Sprintf("%.0f", res.Duration.Seconds()),
				"prompt":     res.Prompt,
			},
			Output: summary,
		})

		// Queue for next-turn injection. Take the active session ref
		// under sessMu (might have changed if user soul-switched while
		// child was running — in that case we still inject into the
		// CURRENT session, accepting that minor mismatch over losing
		// the result entirely).
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		s.spawnMu.Lock()
		s.pendingSpawn = append(s.pendingSpawn, res)
		s.spawnMu.Unlock()
	}
	if currentSess.registry != nil {
		currentSess.registry.SetSpawnResultCallback(spawnResultCallback)
	}
	// Re-register on soul switch (newSession rebuilds registry and gate).
	prevSoulSwitch := soulSwitchHandler
	soulSwitchHandler = func(p string) error {
		if err := prevSoulSwitch(p); err != nil {
			return err
		}
		sessMu.Lock()
		s := currentSess
		sessMu.Unlock()
		if s.registry != nil {
			s.registry.SetSpawnResultCallback(spawnResultCallback)
		}
		s.gate.SetAsker(serverAsker{srv})
		s.questioner = serverQuestioner{srv}
		return nil
	}
	srv.SetSoulHandlers(soulListHandler, soulSwitchHandler)
	// Wire the live-screen feed for KinClaw Mac's Cowork mode (which
	// renders /api/screen/current.jpg inline above the chat). The
	// server caches the result for 800ms so faster polling buys
	// nothing — we just shell out to screencapture(1) per uncached
	// hit. macOS prompts for Screen Recording permission on first
	// invocation; until granted, captures return blank but the
	// pipeline still works.
	srv.SetLiveScreenCapture(captureScreenJPEG)
	srv.SetLiveScreenInfo(activeAppName)

	// Per-server-run JSONL recording. ~/.kinclaw/serve-sessions/
	// <YYYYMMDD-HHMMSS>.jsonl, one line per Event. `kinclaw serve
	// --replay <file>` plays it back. Disabled with -no-record.
	var recordPath string
	if !*noRecord {
		rec, p, err := openSessionRecorder()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: recording disabled (%v)\n", err)
		} else {
			recordPath = p
			srv.SetEventLogger(rec.log)
			defer rec.close()
		}
	}

	helloEv := server.Event{
		Type: "hello",
		Name: sess.soul.Meta.Name,
		Params: map[string]string{
			"brain":  fmt.Sprintf("%s/%s", sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model),
			"skills": fmt.Sprintf("%d", len(sess.toolDefs)),
			"mode":   string(sess.gate.Mode()),
		},
	}
	srv.SetHello(helloEv)
	// Also push hello through Push so the recorder captures it as
	// the first event of the file — replay then starts with the right
	// soul/brain in the header instead of "— soul loading —".
	if recordPath != "" {
		srv.Push(helloEv)
	}

	fmt.Fprintf(os.Stderr,
		"\033[2m  LocalKin %s\n  Soul:     %s (%s)\n  Brain:    %s / %s\n  Skills:   %d loaded\033[0m\n",
		version, sess.soul.Meta.Name, sess.soul.FilePath,
		sess.soul.Meta.Brain.Provider, sess.soul.Meta.Brain.Model, len(sess.toolDefs))
	if recordPath != "" {
		fmt.Fprintf(os.Stderr, "  Record:   %s\n", recordPath)
	}
	fmt.Fprintf(os.Stderr, "  Open:     \033[1mhttp://%s/\033[0m\n", browserAddr(*addr))
	fmt.Fprintf(os.Stderr, "  Float:    \033[1mhttp://%s/?compact\033[0m  (chat-only,小窗贴角)\n", browserAddr(*addr))
	fmt.Fprintf(os.Stderr, "\033[2m  Tip: float 模式做 always-on-top:\n"+
		"    chrome --app=http://%s/?compact     # standalone window 模式\n"+
		"    或 Rectangle / Hammerspoon 给窗口绑 \"always on top\" 快捷键\033[0m\n\n",
		browserAddr(*addr))

	if err := srv.ListenAndServe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

// sseSink turns a turn into SSE events: every text chunk becomes
// text_delta, every dispatched call tool_call, every result tool_result
// with image paths resolved to /file URLs the browser can fetch.
type sseSink struct{ srv *server.Server }

func (s sseSink) text(chunk string, thinking bool) {
	s.srv.Push(server.Event{Type: "text_delta", Text: chunk, Thinking: thinking})
}
func (s sseSink) toolCall(id, name string, params map[string]string) {
	s.srv.Push(server.Event{Type: "tool_call", ID: id, Name: name, Params: params})
}
func (s sseSink) toolResult(r skill.ToolResult) {
	urls := make([]string, 0, len(r.Images))
	for _, p := range r.Images {
		if u := s.srv.FileURL(p); u != "" {
			urls = append(urls, u)
		}
	}
	// Pull video / image paths out of structured `path: ...` lines
	// (record stop uses this shape; screen capture uses image://
	// markers which already populated r.Images).
	for _, p := range extractStructuredPaths(r.Output) {
		if u := s.srv.FileURL(p); u != "" {
			urls = append(urls, u)
		}
	}
	s.srv.Push(server.Event{
		Type: "tool_result", ID: r.ToolCallID, Name: r.Name,
		Output: r.Output, Images: r.Images, URLs: urls,
	})
}

// notice is its own event type on purpose. Circuit-breaker trips used
// to go out as `error`, and every client treats `error` as "the turn
// is over" — KinClaw Mac stops reading the stream on it — so a
// mid-turn warning silently truncated the rest of the turn in the UI.
func (s sseSink) notice(msg string) {
	s.srv.Push(server.Event{Type: "notice", Message: msg})
}
func (s sseSink) usage(u brain.Usage, ctxLen int) {
	s.srv.Push(server.Event{Type: "usage", InputTokens: u.Total(), OutputTokens: u.OutputTokens, ContextLength: ctxLen})
}
func (s sseSink) compacted(res compact.Result) {
	s.srv.Push(server.Event{
		Type: "compacted", BeforeTokens: res.BeforeTokens, AfterTokens: res.AfterTokens,
		Message: fmt.Sprintf("%d messages folded into a summary (~%d → ~%d tokens)", res.Summarized, res.BeforeTokens, res.AfterTokens),
		Output:  res.Summary,
	})
}

// serverAsker routes permission requests to the connected UIs and
// waits for POST /api/permission. The turn's ctx cancels the wait when
// the user hits Stop.
type serverAsker struct{ srv *server.Server }

func (a serverAsker) Ask(ctx context.Context, req permission.Request) (permission.Answer, error) {
	decision, err := a.srv.AskPermission(ctx, server.Event{
		ID: req.ID, Name: req.Skill, Params: req.Params, Summary: req.Summary, Reason: req.Reason,
	})
	if err != nil {
		return permission.Deny, err
	}
	switch decision {
	case "allow":
		return permission.AllowOnce, nil
	case "allow_session":
		return permission.AllowSession, nil
	case "allow_always":
		return permission.AllowAlways, nil
	default:
		return permission.Deny, nil
	}
}

// serverQuestioner routes ask_user questions to the connected UIs and
// waits for POST /api/answer.
type serverQuestioner struct{ srv *server.Server }

func (q serverQuestioner) Ask(ctx context.Context, question skill.Question) (string, error) {
	return q.srv.AskQuestion(ctx, server.Event{ID: question.ID, Message: question.Text, Options: question.Options})
}

// runTurn drives one turn for the UI and always closes with turn_done.
func runTurn(ctx context.Context, sess *session, srv *server.Server, input string) {
	_, err := runTurnCore(ctx, sess, input, sseSink{srv})
	if err != nil {
		srv.Push(server.Event{Type: "error", Message: err.Error()})
		sess.appendMessage(abortNote(err))
	}
	srv.Push(server.Event{Type: "turn_done"})
}

// extractStructuredPaths picks up `path: /abs/foo.mp4` lines from
// structured tool output (record stop / screen capture's text body).
// Returns absolute paths whose extension we recognize as renderable.
var pathRe = regexp.MustCompile(`(?m)^\s*path:\s*(/[^\s]+\.(?:mp4|mov|m4v|png|jpe?g|webp|gif))\s*$`)

func extractStructuredPaths(out string) []string {
	if out == "" {
		return nil
	}
	matches := pathRe.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return nil
	}
	out2 := make([]string, 0, len(matches))
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) >= 2 && !seen[m[1]] {
			seen[m[1]] = true
			out2 = append(out2, m[1])
		}
	}
	return out2
}

// listAvailableSouls scans the standard soul dirs (./souls/ +
// ~/.localkin/souls/, per soulDirs() in main.go) for *.soul.md files
// and returns their meta. Skips files that fail to parse — broken
// souls just don't show up in the dropdown.
func listAvailableSouls() []server.SoulInfo {
	var out []server.SoulInfo
	seen := map[string]bool{}
	for _, dir := range soulDirs() {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.soul.md"))
		for _, f := range matches {
			abs, err := filepath.Abs(f)
			if err != nil || seen[abs] {
				continue
			}
			seen[abs] = true
			s, err := soul.LoadSoul(f)
			if err != nil {
				continue
			}
			out = append(out, server.SoulInfo{
				Path:  abs,
				Name:  s.Meta.Name,
				Brain: fmt.Sprintf("%s/%s", s.Meta.Brain.Provider, s.Meta.Brain.Model),
			})
		}
	}
	return out
}

// browserAddr converts a listen address into something a human can
// click. 0.0.0.0:8019 → 127.0.0.1:8019 (browsers won't navigate to
// 0.0.0.0). Bare ports like ":8019" get the same treatment.
func browserAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// startOrphanWatch fires off a goroutine that exits the process when
// the original parent dies. macOS doesn't auto-SIGTERM children when
// a parent goes away — they get reparented to launchd (pid 1) and
// keep running, leaking subprocess + port until manually killed.
//
// We poll os.Getppid() every 2s. If it changes from the value we saw
// at startup, the parent died and we got reparented; clean exit.
//
// Skipped when the recorded parent is pid 0 or 1 — that means we
// were either started by launchd directly (no orphan risk) or someone
// already reparented us before we got here, in which case there's no
// "original parent" to watch.
func startOrphanWatch() {
	origParent := os.Getppid()
	if origParent <= 1 {
		return
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			if os.Getppid() != origParent {
				fmt.Fprintln(os.Stderr,
					"[orphan-watch] parent died, exiting")
				os.Exit(0)
			}
		}
	}()
}

// captureScreenJPEG + activeAppName are platform-specific. macOS lives
// in serve_livefeed_darwin.go (screencapture(1) + osascript). Linux/
// Windows stubs live in serve_livefeed_other.go and return empty —
// the UI hides the feed gracefully when bytes are 0.

// expandHome resolves a leading ~ to the user's home dir. We accept
// "~/foo" and "~user/foo" forms; bare "~" expands to home.
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if p == "~" {
		return homeDir()
	}
	if len(p) > 1 && p[1] == '/' {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

// ─── session recording ────────────────────────────────────────
// recordEntry is one line of the JSONL log. TS is wall-clock ms so
// replay can reproduce the original timing (capped to keep idle gaps
// from making playback boring).
type recordEntry struct {
	TS    int64        `json:"ts_ms"`
	Event server.Event `json:"event"`
}

type sessionRecorder struct {
	f  *os.File
	mu sync.Mutex
}

func (r *sessionRecorder) log(ev server.Event) {
	if r == nil || r.f == nil {
		return
	}
	entry := recordEntry{TS: time.Now().UnixMilli(), Event: ev}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	r.mu.Lock()
	_, _ = r.f.Write(data)
	_, _ = r.f.Write([]byte("\n"))
	r.mu.Unlock()
}

func (r *sessionRecorder) close() {
	if r == nil || r.f == nil {
		return
	}
	r.mu.Lock()
	_ = r.f.Close()
	r.mu.Unlock()
}

// openSessionRecorder creates ~/.kinclaw/serve-sessions/<ts>.jsonl
// and returns the recorder + its path. Caller installs r.log as the
// EventLogger and defers r.close() before exit.
//
// Pre-2026-05-03 this was ~/.localkin/serve-sessions/ — moved to
// ~/.kinclaw/ since serve recordings are kinclaw-specific output,
// not LocalKin family runtime data.
func openSessionRecorder() (*sessionRecorder, string, error) {
	dir := filepath.Join(homeDir(), ".kinclaw", "serve-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	name := time.Now().Format("20060102-150405") + ".jsonl"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}
	return &sessionRecorder{f: f}, path, nil
}

// ─── replay mode ──────────────────────────────────────────────
// runReplayServer plays a recorded JSONL session log into a fresh
// server. chat is rejected (read-only mode), Esc cancels playback,
// soul switcher stays available but with the live-mode handler
// disabled. Caller passes a ctx that gets canceled on SIGINT.
func runReplayServer(ctx context.Context, addr, replayPath string) {
	abs, err := filepath.Abs(replayPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: bad replay path: %v\n", err)
		os.Exit(1)
	}
	f, err := os.Open(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: open replay file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// Read all entries up front so we can show event count + check
	// for malformed lines without being mid-playback when something
	// breaks. Recordings are tiny (~few hundred KB even for long
	// turns) so this is fine.
	var entries []recordEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB max line
	for scanner.Scan() {
		var e recordEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "Error: replay file empty or no valid events: %s\n", abs)
		os.Exit(1)
	}

	// Allowed dirs for /file in replay — same set as live mode plus
	// wherever the original recording references. We can't introspect
	// every URL so just allow the standard locations; URLs outside
	// will 404 in the browser (graceful).
	home := homeDir()
	allowed := []string{
		filepath.Join(home, "Library", "Caches", "kinclaw"),
		filepath.Join(home, ".kinclaw"),
		filepath.Join(home, ".localkin"),
		"./output",
	}

	// Stub chat handler — reject with a friendly message.
	chatStub := func(_ context.Context, _ string) {
		// Server.handleChatPost echoes user_message before we get here,
		// so error here completes the visual.
	}

	// Replay control: a single cancelable context for the playback
	// goroutine. Esc / DELETE /api/chat stops playback.
	playCtx, playCancel := context.WithCancel(ctx)
	defer playCancel()

	srv := server.New(addr, allowed, chatStub)
	srv.SetInterruptHandler(func() { playCancel() })
	srv.SetHello(server.Event{
		Type: "hello",
		Name: "REPLAY",
		Params: map[string]string{
			"brain":  "playback",
			"replay": filepath.Base(abs),
		},
	})

	// Override chatStub: in replay, any POST should bounce back as
	// an error so the UI shows "replay 模式,无法对话".
	chatRejectHandler := func(_ context.Context, _ string) {
		srv.Push(server.Event{Type: "error", Message: "replay 模式 · 无法发新消息"})
	}
	// Re-wire by creating a new server with the proper handler.
	// (Cleaner than mutating srv; chatStub above was just a placeholder
	// because Server requires non-nil handler at construction.)
	srv = server.New(addr, allowed, chatRejectHandler)
	srv.SetInterruptHandler(func() { playCancel() })
	srv.SetHello(server.Event{
		Type: "hello",
		Name: "REPLAY · " + filepath.Base(abs),
		Params: map[string]string{
			"brain":  fmt.Sprintf("%d events", len(entries)),
			"replay": "1",
		},
	})

	// Playback goroutine. Sleep deltas between events, capped at 2s
	// so a long brain pause doesn't make the user wait for nothing.
	// We block on first-subscriber so events recorded before the
	// browser opens (e.g. the initial hello) don't fire into the void.
	go func() {
		if err := srv.WaitForFirstSubscriber(playCtx); err != nil {
			return
		}
		// Tiny grace so the browser finishes initial render before
		// the first event lands.
		select {
		case <-time.After(200 * time.Millisecond):
		case <-playCtx.Done():
			return
		}
		var prevTS int64
		for i, e := range entries {
			if playCtx.Err() != nil {
				srv.Push(server.Event{Type: "error", Message: "replay 已取消"})
				return
			}
			// Skip recorded hello — replay mode has its own ("REPLAY ·
			// <file>") and we don't want to overwrite it with the
			// original soul name. Same for soul_switched-during-replay
			// would be confusing; we keep that one because it might be
			// part of the meaningful narrative being replayed.
			if e.Event.Type == "hello" {
				prevTS = e.TS
				continue
			}
			if i > 0 {
				delta := e.TS - prevTS
				if delta < 0 {
					delta = 0
				}
				if delta > 2000 {
					delta = 2000
				}
				if delta > 0 {
					select {
					case <-time.After(time.Duration(delta) * time.Millisecond):
					case <-playCtx.Done():
						return
					}
				}
			}
			prevTS = e.TS
			srv.Push(e.Event)
		}
		// Tail event so the UI knows playback is done.
		srv.Push(server.Event{Type: "turn_done"})
		srv.Push(server.Event{Type: "error", Message: fmt.Sprintf("replay 完成 · %d events", len(entries))})
	}()

	fmt.Fprintf(os.Stderr,
		"\033[2m  LocalKin %s · REPLAY MODE\n  File:     %s\n  Events:   %d\033[0m\n",
		version, abs, len(entries))
	fmt.Fprintf(os.Stderr, "  Open:     \033[1mhttp://%s/\033[0m\n\n", browserAddr(addr))

	if err := srv.ListenAndServe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

// mcpStatusFor merges a session's MCP config with what actually happened when
// those servers were loaded.
//
// Both halves are needed. Config alone can't say whether a server works;
// runtime results alone lose servers that are configured but disabled, and
// those must still appear in the UI or the user can't turn them back on.
func mcpStatusFor(s *session) []server.MCPServerStatus {
	if s == nil || s.mcpConfig == nil {
		return nil
	}

	// Tools grouped by the server that published them, so the UI can show
	// what each one actually contributed rather than a bare count.
	toolsByServer := map[string][]string{}
	for _, c := range s.mcpClients {
		for _, t := range c.Tools() {
			toolsByServer[c.Name()] = append(toolsByServer[c.Name()], t.Name)
		}
	}

	byName := map[string]mcp.LoadResult{}
	for _, r := range s.mcpResults {
		byName[r.Name] = r
	}

	names := make([]string, 0, len(s.mcpConfig.MCPServers))
	for name := range s.mcpConfig.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]server.MCPServerStatus, 0, len(names))
	for _, name := range names {
		cfg := s.mcpConfig.MCPServers[name]
		res := byName[name]

		st := server.MCPServerStatus{
			Name:      name,
			Command:   cfg.Command,
			Args:      cfg.Args,
			Disabled:  cfg.Disabled,
			ToolCount: res.ToolCount,
			Tools:     toolsByServer[name],
			LogPath:   mcp.ServerLogPath(name),
		}
		if res.Err != nil {
			st.Error = res.Err.Error()
		}
		// Connected means tools were actually retrieved — a process that
		// started and then failed the handshake is not usable, and showing it
		// as connected would be the misleading half of "configured but
		// silently broken".
		st.Connected = res.Err == nil && !cfg.Disabled && res.ToolCount > 0
		out = append(out, st)
	}
	return out
}

// harvestStatus reads the harvest manifest and staged candidates.
//
// Errors are returned in the payload instead of failing the request: a
// missing manifest is the normal state for someone who has never run harvest,
// and the settings UI should render an empty state for that, not an error.
func harvestStatus() server.HarvestStatus {
	out := server.HarvestStatus{ManifestPath: harvest.DefaultManifestPath()}

	home, err := os.UserHomeDir()
	if err != nil {
		out.Error = err.Error()
		return out
	}

	staged, err := harvest.ListStaged(home)
	if err != nil {
		out.Error = err.Error()
	}

	stagedPerSource := map[string]int{}
	for _, s := range staged {
		stagedPerSource[s.SourceName]++
		out.Candidates = append(out.Candidates, server.HarvestCandidate{
			Source:   s.SourceName,
			Name:     s.SkillName,
			Verdict:  string(s.Verdict),
			Reason:   s.Reason,
			Domain:   s.Domain,
			StagedAt: s.StagedAt.Format(time.RFC3339),
		})
	}

	if m, err := harvest.LoadManifest(out.ManifestPath); err == nil {
		for _, src := range m.Sources {
			out.Sources = append(out.Sources, server.HarvestSource{
				Name:         src.Name,
				URL:          src.URL,
				SkillPaths:   src.SkillPaths,
				LicenseAllow: src.LicenseAllow,
				Branch:       src.Branch,
				Staged:       stagedPerSource[src.Name],
			})
		}
	} else if out.Error == "" {
		out.Error = err.Error()
	}

	out.CachedVerdicts = harvest.LoadVerdictCache(home).Len()
	return out
}

// skillStatusFor reports which registered skills the session's soul exposes.
//
// Registered and exposed are deliberately separate numbers. Every skill in the
// registry is loaded and working; whether the model can see it is a per-soul
// decision made by skills.enable. "It's loaded but my agent says it can't do
// that" is the single most common confusion here, and collapsing the two into
// one list is what causes it.
func skillStatusFor(s *session) server.SkillStatus {
	var out server.SkillStatus
	if s == nil || s.registry == nil {
		return out
	}
	out.Soul = s.soul.Meta.Name
	out.EnablePatterns = s.soul.Meta.Skills.Enable
	out.Extras = skill.LoadExtras(skill.ExtrasPath()).For(s.soul.Meta.Name)

	// Exposure is computed from the merged list — soul plus extras — because
	// that is what FilteredToolDefs actually feeds the model. Reporting only
	// the soul's list would show a skill as hidden while the agent is using it.
	merged := effectiveEnable(s.soul)

	names := s.registry.AllNames()
	out.Counts.Registered = len(names)

	for _, name := range names {
		entry := server.SkillEntry{
			Name:    name,
			Exposed: skill.MatchesAllow(merged, name),
			Source:  "builtin",
		}
		if strings.HasPrefix(name, mcp.NamePrefix) {
			entry.Source = "mcp"
		}
		if sk, err := s.registry.Get(name); err == nil {
			entry.Description = sk.Description()
		}
		if entry.Exposed {
			out.Counts.Exposed++
		}
		out.Skills = append(out.Skills, entry)
	}

	// Enable entries that match nothing. A wildcard counts as matched if any
	// registered skill has that prefix, so `mcp_github_*` isn't reported as
	// missing merely because that server happens to be down.
	for _, pattern := range merged {
		matched := false
		for _, name := range names {
			if skill.MatchesAllow([]string{pattern}, name) {
				matched = true
				break
			}
		}
		if !matched {
			out.Missing = append(out.Missing, pattern)
		}
	}
	return out
}

// acceptStagedCandidate forges one staged candidate, for POST /api/harvest/accept.
//
// Paths mirror the CLI defaults (skills/ and skills/library/) resolved against
// the same search order the kernel uses to load skills — otherwise a skill
// forged from the UI would land somewhere the running agent never reads, and
// appear to have silently done nothing.
func acceptStagedCandidate(skillID string) (verdict, destPath, forgedName, reason string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", "", err
	}
	bin, err := os.Executable()
	if err != nil {
		return "", "", "", "", err
	}

	skillsDir := primarySkillsDir()
	opts := harvest.AcceptOptions{
		Home:          home,
		KinclawBin:    bin,
		CoderSoulPath: resolveSoulFile("coder.soul.md"),
		SkillsDir:     skillsDir,
		LibraryDir:    filepath.Join(skillsDir, "library"),
		Out:           io.Discard,
	}

	// 5 minutes: coder's own forge timeout is 240s, and this must outlast it
	// so a slow-but-succeeding forge isn't killed by its own supervisor.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := harvest.AcceptStaged(ctx, opts, skillID)
	if err != nil {
		return "", "", "", "", err
	}
	return string(res.Verdict), res.DestPath, res.ForgedName, res.Reason, nil
}

// primarySkillsDir is where a forged skill should land: the first directory
// the kernel actually searches, so the result is loadable on next start.
func primarySkillsDir() string {
	dirs := skillSearchDirs("skills")
	if len(dirs) > 0 {
		return dirs[0]
	}
	return "skills"
}

// toggleSkillExtra adds or removes one pattern from the per-soul overlay.
func toggleSkillExtra(soulName, pattern string, enable bool) error {
	path := skill.ExtrasPath()
	if path == "" {
		return fmt.Errorf("no home directory")
	}
	extras := skill.LoadExtras(path)
	if extras.BySoul == nil {
		extras.BySoul = map[string][]string{}
	}

	current := extras.BySoul[soulName]
	next := make([]string, 0, len(current)+1)
	for _, p := range current {
		if p != pattern {
			next = append(next, p)
		}
	}
	if enable {
		next = append(next, pattern)
	}
	if len(next) == 0 {
		delete(extras.BySoul, soulName)
	} else {
		extras.BySoul[soulName] = next
	}
	return extras.Save(path)
}
