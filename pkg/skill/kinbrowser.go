package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// kinbrowserSkill exposes the kinbrowser CLI as a KinClaw tool —
// markdown-native fetch over web URLs.
//
// kinbrowser (https://github.com/LocalKinAI/kinbrowser) is a separate
// public binary that fetches a URL through a 3-layer escalating chain
// (HTTP + readability + html→markdown ➜ Lightpanda ➜ chromedp) and
// returns clean markdown. Same contract as the kinbrain skill: we shell
// out to the binary rather than importing pkg/kinbrowser, so kinclaw
// stays a self-contained public binary with no extra Go module deps.
//
// REPLACES (when enabled in a soul): web_fetch, web_search, web,
// browser_session, web_scrape — one tool with 5× less surface area.
//
// Two actions:
//
//	open     — fetch URL, return markdown. Session-cached, no persistence.
//	archive  — fetch + save to KinBrain (~/.kinbrain/notes/<date>/web/).
//	           Opt-in: agents should only archive high-signal content,
//	           leaving KinBrain curated.
//
// If `kinbrowser` isn't on PATH, Execute returns a clear install hint.
// The skill stays registered unconditionally (souls that don't enable
// it via permissions.skills.enable see nothing).
type kinbrowserSkill struct{}

// NewKinBrowserSkill constructs the skill. No config — kinbrowser's
// own flags (--lightpanda, --no-chrome, --cache, --timeout) are not
// surfaced to the LLM; we accept the binary's sensible defaults
// (Layer 1+3 enabled, 128-entry LRU, 30s timeout). If you want
// Lightpanda (Layer 2), set up `kinbrowser` with the right env or
// run a wrapper script — keeps the skill simple.
func NewKinBrowserSkill() Skill { return &kinbrowserSkill{} }

func (s *kinbrowserSkill) Name() string { return "kinbrowser" }

func (s *kinbrowserSkill) Description() string {
	return "Markdown-native browser for fetching web pages. " +
		"Returns extracted main content as clean markdown (strips " +
		"nav/footer/ads/JS cruft). Three escalating backends behind " +
		"one interface: HTTP+readability (~100ms, 80% of sites) → " +
		"Lightpanda (JS exec, ~500ms) → chromedp full Chrome (~2s). " +
		"Replaces the older web_fetch/web_search/web/browser_session/" +
		"web_scrape skills with one uniform tool. " +
		"Two actions: 'open' (read + return markdown, no persistence) " +
		"and 'archive' (read + save to KinBrain notes/ for future " +
		"recall — opt-in, only for high-signal content like papers, " +
		"docs, authoritative sources). Backed by the `kinbrowser` " +
		"CLI from LocalKinAI/kinbrowser; install with `go install " +
		"github.com/LocalKinAI/kinbrowser/cmd/kinbrowser@latest`."
}

func (s *kinbrowserSkill) ToolDef() json.RawMessage {
	return MakeToolDef("kinbrowser", s.Description(),
		map[string]map[string]string{
			"action": {
				"type":        "string",
				"description": "'open' (fetch + return markdown) or 'archive' (fetch + persist to KinBrain).",
			},
			"url": {
				"type": "string",
				"description": "Full URL (https://...) of a specific content page (article, " +
					"paper, doc, README, post). DO NOT pass search-engine URLs " +
					"(google.com/search, bing.com/search, duckduckgo.com/?q=...) " +
					"— those return bot-challenge pages, not results. For search " +
					"queries use the `web_search` skill, then pass the result " +
					"URLs to kinbrowser one at a time. For PDFs (arxiv /pdf/) " +
					"kinbrowser auto-detects content-type and extracts text. " +
					"Layer 1 (~100ms) handles ~80% of sites; SPAs escalate to " +
					"L2 (Lightpanda) or L3 (Chrome) transparently.",
			},
		},
		[]string{"action", "url"},
	)
}

func (s *kinbrowserSkill) Execute(params map[string]string) (string, error) {
	action := params["action"]
	url := strings.TrimSpace(params["url"])

	switch action {
	case "open", "archive":
		// fall through after validation
	case "":
		return "", errors.New("kinbrowser: 'action' is required ('open' or 'archive')")
	default:
		return "", fmt.Errorf("kinbrowser: unknown action %q (use 'open' or 'archive')", action)
	}

	if url == "" {
		return "", errors.New("kinbrowser: 'url' is required")
	}

	// SKILL-LEVEL ERROR vs URL-LEVEL ERROR distinction.
	//
	// We return a Go error ONLY when the skill itself is broken
	// (kinbrowser binary missing, bad params). When kinbrowser runs
	// but THIS URL fails (timeout, 404, JS-required, Cloudflare
	// block), we return readable MARKDOWN explaining what happened.
	//
	// Why: KinClaw's circuit breaker counts "skill failed N times" —
	// it's meant to catch broken skills, not "this URL didn't work,
	// try another". Treating per-URL failures as skill errors trips
	// the breaker after 3 unrelated URL failures and blocks the
	// whole task even though kinbrowser is fine.
	//
	// Returning markdown content lets the LLM read "FAILED because X,
	// try Y" and make a decision (different URL, web_search, etc.).
	if _, err := exec.LookPath("kinbrowser"); err != nil {
		return "", errors.New(
			"kinbrowser CLI not found on PATH. Install:\n" +
				"  go install github.com/LocalKinAI/kinbrowser/cmd/kinbrowser@latest\n" +
				"Then verify with `kinbrowser version`.")
	}

	// --quiet so the LLM doesn't see the "[kinbrowser] L1 http | ..."
	// stderr decoration line on every call.
	cmd := exec.Command("kinbrowser", action, "--quiet", "--", url)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	// Fetch-level failure → return as readable markdown, NOT a Go
	// error. The LLM gets "this URL failed: <reason>" and can
	// proceed to try a different URL on its own.
	if err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		// Strip the "kinbrowser: " prefix die() adds; we re-add our
		// own header in the markdown.
		reason = strings.TrimPrefix(reason, "kinbrowser: ")
		return fmt.Sprintf(
			"# Fetch failed\n\n"+
				"**URL**: %s\n"+
				"**Reason**: %s\n\n"+
				"This is a PER-URL failure, not a kinbrowser problem. "+
				"Suggestions for the agent:\n"+
				"- Try a different URL from your search results\n"+
				"- The URL may be behind a paywall, geofence, or anti-bot challenge\n"+
				"- If you have web_search available, try a different query / source\n"+
				"- If 4-5 URLs all fail, then it might be a network issue worth surfacing to the user\n",
			url, reason), nil
	}

	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		return "# Empty content\n\n**URL**: " + url + "\n\nkinbrowser returned no extractable content. " +
			"The page may have been image-only, behind a paywall, or had no main text. " +
			"Try a different source.", nil
	}
	return out, nil
}
