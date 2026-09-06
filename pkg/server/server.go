// Package server hosts the kinclaw "watch-it-work" UI: a single-file
// HTML page on / and a Server-Sent-Events stream on /api/events that
// pushes every text delta + tool call + tool result the kernel produces.
//
// The transport choice is deliberate. SSE is one direction (server →
// client), one TCP stream, no extra deps, no upgrade dance. The other
// direction is a single POST /api/chat that just kicks a turn — chat
// I/O is asymmetric (user types in bursts, agent streams continuously)
// so SSE matches the shape and lets us keep the deps list at zero.
//
// File serving: the kernel emits absolute filesystem paths for
// screenshots and recordings. /file/<abspath> serves them with a strict
// allow-list prefix check (no .. traversal, no arbitrary disk reads)
// so the browser can render them as <img src> / <video src>.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var staticFS embed.FS

// Event is a single UI update pushed to all SSE subscribers. The shape
// is intentionally flat — one struct, JSON-tagged, fields filled per
// Type. Frontend dispatches on Type; missing fields are zero values.
type Event struct {
	Type string `json:"type"`

	// user_message / assistant text — text_delta carries deltas, the
	// frontend appends to the current assistant bubble until a tool_call
	// or turn_done arrives.
	Text     string `json:"text,omitempty"`
	Thinking bool   `json:"thinking,omitempty"`

	// tool_call: which claw, with what params.
	ID     string            `json:"id,omitempty"`
	Name   string            `json:"name,omitempty"`
	Params map[string]string `json:"params,omitempty"`

	// tool_result / screen_frame / record_done.
	Output string   `json:"output,omitempty"`
	Images []string `json:"images,omitempty"` // absolute paths
	URLs   []string `json:"urls,omitempty"`   // browser-fetchable /file/... URLs
	Path   string   `json:"path,omitempty"`
	URL    string   `json:"url,omitempty"`

	// error
	Message string `json:"message,omitempty"`

	// permission_request: what the human is asked to approve. ID is
	// the request id to echo back on POST /api/permission; Name /
	// Params describe the call; Summary is the one-line rendering.
	Summary string `json:"summary,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// usage: token accounting after each model call. ContextLength is
	// the soul's window so a UI can draw "62% of context" without
	// knowing the model. Same field names kincode emits.
	InputTokens   int `json:"input_tokens,omitempty"`
	OutputTokens  int `json:"output_tokens,omitempty"`
	ContextLength int `json:"context_length,omitempty"`

	// plan_mode: the toggle state (true = read-only gate on).
	PlanMode bool `json:"plan_mode,omitempty"`

	// compacted: how much the fold removed. Message carries the summary
	// preview; Output carries the full summary text.
	BeforeTokens int `json:"before_tokens,omitempty"`
	AfterTokens  int `json:"after_tokens,omitempty"`

	// question (ask_user): Message is the question, Options the choices;
	// answer via POST /api/answer with the same ID.
	Options []string `json:"options,omitempty"`

	// workspace: the directory file operations default to.
	Workspace string `json:"workspace,omitempty"`
}

// State is the GET /api/state answer — enough for a UI to render its
// header on connect without waiting for the next event.
type State struct {
	Soul           string   `json:"soul"`
	Brain          string   `json:"brain"`
	Skills         int      `json:"skills"`
	PermissionMode string   `json:"permission_mode"`
	PlanMode       bool     `json:"plan_mode"`
	SessionAllowed []string `json:"session_allowed,omitempty"`
	Messages       int      `json:"messages"`
	InputTokens    int      `json:"input_tokens"`
	ContextLength  int      `json:"context_length"`
	// TotalInput / TotalOutput are the running sums for this process —
	// the /cost view.
	TotalInput  int `json:"total_input_tokens"`
	TotalOutput int `json:"total_output_tokens"`
	// Workspace is the directory relative paths and shell commands use.
	Workspace string `json:"workspace"`
	// Deferred lists skills withheld until tool_search loads them; Loaded
	// the ones the model has pulled in this session.
	Deferred []string `json:"deferred,omitempty"`
	Loaded   []string `json:"loaded,omitempty"`
}

// WorkspaceHandler changes the session workspace; returns the cleaned
// path it settled on.
type WorkspaceHandler func(path string) (string, error)

// RoutineHandlers back the /api/routines endpoints.
type RoutineHandlers struct {
	List       func() ([]RoutineInfo, error)
	Add        func(name, prompt, schedule, soul string) (RoutineInfo, error)
	Remove     func(id string) error
	Run        func(id string) error
	SetEnabled func(id string, on bool) error
	Log        func(id string) (string, error)
}

// RoutineInfo is one scheduled run as the UI sees it.
type RoutineInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Soul      string `json:"soul,omitempty"`
	Schedule  string `json:"schedule"`     // human form
	Raw       string `json:"schedule_raw"` // what was typed
	Enabled   bool   `json:"enabled"`
	Installed bool   `json:"installed"` // LaunchAgent present
	LastRun   string `json:"last_run,omitempty"`
	LogPath   string `json:"log_path"`
	CreatedAt string `json:"created_at"`
}

// StateHandler reports the live session state.
type StateHandler func() State

// PlanModeHandler flips plan mode and returns the resulting state. The
// handler may refuse (e.g. mid-turn) by returning the old state.
type PlanModeHandler func(enabled bool) bool

// CompactHandler folds the conversation now. Returns a human-readable
// outcome line, or an error (nothing to compact, turn in flight).
type CompactHandler func() (string, error)

// ChatHandler is invoked for every POST /api/chat. Runs in its own
// goroutine — should call back into Server.Push to stream events.
// Context is request-scoped; the handler should respect cancellation
// (browser closed, etc.).
type ChatHandler func(ctx context.Context, message string)

// InterruptHandler is invoked when the browser asks to abort the
// current turn (DELETE /api/chat or "Esc" in the UI). Implementation
// should cancel whatever ctx the running turn is using. No-op if no
// turn is in flight.
type InterruptHandler func()

// SoulInfo is one entry in the soul-list response. Active marks the
// currently-loaded soul so the UI can highlight it.
type SoulInfo struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Brain  string `json:"brain"`
	Active bool   `json:"active,omitempty"`
}

// SoulListHandler returns the souls available to switch to. Path is
// absolute. The implementation decides the search strategy (typically
// ./souls/ + ~/.localkin/souls/).
type SoulListHandler func() []SoulInfo

// SoulSwitchHandler swaps the running session over to the soul at
// path. Should refuse if a turn is in flight (caller-policy). Returns
// an error if the soul fails to load.
type SoulSwitchHandler func(path string) error

// BrainSwitchRequest is the body of POST /api/brain. APIKey and
// Endpoint are optional — handler falls back to env vars / soul
// defaults when they're empty. Mirrors kincode's shape so the same
// Mac dropdown can drive both kernels.
type BrainSwitchRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// BrainSwitchHandler swaps the brain (provider + model) on the
// running session WITHOUT changing the soul. Lets the user keep
// Pilot's prompt + skills + permissions while flipping between
// claude / openai / ollama backends. Should atomically swap so
// in-flight turns either complete on the old brain or fail clean.
// Returns an error if the new config is invalid (missing key for
// non-ollama, unreachable endpoint, etc.).
type BrainSwitchHandler func(req BrainSwitchRequest) error

// SessionResetHandler clears the kernel's conversation history for
// the running session WITHOUT changing the soul, brain, skills, or
// permissions. The "New session" button in the UI hits this so the
// next user message starts from a clean tape — otherwise a previous
// turn's mid-task chatter (esp. a stuck tool-call retry loop) bleeds
// into the new turn and confuses the model. Should refuse if a turn
// is in flight (caller-policy via TryLock on turnMu).
type SessionResetHandler func() error

// MCPServerStatus is one configured MCP server as the settings UI sees it.
//
// Config and runtime state are reported together on purpose: a UI that reads
// mcp.json alone can show what the user asked for but not whether it worked,
// and "configured but silently failing" is the state that actually needs
// showing.
type MCPServerStatus struct {
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	Disabled  bool     `json:"disabled"`
	Connected bool     `json:"connected"`
	ToolCount int      `json:"toolCount"`
	Error     string   `json:"error,omitempty"`
	LogPath   string   `json:"logPath,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// MCPStatusHandler reports the MCP servers this kernel loaded.
type MCPStatusHandler func() []MCPServerStatus

// HarvestSource is one configured skill source.
type HarvestSource struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	SkillPaths   []string `json:"skillPaths,omitempty"`
	LicenseAllow []string `json:"licenseAllow,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	// Candidates currently staged from this source, so the UI can show which
	// sources are actually producing anything — a source that has yielded
	// nothing across many runs is a candidate for removal.
	Staged int `json:"staged"`
}

// HarvestCandidate is one staged skill awaiting review.
type HarvestCandidate struct {
	Source   string `json:"source"`
	Name     string `json:"name"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
	Domain   string `json:"domain,omitempty"`
	StagedAt string `json:"stagedAt,omitempty"`
}

