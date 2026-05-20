package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// kinbrainSkill exposes Jacky's accumulated knowledge base — the four
// roots ~/.kinbrain/notes/ + localkin/output/ + localkin/knowledge/ +
// localkin/input/ — to KinClaw as a tool call.
//
// Why a wrapper around the `kinbrain` CLI instead of importing
// pkg/kinbrain directly: localkin-core (where kinbrain lives) is a
// private repo, while kinclaw is public. Shelling out to the binary
// keeps KinClaw's dependency closure clean — users install the
// kinbrain binary separately (one-time `go install` from their
// localkin checkout) and KinClaw exec's it. The contract is the CLI's
// stdout format, not Go types.
//
// Two actions:
//
//	recall — grep across all configured roots. Returns paths + per-root
//	         hit counts. Use this BEFORE doing anything novel: chances
//	         are the swarm or Jacky already wrote about it.
//	save   — append a short note to ~/.kinbrain/notes/. Use this AFTER
//	         finishing a task that produced a reusable insight, or when
//	         Jacky asks "remember this".
//
// If the `kinbrain` binary isn't on PATH, Execute returns a precise
// error pointing at how to install it. The skill stays registered but
// effectively no-ops — souls that don't enable `kinbrain` see nothing.
type kinbrainSkill struct{}

// NewKinBrainSkill constructs the skill. No config — paths and
// preferences are entirely owned by the kinbrain binary
// ($KINBRAIN_HOME, $LOCALKIN_REPO). KinClaw just hands it queries.
func NewKinBrainSkill() Skill { return &kinbrainSkill{} }

func (s *kinbrainSkill) Name() string { return "kinbrain" }

func (s *kinbrainSkill) Description() string {
	return "Search Jacky's accumulated knowledge base (kinbrain) — " +
		"a unified grep view over: (1) ~/.kinbrain/notes/ — manual " +
		"notes; (2) localkin/output/ — 1,500+ markdowns the swarm " +
		"has written (swarm_architect / quality_auditor / content / " +
		"ceo / tcm_conductor / spiritual masters / etc.); (3) " +
		"localkin/knowledge/ — curated bible 5-version corpus; (4) " +
		"localkin/input/ — 80+ EN + 90+ ZH spiritual classics + 90+ " +
		"TCM texts. ALWAYS call this before doing novel research — " +
		"there's a very good chance the swarm already wrote about it. " +
		"Two actions: 'recall' (grep + return matching file paths) " +
		"and 'save' (append a short note to ~/.kinbrain/notes/ for " +
		"future recall). Backed by the `kinbrain` CLI (install from " +
		"LocalKinAI/localkin-core/cmd/kinbrain)."
}

func (s *kinbrainSkill) ToolDef() json.RawMessage {
	return MakeToolDef("kinbrain", s.Description(),
		map[string]map[string]string{
			"action": {
				"type":        "string",
				"description": "'recall' (search) or 'save' (write a note)",
			},
			"query": {
				"type":        "string",
				"description": "For 'recall': the search query, e.g. 'Madame Guyon' or 'how to rename Finder folder'. Grep-style — case-insensitive substring match across all roots.",
			},
			"title": {
				"type":        "string",
				"description": "For 'save': short title for the note (becomes part of the filename + frontmatter).",
			},
			"body": {
				"type":        "string",
				"description": "For 'save': the note body (Markdown OK).",
			},
		},
		[]string{"action"},
	)
}

func (s *kinbrainSkill) Execute(params map[string]string) (string, error) {
	// Validate action FIRST — a soul that called us wrong should see
	// "use recall|save", not "go install ..." (both true, but the
	// former is the immediate fix).
	action := params["action"]
	switch action {
	case "recall", "save":
		// fall through to binary check
	case "":
		return "", errors.New("kinbrain: 'action' is required ('recall' or 'save')")
	default:
		return "", fmt.Errorf("kinbrain: unknown action %q (use 'recall' or 'save')", action)
	}

	if _, err := exec.LookPath("kinbrain"); err != nil {
		return "", errors.New(
			"kinbrain CLI not found on PATH. Install it from your " +
				"localkin checkout:\n" +
				"  cd ~/Documents/Workspace/localkin && go install ./cmd/kinbrain\n" +
				"Then verify with `kinbrain version`. If you're missing " +
				"the repo, the kinbrain source lives at " +
				"github.com/LocalKinAI/localkin-core/cmd/kinbrain (private).")
	}

	switch action {
	case "recall":
		return s.recall(params["query"])
	case "save":
		return s.save(params["title"], params["body"])
	}
	return "", nil // unreachable
}

func (s *kinbrainSkill) recall(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("kinbrain recall: 'query' is required")
	}
	// kinbrain prints matches to stdout (per-root sections + paths)
	// and "N total" to stderr. We want both.
	cmd := exec.Command("kinbrain", "recall", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// `kinbrain recall` does not exit non-zero on "no matches"; if
		// we got here something actually went wrong. Pass through.
		return "", fmt.Errorf("kinbrain recall: %w\n%s", err, stderr.String())
	}
	out := stdout.String()
	if extra := strings.TrimSpace(stderr.String()); extra != "" {
		out += "\n" + extra
	}
	if strings.TrimSpace(out) == "" {
		return "(no matches in any kinbrain root)", nil
	}
	return out, nil
}

func (s *kinbrainSkill) save(title, body string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("kinbrain save: 'title' is required")
	}
	if strings.TrimSpace(body) == "" {
		return "", errors.New("kinbrain save: 'body' is required")
	}
	// `kinbrain save <source> <title>` reads body from stdin.
	// Source bucket = "kinclaw" so notes captured by an LLM via the
	// claw are visually distinct from manual `kinbrain save thought`
	// entries when browsing ~/.kinbrain/notes/<date>/.
	cmd := exec.Command("kinbrain", "save", "kinclaw", title)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kinbrain save: %w\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}
