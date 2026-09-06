package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// permissionRegistry holds the approval requests a turn is blocked on.
//
// The kernel's permission gate calls AskPermission from inside the turn
// goroutine; the request goes out as a `permission_request` SSE event
// and the goroutine parks on a channel until POST /api/permission
// answers with the same id, the turn is interrupted, or the wait times
// out. One request at a time in practice (turns are serialized), but
// keyed by id so a stale answer to an earlier request is rejected
// rather than approving the wrong call.
type permissionRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan string
}

func newPermissionRegistry() *permissionRegistry {
	return &permissionRegistry{waiters: map[string]chan string{}}
}

// PermissionTimeout bounds how long a turn waits for a human. Long on
// purpose: the user may be away from the machine, and the alternative
// to waiting is denying — which the model then has to explain.
const PermissionTimeout = 10 * time.Minute

// AskPermission publishes the request and blocks for the decision:
// "allow", "allow_session" or "deny". A closed ctx (interrupted turn)
// or the timeout yields "deny" with an error explaining which.
func (s *Server) AskPermission(ctx context.Context, ev Event) (string, error) {
	if ev.ID == "" {
		return "deny", fmt.Errorf("permission request without id")
	}
	ch := make(chan string, 1)
	s.permissions.mu.Lock()
	s.permissions.waiters[ev.ID] = ch
	s.permissions.mu.Unlock()
	defer func() {
		s.permissions.mu.Lock()
		delete(s.permissions.waiters, ev.ID)
		s.permissions.mu.Unlock()
	}()

	ev.Type = "permission_request"
	s.Push(ev)

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		s.Push(Event{Type: "permission_resolved", ID: ev.ID, Message: "cancelled"})
		return "deny", fmt.Errorf("turn interrupted while waiting for approval")
	case <-time.After(PermissionTimeout):
		s.Push(Event{Type: "permission_resolved", ID: ev.ID, Message: "timeout"})
		return "deny", fmt.Errorf("no answer within %s", PermissionTimeout)
	}
}

// QuestionTimeout bounds how long a turn waits for an ask_user answer.
// Longer than approvals: a question is often "which folder?" asked of a
// user who stepped away, and a wrong guess costs more than the wait.
const QuestionTimeout = 30 * time.Minute

// AskQuestion publishes an ask_user question and blocks for the answer
// text from POST /api/answer. Cancelled ctx or timeout return an error.
func (s *Server) AskQuestion(ctx context.Context, ev Event) (string, error) {
	if ev.ID == "" {
		return "", fmt.Errorf("question without id")
	}
	ch := make(chan string, 1)
	s.permissions.mu.Lock()
	s.permissions.waiters[ev.ID] = ch
	s.permissions.mu.Unlock()
	defer func() {
		s.permissions.mu.Lock()
		delete(s.permissions.waiters, ev.ID)
		s.permissions.mu.Unlock()
	}()

	ev.Type = "question"
	s.Push(ev)

	select {
	case a := <-ch:
		return a, nil
	case <-ctx.Done():
		s.Push(Event{Type: "question_resolved", ID: ev.ID, Message: "cancelled"})
		return "", fmt.Errorf("turn interrupted while waiting for an answer")
	case <-time.After(QuestionTimeout):
		s.Push(Event{Type: "question_resolved", ID: ev.ID, Message: "timeout"})
		return "", fmt.Errorf("no answer within %s", QuestionTimeout)
	}
}