// HarvestStatus is the whole picture for the settings UI.
type HarvestStatus struct {
	ManifestPath string             `json:"manifestPath"`
	Sources      []HarvestSource    `json:"sources"`
	Candidates   []HarvestCandidate `json:"candidates"`
	// Verdicts already cached, i.e. how much of a scheduled run is free.
	CachedVerdicts int    `json:"cachedVerdicts"`
	Error          string `json:"error,omitempty"`
}

// HarvestStatusHandler reports harvest configuration and staged candidates.
type HarvestStatusHandler func() HarvestStatus

// SkillEntry is one registered skill and whether the active soul exposes it.
type SkillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Exposed     bool   `json:"exposed"`
	// Source distinguishes where a skill came from, since that decides who
	// maintains it: "builtin" ships in the repo, "mcp" comes from an external
	// server configured in mcp.json.
	Source string `json:"source"`
}

// SkillStatus answers "what can this agent actually do right now".
//
// The same registry produces a different answer per soul, because each soul's
// skills.enable list is its own. Reporting registered and exposed separately
// is the point: "the skill is loaded but this soul can't see it" is the most
// common confusion, and a single flat list hides it.
type SkillStatus struct {
	Soul string `json:"soul"`
	// EnablePatterns is the soul's raw allowlist, wildcards included.
	EnablePatterns []string     `json:"enablePatterns"`
	Skills         []SkillEntry `json:"skills"`
	// Missing are enable entries matching nothing registered — typos, or a
	// skill that failed to load. Worth surfacing: the agent silently lacks a
	// capability its soul claims to grant.
	Missing []string `json:"missing,omitempty"`
	// Extras are patterns added from the settings UI on top of the soul's own
	// list. Reported separately so the UI can show which toggles are its own
	// doing and which come from the (read-only) soul file.
	Extras []string `json:"extras,omitempty"`
	Counts struct {
		Registered int `json:"registered"`
		Exposed    int `json:"exposed"`
	} `json:"counts"`
}

