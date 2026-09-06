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
	case "allow", "allow_session", "deny":
	default:
		http.Error(w, `{"error":"decision must be allow | allow_session | deny"}`, http.StatusBadRequest)
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
