package skill

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Skill interface {
	Name() string
	Description() string
	ToolDef() json.RawMessage // OpenAI function-calling format
	Execute(params map[string]string) (string, error)
}

type Registry struct {
	mu     sync.RWMutex
	skills map[string]Skill
}

func NewRegistry() *Registry {
	return &Registry{skills: make(map[string]Skill)}
}

func (r *Registry) Register(s Skill) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skills[s.Name()] = s
}

func (r *Registry) Get(name string) (Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	if !ok {
		return nil, fmt.Errorf("skill not found: %s", name)
	}
	return s, nil
}

// SetSpawnResultCallback installs the async-completion hook on the
// registered spawn skill (if any). Called from serve.go after the SSE
// Server is constructed — the spawn skill needs a way to deliver
// detached child results back to the UI without going through the
// parent's tool_result protocol (the parent's turn already ended).
//
// Returns false if no spawn skill is registered (soul didn't enable
// it, or kinclaw built without it). Caller can ignore the return —
// it just means detached spawn won't have a delivery path and falls
// back to sync.
func (r *Registry) SetSpawnResultCallback(cb SpawnResultCallback) bool {
	r.mu.RLock()
	s, ok := r.skills["spawn"]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	if sp, ok := s.(*spawnSkill); ok {
		sp.SetResultCallback(cb)
		return true
	}
	return false
}

func (r *Registry) ToolDefs() []json.RawMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]json.RawMessage, 0, len(r.skills))
	for _, s := range r.skills {
		defs = append(defs, s.ToolDef())
	}
	return defs
}

// AllNames returns the names of every registered skill, sorted.
// Used by buildRegistry's startup banner to surface the full list
// of skills (vs. just the count) and by diagnostics that need to
// know what's actually in the registry.
func (r *Registry) AllNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.skills))
	for name := range r.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) FilteredToolDefs(allow []string) []json.RawMessage {
	if len(allow) == 0 {
		return r.ToolDefs()
	}
	m := newAllowMatcher(allow)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []json.RawMessage
	for _, s := range r.skills {
		if m.matches(s.Name()) {
			defs = append(defs, s.ToolDef())
		}
	}
	return defs
}

// allowMatcher resolves a soul's `skills.enable` list, which is exact names
// plus optional trailing-`*` prefixes.
//
// Wildcards exist for MCP. A server's tool names are chosen by the server and
// are only knowable after connecting to it, so an exact-match-only allowlist
// would require the user to launch the agent, read the log to discover the
// names, then edit the soul — every time a server updates its tool set.
// `mcp_github_*` says "trust this server", which is the decision the user is
// actually making when they add it to mcp.json.
//
// Prefix-only by design, not full glob. `*` in the middle invites patterns
// like `*_write` that read as safe but silently widen as new skills land.
type allowMatcher struct {
	exact    map[string]bool
	prefixes []string
}

func newAllowMatcher(allow []string) allowMatcher {
	m := allowMatcher{exact: make(map[string]bool, len(allow))}
	for _, name := range allow {
		if strings.HasSuffix(name, "*") {
			m.prefixes = append(m.prefixes, strings.TrimSuffix(name, "*"))
			continue
		}
		m.exact[name] = true
	}
	return m
}