// SkillStatusHandler reports the active soul's skill exposure.
type SkillStatusHandler func() SkillStatus

// SkillExtrasHandler adds or removes an extra enable pattern for the active
// soul. Additive overlay only — it never edits the soul file.
type SkillExtrasHandler func(pattern string, enable bool) error

type Server struct {
	addr             string
	chatHandler      ChatHandler
	interruptHandler InterruptHandler
	soulList         SoulListHandler
	soulSwitch       SoulSwitchHandler
	brainSwitch      BrainSwitchHandler
	sessionReset     SessionResetHandler
	mcpStatus        MCPStatusHandler
	harvestStatus    HarvestStatusHandler
	skillStatus      SkillStatusHandler
	acceptHandler    AcceptHandler
	skillExtras      SkillExtrasHandler
	stateHandler     StateHandler
	planModeHandler  PlanModeHandler
	compactHandler   CompactHandler
	workspaceHandler WorkspaceHandler
	routines         *RoutineHandlers
	accepts          *acceptRegistry
	permissions      *permissionRegistry
	allowedDirs      []string // /file allow-list (absolute, cleaned)

	mu          sync.Mutex
	subs        map[chan Event]struct{}
	hello       *Event // pushed to each new subscriber on connect (soul info)
	eventLogger EventLogger
	// firstSubCh is closed when the first subscriber connects. Replay
	// mode waits on this so it doesn't push the recorded events into
	// the void before the browser opens. Reset to nil after firing.
	firstSubCh chan struct{}

	// Live-screen feed cache. 800ms TTL absorbs client polling faster
	// than the macOS screencapture call returns (~100ms). All access
	// guarded by mu.
	liveScreen     LiveScreenCapture
	liveScreenInfo LiveScreenInfo
	liveCache      []byte
	liveCacheStamp time.Time
}

