package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// AcceptJob tracks one in-flight `harvest --accept`.
//
// Accept is asynchronous because it isn't a save — it spawns the coder agent
// to rewrite an external skill into KinClaw's exec form, which takes up to
// four minutes. A synchronous request would sit past every sensible client
// timeout, and the caller would have no way to tell "still forging" from
// "died".
type AcceptJob struct {
	ID      string `json:"id"`
	SkillID string `json:"skillId"`
	// Status is queued / running / done / failed.
	Status string `json:"status"`
	// Verdict mirrors harvest.AcceptVerdict on success: forged / library /
	// error / duplicate. Present only when Status is done.
	Verdict    string `json:"verdict,omitempty"`
	DestPath   string `json:"destPath,omitempty"`
	ForgedName string `json:"forgedName,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt,omitempty"`
}

// AcceptHandler runs one accept synchronously; the server wraps it in a job.
//
// Returning the four fields rather than an error alone is deliberate: a
// "duplicate" or "library" outcome is not a failure, and a UI needs to say
// which of them happened.
type AcceptHandler func(skillID string) (verdict, destPath, forgedName, reason string, err error)

type acceptRegistry struct {
	mu   sync.Mutex
	jobs map[string]*AcceptJob
	// inFlight guards against two accepts of the same candidate racing, which
	// would have two coder agents writing the same destination directory.
	inFlight map[string]bool
	seq      int
}

func newAcceptRegistry() *acceptRegistry {
	return &acceptRegistry{
		jobs:     map[string]*AcceptJob{},
		inFlight: map[string]bool{},
	}
}

// SetAcceptHandler wires POST /api/harvest/accept.
//
// Left unset, the endpoint refuses with 501 rather than silently doing
// nothing: this is the one write operation in the settings surface, and a UI
// button that appears to work while writing nothing is worse than an error.
func (s *Server) SetAcceptHandler(h AcceptHandler) { s.acceptHandler = h }

// handleAcceptStart starts a forge and returns the job immediately.
func (s *Server) handleAcceptStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.acceptHandler == nil {
		http.Error(w, `{"error":"accept not wired"}`, http.StatusNotImplemented)
		return
	}

	var body struct {
		SkillID string `json:"skillId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SkillID == "" {
		http.Error(w, `{"error":"skillId required"}`, http.StatusBadRequest)
		return
	}

	s.accepts.mu.Lock()
	if s.accepts.inFlight[body.SkillID] {
		s.accepts.mu.Unlock()
		http.Error(w, `{"error":"already forging this candidate"}`, http.StatusConflict)
		return
	}
	s.accepts.seq++
	job := &AcceptJob{
		ID:        "job-" + itoa(s.accepts.seq),
		SkillID:   body.SkillID,
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.accepts.jobs[job.ID] = job
	s.accepts.inFlight[body.SkillID] = true
	s.accepts.mu.Unlock()

	go func() {
		verdict, dest, forged, reason, err := s.acceptHandler(body.SkillID)

		s.accepts.mu.Lock()
		defer s.accepts.mu.Unlock()
		job.EndedAt = time.Now().UTC().Format(time.RFC3339)
		delete(s.accepts.inFlight, body.SkillID)
		if err != nil {
			job.Status = "failed"
			job.Error = err.Error()
			return
		}
		job.Status = "done"
		job.Verdict = verdict
		job.DestPath = dest
		job.ForgedName = forged
		job.Reason = reason
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	s.accepts.mu.Lock()
	snapshot := *job
	s.accepts.mu.Unlock()
	_ = json.NewEncoder(w).Encode(snapshot)
}

// handleAcceptStatus reports one job, or all of them.
func (s *Server) handleAcceptStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")

	s.accepts.mu.Lock()
	defer s.accepts.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	if id != "" {
		job, ok := s.accepts.jobs[id]
		if !ok {
			http.Error(w, `{"error":"no such job"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(*job)
		return
	}

	out := make([]AcceptJob, 0, len(s.accepts.jobs))
	for _, j := range s.accepts.jobs {
		out = append(out, *j)
	}
	_ = json.NewEncoder(w).Encode(out)
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