// handleAnswer resolves an ask_user question. Body: {"id": "...", "text": "..."}.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, `{"error":"id and text required"}`, http.StatusBadRequest)
		return
	}
	s.permissions.mu.Lock()
	ch, ok := s.permissions.waiters[body.ID]
	if ok {
		delete(s.permissions.waiters, body.ID)
	}
	s.permissions.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"no such pending question"}`, http.StatusNotFound)
		return
	}
	ch <- body.Text
	s.Push(Event{Type: "question_resolved", ID: body.ID, Message: body.Text})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleWorkspace reports (GET) or changes (POST {"path": ...}) the
// session workspace, broadcasting `workspace` on change.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		ws := ""
		if s.stateHandler != nil {
			ws = s.stateHandler().Workspace
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"workspace": ws})
	case http.MethodPost:
		if s.workspaceHandler == nil {
			http.Error(w, `{"error":"workspace not wired"}`, http.StatusNotImplemented)
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			http.Error(w, `{"error":"path required"}`, http.StatusBadRequest)
			return
		}
		ws, err := s.workspaceHandler(body.Path)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		s.AllowDir(ws)
		s.Push(Event{Type: "workspace", Workspace: ws})
		_ = json.NewEncoder(w).Encode(map[string]string{"workspace": ws})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleRoutines lists (GET), adds (POST {name,prompt,schedule,soul?})
// or removes (DELETE ?id=) scheduled runs.
func (s *Server) handleRoutines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.routines == nil {
		http.Error(w, `{"error":"routines not wired"}`, http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := s.routines.List()
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []RoutineInfo{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"routines": list})
	case http.MethodPost:
		var body struct {
			Name, Prompt, Schedule, Soul string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		info, err := s.routines.Add(body.Name, body.Prompt, body.Schedule, body.Soul)
		if err != nil {
			// Saved-but-not-scheduled comes back as 202 with the error so
			// the UI can show the routine and the reason together.
			if info.ID != "" {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"routine": info, "warning": err.Error()})
				return
			}
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"routine": info})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
			return
		}
		if err := s.routines.Remove(id); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		http.Error(w, "GET, POST or DELETE", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoutineRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || s.routines == nil {
		http.Error(w, `{"error":"POST only / not wired"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}
	if err := s.routines.Run(body.ID); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRoutineEnable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || s.routines == nil {
		http.Error(w, `{"error":"POST only / not wired"}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}
	if err := s.routines.SetEnabled(body.ID, body.Enabled); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRoutineLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || s.routines == nil {
		http.Error(w, "GET only / not wired", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	text, err := s.routines.Log(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(text))
}

// PendingPermissions lists request ids currently waiting — a UI that
// reconnects mid-wait can ask and re-render the card.
func (s *Server) PendingPermissions() []string {
	s.permissions.mu.Lock()
	defer s.permissions.mu.Unlock()
	out := make([]string, 0, len(s.permissions.waiters))
	for id := range s.permissions.waiters {
		out = append(out, id)
	}
	return out
}

// handlePermission answers one request. Body: {"id": "...",
// "decision": "allow" | "allow_session" | "deny"}. 404 if the id is not
// waiting (already answered, timed out, or never existed).
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"pending": s.PendingPermissions()})
		return
	case http.MethodPost:
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, `{"error":"id and decision required"}`, http.StatusBadRequest)
		return
	}
	switch body.Decision {
	case "allow", "allow_session", "allow_always", "deny":
	default:
		http.Error(w, `{"error":"decision must be allow | allow_session | allow_always | deny"}`, http.StatusBadRequest)
		return
	}
	s.permissions.mu.Lock()
	ch, ok := s.permissions.waiters[body.ID]
	if ok {
		delete(s.permissions.waiters, body.ID)
	}
	s.permissions.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"no such pending request"}`, http.StatusNotFound)
		return
	}
	ch <- body.Decision
	// Tell every other connected UI the card can go away.
	s.Push(Event{Type: "permission_resolved", ID: body.ID, Message: body.Decision})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleState reports the live session state.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.stateHandler == nil {
		_ = json.NewEncoder(w).Encode(State{})
		return
	}
	_ = json.NewEncoder(w).Encode(s.stateHandler())
}

// handlePlanMode flips the read-only gate. Body: {"enabled": bool}.
// Replies {"enabled": <resulting state>} and broadcasts `plan_mode`.
func (s *Server) handlePlanMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.planModeHandler == nil {
		http.Error(w, "plan_mode not wired", http.StatusNotImplemented)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state := s.planModeHandler(body.Enabled)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": state})
	s.Push(Event{Type: "plan_mode", PlanMode: state})
}

// handleCompact folds the conversation on demand. 409 when a turn is
// running or there is nothing to fold.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.compactHandler == nil {
		http.Error(w, "compact not wired", http.StatusNotImplemented)
		return
	}
	msg, err := s.compactHandler()
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": msg})
}

// handleSearchStatus reports the last web_search outcome — which
// backend answered, from which engines, which were down. No probing.
func (s *Server) handleSearchStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.search == nil || s.search.Status == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{})
		return
	}
	_ = json.NewEncoder(w).Encode(s.search.Status())
}

// handleSearchProbe runs one restricted health check against the
// meta-search on demand (POST, so nothing polls it by accident).
func (s *Server) handleSearchProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.search == nil || s.search.Probe == nil {
		http.Error(w, `{"error":"search probe not wired"}`, http.StatusNotImplemented)
		return
	}
	_ = json.NewEncoder(w).Encode(s.search.Probe())
}