// New constructs a server. allowedDirs are filesystem prefixes that
// /file is willing to serve from (e.g. ~/Library/Caches/kinclaw,
// ./output). Anything outside returns 403. Empty list = no /file
// service at all.
func New(addr string, allowedDirs []string, h ChatHandler) *Server {
	clean := make([]string, 0, len(allowedDirs))
	for _, d := range allowedDirs {
		if abs, err := filepath.Abs(d); err == nil {
			clean = append(clean, abs)
		}
	}
	return &Server{
		addr: addr, chatHandler: h, allowedDirs: clean,
		subs:        make(map[chan Event]struct{}),
		accepts:     newAcceptRegistry(),
		permissions: newPermissionRegistry(),
	}
}

// SetStateHandler wires GET /api/state.
func (s *Server) SetStateHandler(h StateHandler) { s.stateHandler = h }

// SetPlanModeHandler wires POST /api/plan_mode. Same request/response
// shape as kincode so KinClaw Mac's existing toggle drives both.
func (s *Server) SetPlanModeHandler(h PlanModeHandler) { s.planModeHandler = h }

// SetCompactHandler wires POST /api/compact.
func (s *Server) SetCompactHandler(h CompactHandler) { s.compactHandler = h }

// SetWorkspaceHandler wires POST /api/workspace.
func (s *Server) SetWorkspaceHandler(h WorkspaceHandler) { s.workspaceHandler = h }

// SetRoutineHandlers wires the /api/routines endpoints.
func (s *Server) SetRoutineHandlers(h *RoutineHandlers) { s.routines = h }

// AllowDir adds a directory to the /file allow-list at runtime (a newly
// chosen workspace may hold screenshots the UI wants to render).
func (s *Server) AllowDir(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.allowedDirs {
		if d == abs {
			return
		}
	}
	s.allowedDirs = append(s.allowedDirs, abs)
}

// SetInterruptHandler wires the abort path. Optional — without it
// DELETE /api/chat returns 501 and the UI's interrupt button fails
// gracefully (input stays disabled until normal turn_done).
func (s *Server) SetInterruptHandler(h InterruptHandler) {
	s.interruptHandler = h
}

// SetSoulHandlers wires the soul list + switch endpoints. Both
// optional — without them the UI's dropdown will hit 501 and stay
// in single-soul mode.
func (s *Server) SetSoulHandlers(list SoulListHandler, switcher SoulSwitchHandler) {
	s.soulList = list
	s.soulSwitch = switcher
}

// SetBrainSwitchHandler wires POST /api/brain. Without it the
// endpoint returns 501 — UI's brain dropdown will fail with a
// clear "not wired" error.
func (s *Server) SetBrainSwitchHandler(h BrainSwitchHandler) { s.brainSwitch = h }

// SetSessionResetHandler wires POST /api/session/reset. Without it
// the endpoint returns 501 — Mac UI's "New session" button will fall
// back to clearing only client-side state (which leaves the kernel's
// history buffer dirty — the bug this endpoint exists to fix).
func (s *Server) SetSessionResetHandler(h SessionResetHandler) { s.sessionReset = h }

// SetMCPStatusHandler wires GET /api/mcp. Optional: without it the endpoint
// reports an empty server list rather than failing.
func (s *Server) SetMCPStatusHandler(h MCPStatusHandler) { s.mcpStatus = h }

// SetHarvestStatusHandler wires GET /api/harvest.
func (s *Server) SetHarvestStatusHandler(h HarvestStatusHandler) { s.harvestStatus = h }

// SetSkillStatusHandler wires GET /api/skills.
func (s *Server) SetSkillStatusHandler(h SkillStatusHandler) { s.skillStatus = h }

// SetSkillExtrasHandler wires POST /api/skills/extras.
func (s *Server) SetSkillExtrasHandler(h SkillExtrasHandler) { s.skillExtras = h }

// EventLogger is called for every event Push'd to subscribers. Used
// by the recorder in serve.go to append events to a JSONL session
// log, which `kinclaw serve --replay <file>` can replay verbatim.
// Hook is called BEFORE the broadcast so the log captures all events
// even if a subscriber's channel is full and would have dropped.
type EventLogger func(Event)