func (m allowMatcher) matches(name string) bool {
	if m.exact[name] {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// MatchesAllow reports whether a skill name passes an enable list. Exported
// so callers that report on the allowlist (startup summaries, settings UI)
// agree with what FilteredToolDefs actually did.
func MatchesAllow(allow []string, name string) bool {
	if len(allow) == 0 {
		return true
	}
	return newAllowMatcher(allow).matches(name)
}

type ToolCallInfo struct {
	ID, Name string
	Params   map[string]string
}
type ToolResult struct {
	ToolCallID, Name, Output string
	// Images are file paths to attach to the tool message when the
	// brain supports vision. Skills opt in by emitting `image://<path>`
	// markers in their text output; ExecuteToolCalls strips those
	// markers and populates this field. The brain adapter reads the
	// files at send time and inlines them as image_url / image source
	// blocks. Brains without vision support ignore the field, so
	// adding markers in a skill is always safe.
	Images []string
	Err    error
}

// extractImageMarkers strips lines matching `image://<path>` from the
// tool output and returns the cleaned text + the list of paths
// encountered (de-duped, in order of first appearance). Skills can
// emit any number of markers anywhere in their output; the markers
// don't reach the LLM as text — they get rerouted into the message's
// Images list, which becomes inline image content in the next
// vision-capable API call.
func extractImageMarkers(out string) (cleaned string, images []string) {
	if !strings.Contains(out, "image://") {
		return out, nil
	}
	seen := make(map[string]bool)
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "image://") {
			path := strings.TrimSpace(strings.TrimPrefix(t, "image://"))
			if path != "" && !seen[path] {
				seen[path] = true
				images = append(images, path)
			}
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimRight(strings.Join(keep, "\n"), "\n"), images
}

// exclusiveSkills must never run concurrently with another call in the
// same round. They share one physical resource — the user's screen,
// keyboard, clipboard, foreground app — or mutate the same tree of
// files. Two `input` calls racing produce interleaved keystrokes; a
// `ui click` racing a `screen screenshot` captures a half-transitioned
// window; two `file_edit`s on one file clobber each other. Read-only
// and network skills (web_search × 8, kinbrain, memory, spawn) stay
// parallel — that fan-out is what makes researcher fast.
var exclusiveSkills = map[string]bool{
	"screen": true, "ui": true, "input": true, "record": true, "smart_click": true,
	"cerebellum": true, "kinthink": true, "app_open_clean": true, "tts": true, "stt": true,
	"shell": true, "file_write": true, "file_edit": true, "forge": true,
}

// exclusivePrefixes catches app-driving SKILL.md families by name.
var exclusivePrefixes = []string{"notes_", "music_", "imsg_", "mail_"}

// ParallelSafe reports whether a skill may run alongside others in the
// same round. Unknown skills are assumed safe — that was the behaviour
// before this check existed and external skills were designed for it.
func ParallelSafe(name string) bool {
	if exclusiveSkills[name] {
		return false
	}
	for _, p := range exclusivePrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

func executeOne(reg *Registry, c ToolCallInfo) ToolResult {
	s, err := reg.Get(c.Name)
	if err != nil {
		return ToolResult{ToolCallID: c.ID, Name: c.Name, Output: err.Error(), Err: err}
	}
	out, err := s.Execute(c.Params)
	if err != nil {
		return ToolResult{ToolCallID: c.ID, Name: c.Name, Output: fmt.Sprintf("Error: %v", err), Err: err}
	}
	cleaned, imgs := extractImageMarkers(out)
	return ToolResult{ToolCallID: c.ID, Name: c.Name, Output: cleaned, Images: imgs}
}

// ExecuteToolCalls runs a round's calls and returns results in call
// order. Calls run concurrently only when every one of them is
// ParallelSafe; a single exclusive skill in the batch serializes the
// whole round in the order the model emitted it, which is the order
// the model meant.
func ExecuteToolCalls(reg *Registry, calls []ToolCallInfo) []ToolResult {
	if len(calls) == 0 {
		return nil
	}
	results := make([]ToolResult, len(calls))
	parallel := len(calls) > 1
	for _, c := range calls {
		if !ParallelSafe(c.Name) {
			parallel = false
			break
		}
	}
	if !parallel {
		for i, c := range calls {
			results[i] = executeOne(reg, c)
		}
		return results
	}
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(idx int, ci ToolCallInfo) {
			defer wg.Done()
			results[idx] = executeOne(reg, ci)
		}(i, c)
	}
	wg.Wait()
	return results
}

func MakeToolDef(name, description string, properties map[string]map[string]string, required []string) json.RawMessage {
	props := make(map[string]interface{})
	for k, v := range properties {
		props[k] = v
	}
	schema := map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		},
	}
	b, _ := json.Marshal(schema)
	return b
}
