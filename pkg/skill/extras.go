package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// ExtrasPath is ~/.localkin/skill_extras.json.
func ExtrasPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".localkin", "skill_extras.json")
}

// Extras are skills enabled in addition to what a soul's own enable list
// grants, stored per soul name.
//
// This exists so the UI never rewrites soul files. A soul is hand-authored —
// pilot's enable list is interleaved with comments explaining why each entry
// is there — and a settings toggle that machine-rewrites that file would
// eventually mangle someone's reasoning to save them one text edit.
//
// Additive only. There is no "disable" counterpart, and that asymmetry is
// deliberate: adding a skill is the user granting a capability to their own
// agent, while subtracting one would let a UI silently contradict what the
// soul says it does, leaving a file that no longer describes the running
// agent. Removing stays an edit to the soul, where it's visible in git.
type Extras struct {
	// BySoul maps soul name → extra enable patterns (wildcards allowed, same
	// syntax as skills.enable).
	BySoul map[string][]string `json:"bySoul"`
}

// LoadExtras reads the overlay. A missing file is the normal state and yields
// an empty overlay, not an error.
func LoadExtras(path string) *Extras {
	e := &Extras{BySoul: map[string][]string{}}
	if path == "" {
		return e
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return e
	}
	var parsed Extras
	if err := json.Unmarshal(data, &parsed); err != nil {
		// A hand-corrupted overlay must not take down skill loading; the agent
		// still works with exactly what its soul grants.
		return e
	}
	if parsed.BySoul != nil {
		e.BySoul = parsed.BySoul
	}
	return e
}

// For returns the extra patterns configured for one soul.
func (e *Extras) For(soulName string) []string {
	if e == nil {
		return nil
	}
	return e.BySoul[soulName]
}

// Save writes the overlay back.
func (e *Extras) Save(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// EffectiveEnable merges a soul's own enable list with its extras.
//
// Order matters for one edge case: an empty soul enable list means "expose
// everything", so extras must not turn that into a restrictive allowlist. If
// the soul enables nothing explicitly, the merged result stays empty and the
// permissive behaviour is preserved.
func EffectiveEnable(soulEnable []string, extras []string) []string {
	if len(soulEnable) == 0 {
		return nil
	}
	if len(extras) == 0 {
		return soulEnable
	}

	seen := make(map[string]bool, len(soulEnable)+len(extras))
	out := make([]string, 0, len(soulEnable)+len(extras))
	for _, p := range append(append([]string{}, soulEnable...), extras...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// SortedNames returns the map keys in a stable order, for display.
func (e *Extras) SortedNames() []string {
	if e == nil {
		return nil
	}
	names := make([]string, 0, len(e.BySoul))
	for k := range e.BySoul {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