// SetEventLogger installs the per-event hook. nil unhooks.
func (s *Server) SetEventLogger(l EventLogger) {
	s.mu.Lock()
	s.eventLogger = l
	s.mu.Unlock()
}

// LiveScreenCapture grabs a fresh screenshot of the user's desktop
// and returns the JPEG bytes. Called by the /api/screen/current.jpg
// route when the browser polls for the live feed. Should be quick
// (~80-150ms typical) — server caches result for 800ms to absorb
// over-eager polling.
type LiveScreenCapture func() ([]byte, error)

// LiveScreenInfo describes what the capture is currently targeting
// — used by the /api/screen/info endpoint so the UI can label the
// feed (e.g. "🔴 LIVE · Reminders" instead of just "🔴 LIVE").
// Implementation may report the tracked app's name; "" means we're
// falling back to whole-display capture.
type LiveScreenInfo func() string

// SetLiveScreenCapture wires the live-screen feed. Without it the
// /api/screen/current.jpg route returns 501 and the UI's "agent's
// eyes" mode falls back to the empty placeholder.
func (s *Server) SetLiveScreenCapture(c LiveScreenCapture) {
	s.mu.Lock()
	s.liveScreen = c
	s.mu.Unlock()
}

// SetLiveScreenInfo wires the metadata callback for the feed. nil
// (or unwired) reports as untracked / whole-screen.
func (s *Server) SetLiveScreenInfo(i LiveScreenInfo) {
	s.mu.Lock()
	s.liveScreenInfo = i
	s.mu.Unlock()
}

// SetHello stores an event that will be pushed to every new SSE
// subscriber as soon as their stream opens. Used to ship soul/brain
// metadata so the page header can render before the first turn.
func (s *Server) SetHello(ev Event) {
	s.mu.Lock()
	s.hello = &ev
	s.mu.Unlock()
}

// Push fans an event out to every subscriber non-blockingly. A slow
// browser tab won't stall the kernel — its channel just drops events
// once full (64-deep buffer, ~plenty of headroom for normal turns).
// Also calls the eventLogger if installed (records BEFORE broadcast
// so every event is captured even if a sub drops).
func (s *Server) Push(ev Event) {
	s.mu.Lock()
	logger := s.eventLogger
	s.mu.Unlock()
	if logger != nil {
		logger(ev)
	}
	s.mu.Lock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.mu.Unlock()
}

// FileURL turns an absolute filesystem path into a /file/... URL the
// browser can fetch. Returns "" for paths that aren't allowed — the
// frontend will fall back to showing the path as text.
func (s *Server) FileURL(abs string) string {
	if abs == "" {
		return ""
	}
	clean, err := filepath.Abs(abs)
	if err != nil {
		return ""
	}
	s.mu.Lock()
	dirs := append([]string(nil), s.allowedDirs...)
	s.mu.Unlock()
	for _, dir := range dirs {
		if strings.HasPrefix(clean, dir+string(os.PathSeparator)) || clean == dir {
			return "/file" + clean
		}
	}
	return ""
}

func (s *Server) subscribe() chan Event {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	signalCh := s.firstSubCh
	s.firstSubCh = nil
	s.mu.Unlock()
	// First-subscriber signal: replay mode blocks on this so the
	// browser is connected before playback fires the first event.
	if signalCh != nil {
		close(signalCh)
	}
	return ch
}

