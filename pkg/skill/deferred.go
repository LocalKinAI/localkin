package skill

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ToolSearchName is the kernel skill that loads deferred skills.
const ToolSearchName = "tool_search"

// toolSearchSkill is the progressive-disclosure half of `skills.defer`.
//
// Measured on the pilot soul: 23 tool schemas cost ~12K tokens per call,
// more than the 9K-token soul itself, and a handful of rarely used ones
// (web, forge, record, spawn, browser_session, tts, stt, location) are
// 5K of that. Sending them on every round is waste for a cloud brain and
// noise for a local one — small models pick tools worse from a longer
// list. Deferred skills are named one line each in the system prompt
// and only get their full schema after the model asks for them, the
// same shape as Claude Code's ToolSearch.
type toolSearchSkill struct {
	reg      *Registry
	deferred func() []string // names not yet loaded
	load     func(names []string)
}

// NewToolSearchSkill wires the search over the registry. deferred returns
// the names still hidden; load marks names as loaded (the kernel then
// rebuilds the tool list for the next round).
func NewToolSearchSkill(reg *Registry, deferred func() []string, load func([]string)) Skill {
	return &toolSearchSkill{reg: reg, deferred: deferred, load: load}
}

func (s *toolSearchSkill) Name() string { return ToolSearchName }
func (s *toolSearchSkill) Description() string {
	return "Find and load skills that are not in your current tool list. Some skills are deferred to keep " +
		"the context small; the system prompt lists them under 'Deferred skills'. Call with query=<keywords " +
		"or the exact name>; matching skills are loaded and become callable from your NEXT message (not " +
		"this one). Use load=<name,name> to load exact names without searching."
}
func (s *toolSearchSkill) ToolDef() json.RawMessage {
	return MakeToolDef(ToolSearchName, s.Description(),
		map[string]map[string]string{
			"query": {"type": "string", "description": "Keywords or a skill name, e.g. 'record video', 'playwright', 'tts'."},
			"load":  {"type": "string", "description": "Comma-separated exact skill names to load."},
		}, nil)
}

func (s *toolSearchSkill) Execute(params map[string]string) (string, error) {
	pending := s.deferred()
	if len(pending) == 0 {
		return "No deferred skills — everything you can use is already in your tool list.", nil
	}
	var picked []string
	if l := strings.TrimSpace(params["load"]); l != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(l, ",") {
			want[strings.TrimSpace(n)] = true
		}
		for _, n := range pending {
			if want[n] {
				picked = append(picked, n)
			}
		}
	}
	if q := strings.TrimSpace(params["query"]); q != "" && len(picked) == 0 {
		picked = s.search(q, pending)
	}
	if len(picked) == 0 {
		return fmt.Sprintf("No deferred skill matches. Deferred skills you can load: %s", strings.Join(pending, ", ")), nil
	}
	s.load(picked)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Loaded %d skill(s): %s — call them in your next message.\n", len(picked), strings.Join(picked, ", "))
	for _, n := range picked {
		if sk, err := s.reg.Get(n); err == nil {
			fmt.Fprintf(&sb, "\n%s: %s\n", n, firstSentence(sk.Description(), 240))
		}
	}
	return sb.String(), nil
}

// search ranks deferred skills by name/description overlap with q.
func (s *toolSearchSkill) search(q string, pending []string) []string {
	toks := strings.Fields(strings.ToLower(q))
	type hit struct {
		name  string
		score int
	}
	var hits []hit
	for _, n := range pending {
		sk, err := s.reg.Get(n)
		if err != nil {
			continue
		}
		name := strings.ToLower(n)
		desc := strings.ToLower(sk.Description())
		score := 0
		for _, t := range toks {
			if len(t) < 3 {
				// "a", "to", "of" match every description; only real
				// words count.
				continue
			}
			switch {
			case t == name:
				score += 10
			case strings.Contains(name, t):
				score += 4
			case strings.Contains(desc, t):
				score += 1
			}
		}
		if score > 0 {
			hits = append(hits, hit{n, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []string
	for i, h := range hits {
		if i >= 5 {
			break
		}
		out = append(out, h.name)
	}
	return out
}

// DeferredIndex renders the one-line-per-skill index that goes in the
// system prompt for skills that are deferred and not yet loaded.
func DeferredIndex(reg *Registry, names []string) string {
	if len(names) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n## Deferred skills (not loaded — call `tool_search` to load one before using it)\n\n")
	for _, n := range names {
		desc := ""
		if sk, err := reg.Get(n); err == nil {
			desc = firstSentence(sk.Description(), 110)
		}
		fmt.Fprintf(&sb, "- %s — %s\n", n, desc)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// firstSentence trims a description to its first sentence or n runes.
func firstSentence(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	for _, sep := range []string{". ", "。", "! ", "? "} {
		if i := strings.Index(s, sep); i > 20 {
			s = s[:i+1]
			break
		}
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