// WaitForFirstSubscriber blocks until at least one SSE client is
// connected (or ctx fires). Returns nil immediately if there's
// already a subscriber. Used by replay mode to gate playback.
func (s *Server) WaitForFirstSubscriber(ctx context.Context) error {
	s.mu.Lock()
	if len(s.subs) > 0 {
		s.mu.Unlock()
		return nil
	}
	if s.firstSubCh == nil {
		s.firstSubCh = make(chan struct{})
	}
	ch := s.firstSubCh
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) unsubscribe(ch chan Event) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
	// Drain then close so any in-flight Push goroutines don't panic on
	// send-to-closed (Push holds the mu while sending, but the brief
	// window between unlock and close-by-caller is enough).
	close(ch)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/souls", s.handleSouls)
	mux.HandleFunc("/api/soul", s.handleSoul)
	mux.HandleFunc("/api/brain", s.handleBrain)
	mux.HandleFunc("/api/session/reset", s.handleSessionReset)
	mux.HandleFunc("/api/mcp", s.handleMCP)
	mux.HandleFunc("/api/harvest", s.handleHarvest)
	mux.HandleFunc("/api/skills", s.handleSkills)
	mux.HandleFunc("/api/skills/extras", s.handleSkillExtras)
	mux.HandleFunc("/api/harvest/accept", s.handleAcceptStart)
	mux.HandleFunc("/api/harvest/accept/status", s.handleAcceptStatus)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/plan_mode", s.handlePlanMode)
	mux.HandleFunc("/api/compact", s.handleCompact)
	mux.HandleFunc("/api/permission", s.handlePermission)
	mux.HandleFunc("/api/answer", s.handleAnswer)
	mux.HandleFunc("/api/workspace", s.handleWorkspace)
	mux.HandleFunc("/api/routines", s.handleRoutines)
	mux.HandleFunc("/api/routines/run", s.handleRoutineRun)
	mux.HandleFunc("/api/routines/enable", s.handleRoutineEnable)
	mux.HandleFunc("/api/routines/log", s.handleRoutineLog)
	mux.HandleFunc("/api/screen/current.jpg", s.handleLiveScreen)
	mux.HandleFunc("/api/screen/info", s.handleLiveScreenInfo)
	mux.HandleFunc("/api/voice/transcribe", s.handleVoiceTranscribe)
	mux.HandleFunc("/api/voice/tts", s.handleVoiceTTS)
	mux.HandleFunc("/file/", s.handleFile)

	// Bind manually rather than using srv.ListenAndServe so we can
	// catch the "address in use" error early and print an
	// actionable message — port 5000/7000 collide with macOS's
	// AirPlay Receiver, which is the #1 cause of "Access denied
	// 403" via browser when the user assumed they were hitting
	// kinclaw.
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		_, port, _ := net.SplitHostPort(s.addr)
		hint := ""
		if port == "5000" || port == "7000" {
			hint = "\n  hint: macOS AirPlay Receiver binds 5000/7000 by default.\n" +
				"        关闭: 系统设置 → 通用 → 隔空播放接收器 → 关\n" +
				"        或换端口: -port 8020 (default) / 8088 / 7777"
		}
		return fmt.Errorf("listen %s: %w%s", s.addr, err, hint)
	}

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "  serve: http://%s\n", s.addr)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // bypass nginx buffering if anyone proxies

	ch := s.subscribe()
	defer s.unsubscribe(ch)

	// Hello so the browser flips into "connected" state immediately
	// rather than waiting for the first real event.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Send the hello event (soul info) to this subscriber if one was
	// configured. Per-subscriber, not via the broadcast — late joiners
	// shouldn't replay history, but they do need to know what soul
	// they're talking to.
	s.mu.Lock()
	hello := s.hello
	s.mu.Unlock()
	if hello != nil {
		if data, err := json.Marshal(hello); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleChatPost(w, r)
	case http.MethodDelete:
		s.handleChatDelete(w, r)
	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleChatPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	// Echo the user message into the SSE stream so the frontend can
	// just listen and not duplicate render logic.
	s.Push(Event{Type: "user_message", Text: msg})

	// Run the turn in its own goroutine; we return 202 immediately so
	// the browser's POST resolves and the SSE stream is the only
	// long-lived connection.
	go s.chatHandler(context.Background(), msg)
	w.WriteHeader(http.StatusAccepted)
}

// handleChatDelete is the abort endpoint. UI hits this when the user
// presses Esc or clicks the interrupt button. We delegate to the
// installed InterruptHandler (if any), which cancels the in-flight
// turn's ctx; the running runTurn observes the cancellation, pushes
// an error event + turn_done, releases the turn lock.
func (s *Server) handleChatDelete(w http.ResponseWriter, _ *http.Request) {
	if s.interruptHandler == nil {
		http.Error(w, "interrupt not wired", http.StatusNotImplemented)
		return
	}
	s.interruptHandler()
	w.WriteHeader(http.StatusAccepted)
}

// handleSouls returns the list of souls available to swap to.
// JSON: [{path, name, brain, active}]. 501 if the handler isn't wired.
// handleMCP reports configured MCP servers and whether they connected.
//
// Read-only. Editing goes through the config file, because the servers are
// launched at kernel start: accepting a write here would leave the UI showing
// a server the running kernel has never heard of.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.mcpStatus == nil {
		// Not wired is not an error — a kernel built without MCP support
		// should answer "no servers", not 501, so the UI can render an empty
		// state instead of a failure.
		_ = json.NewEncoder(w).Encode([]MCPServerStatus{})
		return
	}
	out := s.mcpStatus()
	if out == nil {
		out = []MCPServerStatus{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// handleHarvest reports the skill sources and what they've staged.
//
// Read-only, like /api/mcp: harvest runs on a schedule and writes to disk, so
// a write endpoint here would be editing state a background job owns.
// handleSkills reports which skills the active soul exposes to the model.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.skillStatus == nil {
		_ = json.NewEncoder(w).Encode(SkillStatus{Skills: []SkillEntry{}})
		return
	}
	out := s.skillStatus()
	if out.Skills == nil {
		out.Skills = []SkillEntry{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// handleSkillExtras toggles one extra enable pattern for the active soul.
//
// Writes to the overlay file, never to the soul. Enabling a skill this way is
// the user granting their own agent a capability; the soul file stays exactly
// as they wrote it, comments and all.
func (s *Server) handleSkillExtras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.skillExtras == nil {
		http.Error(w, `{"error":"extras not wired"}`, http.StatusNotImplemented)
		return
	}
	var body struct {
		Pattern string `json:"pattern"`
		Enable  bool   `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Pattern == "" {
		http.Error(w, `{"error":"pattern required"}`, http.StatusBadRequest)
		return
	}
	if err := s.skillExtras(body.Pattern, body.Enable); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleHarvest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.harvestStatus == nil {
		_ = json.NewEncoder(w).Encode(HarvestStatus{Sources: []HarvestSource{}, Candidates: []HarvestCandidate{}})
		return
	}
	out := s.harvestStatus()
	if out.Sources == nil {
		out.Sources = []HarvestSource{}
	}
	if out.Candidates == nil {
		out.Candidates = []HarvestCandidate{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleSouls(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if s.soulList == nil {
		http.Error(w, "soul listing not wired", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.soulList())
}

// handleSoul switches the running session to a different soul.
// Body: {"path": "/abs/path/to/x.soul.md"}. Returns 202 on success,
// 4xx with the error message if the swap is refused (e.g. turn in
// flight, soul doesn't load).
func (s *Server) handleSoul(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.soulSwitch == nil {
		http.Error(w, "soul switching not wired", http.StatusNotImplemented)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Path == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if err := s.soulSwitch(body.Path); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleBrain swaps the running session's brain (provider + model)
// without touching the soul. Body: {provider, model, api_key?,
// endpoint?}. On success returns 202; on failure returns the error
// from the brain handler (e.g. "anthropic API key required") with
// 4xx so the UI can show "couldn't switch".
//
// Mirrors kincode's /api/brain endpoint by design — same Mac
// dropdown drives both kernels with identical request shape.
func (s *Server) handleBrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.brainSwitch == nil {
		http.Error(w, "brain switching not wired", http.StatusNotImplemented)
		return
	}
	var body BrainSwitchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Provider == "" || body.Model == "" {
		http.Error(w, "provider and model are required", http.StatusBadRequest)
		return
	}
	if err := s.brainSwitch(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)

	// Re-emit a hello event with the new brain marker so any new
	// SSE subscribers see the live state. UI dropdown also refreshes
	// optimistically based on its own POST result.
	s.Push(Event{Type: "brain_switched"})
}

// handleSessionReset clears the kernel's per-session conversation
// history (in-memory + sqlite) without rebuilding the soul/brain.
// Returns 202 on success, 409 if a turn is in flight (the caller
// should hit DELETE /api/chat first to abort, then retry), 501 if
// the handler isn't wired.
//
// Reset events fan out as `session_reset` SSE so any open browser
// tab can clear its local rendered transcript without a full reload.
func (s *Server) handleSessionReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.sessionReset == nil {
		http.Error(w, "session reset not wired", http.StatusNotImplemented)
		return
	}
	if err := s.sessionReset(); err != nil {
		// Most likely cause is "turn in progress" — surface as 409
		// Conflict so the UI can show a useful message instead of a
		// generic 500. Other errors (sqlite write failure) are also
		// retriable from the user's perspective so 409 still fits.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleLiveScreen serves a fresh JPEG screenshot of the user's
// desktop on every request — the "agent's eyes" feed. Cached for
// 800ms so the UI's polling at 1.5s + occasional double-fetch don't
// trigger redundant captures.
//
// Cache-Control: no-store; we WANT the browser to refetch each time
// (rather than serve from disk cache) — the URL has a ?t=timestamp
// cache-buster too as belt-and-suspenders.
func (s *Server) handleLiveScreen(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	cap := s.liveScreen
	if cap == nil {
		s.mu.Unlock()
		http.Error(w, "live screen not wired (macOS only?)", http.StatusNotImplemented)
		return
	}
	// Hit cache if recent enough.
	if s.liveCache != nil && time.Since(s.liveCacheStamp) < 800*time.Millisecond {
		data := s.liveCache
		s.mu.Unlock()
		writeImageJPEG(w, data)
		return
	}
	s.mu.Unlock()

	data, err := cap()
	if err != nil {
		http.Error(w, "capture failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.liveCache = data
	s.liveCacheStamp = time.Now()
	s.mu.Unlock()
	writeImageJPEG(w, data)
}

func writeImageJPEG(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	_, _ = w.Write(data)
}

// handleLiveScreenInfo returns JSON metadata about what the live
// feed is tracking. UI calls this on a slow cadence (every few sec)
// to update the "🔴 LIVE · <app>" label.
func (s *Server) handleLiveScreenInfo(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	info := s.liveScreenInfo
	s.mu.Unlock()
	app := ""
	if info != nil {
		app = info()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"tracked_app": app})
}

// handleVoiceTranscribe proxies the browser's mic recording to the
// LocalKin Service Audio Server (default :8000 / SenseVoice). The
// request is multipart/form-data with a "file" field; we forward as-
// is. Same response shape: {"text":"...","language":...,"confidence":...}.
//
// Why proxy instead of letting the browser hit :8000 directly:
// CORS. The audio server doesn't set Access-Control-Allow-Origin,
// so the browser would 403 on a cross-origin POST. Proxying keeps
// it single-origin from the browser's view.
func (s *Server) handleVoiceTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	endpoint := os.Getenv("STT_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:8000"
	}
	target := strings.TrimRight(endpoint, "/") + "/transcribe"

	upstream, err := http.NewRequest("POST", target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Pass through Content-Type (incl. multipart boundary) — that's
	// the only header the audio server needs to parse the upload.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upstream.Header.Set("Content-Type", ct)
	}
	// 60s for a long-ish recording. Local SenseVoice typically
	// transcribes in under 2-3s for normal sentence-length input.
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(upstream)
	if err != nil {
		http.Error(w, "STT server unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleVoiceTTS proxies a {text, speaker?} JSON request to the
// LocalKin TTS server (default :8001 / Kokoro). Returns audio/wav
// bytes the browser can play via <audio> or AudioContext.
//
// CJK auto-detection happens client-side in our index.html (it picks
// the speaker based on text content) — keeping the server proxy
// dumb. If the body already has a "speaker" field, it's preserved.
func (s *Server) handleVoiceTTS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	endpoint := os.Getenv("TTS_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:8001"
	}
	target := strings.TrimRight(endpoint, "/") + "/synthesize"

	upstream, err := http.NewRequest("POST", target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(upstream)
	if err != nil {
		http.Error(w, "TTS server unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	// Forward response — audio/wav typically ~500KB-2MB for a normal
	// reply length. Streaming the body chunks rather than buffering
	// the whole thing keeps memory flat on long synthesis.
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleFile serves files from the allow-list. Path traversal: we
// filepath.Clean and require the result to live under one of the
// allowed dirs (prefix + path separator, so /tmp/foo doesn't match
// /tmp/foobar). Caller is the same machine running the agent —
// this is defense-in-depth not a hardened service.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimPrefix(r.URL.Path, "/file")
	if rawPath == "" || rawPath[0] != '/' {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean(rawPath)
	allowed := false
	for _, dir := range s.allowedDirs {
		if strings.HasPrefix(clean, dir+string(os.PathSeparator)) || clean == dir {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}
	// Set a permissive cache for screenshots so flipping back to a
	// frame doesn't refetch. Recordings get the same — they're
	// content-addressed by mtime+name.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, clean)
}
