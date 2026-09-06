// Package compact keeps a long-running agent inside its context window.
//
// A kinclaw session is meant to run for days (menubar app, REPL that
// resumes across restarts). Every tool round appends to history, and a
// single `ui tree` or web fetch can add tens of kilobytes. Without
// compaction the prompt grows until the provider rejects it — or,
// worse, silently degrades as the model loses the thread in noise.
//
// The approach is the one Claude Code uses: when the prompt crosses a
// fraction of the window, fold the older part of the conversation into
// a dense summary written by the model itself, keep the most recent
// messages verbatim, and continue. The summary is stored as an
// ordinary user message so it survives restarts and is visible in
// `kinclaw memory sessions`.
package compact

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/brain"
)

// SummaryPrefix opens every compaction summary message. UIs and the
// memory CLI key off it to render the summary as a divider rather
// than as something the user typed.
const SummaryPrefix = "[Context compacted"

// Config tunes when and how much to compact. Zero values take defaults.
type Config struct {
	// ContextLength is the model window in tokens (soul brain.context_length).
	ContextLength int
	// Threshold is the fraction of the window that triggers compaction.
	// Default 0.75 — leaves headroom for the reply and one more tool round.
	Threshold float64
	// KeepRecent is the minimum number of trailing messages kept verbatim.
	// Default 8. Alignment may keep a few more so tool groups stay intact.
	KeepRecent int
}

const (
	defaultThreshold  = 0.75
	defaultKeepRecent = 8
	// minSummarize is the fewest messages worth folding. Below this the
	// summary would be longer than what it replaces.
	minSummarize = 4
)

func (c Config) withDefaults() Config {
	if c.Threshold <= 0 || c.Threshold >= 1 {
		c.Threshold = defaultThreshold
	}
	if c.KeepRecent <= 0 {
		c.KeepRecent = defaultKeepRecent
	}
	if c.ContextLength <= 0 {
		c.ContextLength = 8192
	}
	return c
}

// Result describes what one compaction did, for logs and UI events.
type Result struct {
	BeforeTokens int
	AfterTokens  int
	Summarized   int // messages folded into the summary
	Kept         int // messages preserved verbatim
	Summary      string
}

// Needed reports whether the history should be compacted before the
// next model call. lastInput is the provider-reported prompt size of
// the previous call (the honest number); when it is zero — first call,
// or a server that doesn't report usage — a CJK-aware estimate over
// system prompt + history stands in.
func Needed(lastInput int, system string, history []brain.Message, cfg Config) bool {
	cfg = cfg.withDefaults()
	if SplitPoint(history, cfg.KeepRecent) == 0 {
		return false
	}
	used := lastInput
	if used == 0 {
		used = brain.EstimateText(system) + brain.EstimateTokens(history)
	}
	return float64(used) >= float64(cfg.ContextLength)*cfg.Threshold
}

// SplitPoint returns the index where the kept tail begins: history[:i]
// is summarized, history[i:] stays verbatim. The split always lands on
// a user message so no assistant tool_call is separated from its
// results. Returns 0 when there is nothing worth summarizing.
func SplitPoint(history []brain.Message, keepRecent int) int {
	if keepRecent <= 0 {
		keepRecent = defaultKeepRecent
	}
	if len(history) < keepRecent+minSummarize {
		return 0
	}
	// Walk back from the latest allowed split until we sit on a plain
	// user message (not a tool result — those are RoleTool, so any
	// RoleUser is a real turn boundary or a system notice).
	i := len(history) - keepRecent
	for i > 0 && history[i].Role != brain.RoleUser {
		i--
	}
	if i < minSummarize {
		return 0
	}
	return i
}

// Compact folds history[:split] into a summary produced by b and
// returns the new history: summary, an assistant acknowledgement, then
// the kept tail. On summarizer failure the original history is
// returned with the error so the caller can carry on uncompacted.
func Compact(ctx context.Context, b brain.Brain, history []brain.Message, cfg Config) ([]brain.Message, Result, error) {
	cfg = cfg.withDefaults()
	split := SplitPoint(history, cfg.KeepRecent)
	if split == 0 {
		return history, Result{}, fmt.Errorf("nothing to compact (%d messages)", len(history))
	}
	older, recent := history[:split], history[split:]
	before := brain.EstimateTokens(history)

	// The summarizer's own prompt must fit the window too. Budget ~60%
	// of it for the transcript and drop from the oldest end when over.
	transcript, omitted := Transcript(older, int(float64(cfg.ContextLength)*0.6))
	if omitted > 0 {
		transcript = fmt.Sprintf("[%d earlier messages omitted from this transcript]\n\n%s", omitted, transcript)
	}

	req := []brain.Message{
		{Role: brain.RoleSystem, Content: summaryInstructions},
		{Role: brain.RoleUser, Content: "Transcript to compact:\n\n" + transcript},
	}
	res, err := b.Chat(ctx, req, nil, nil)
	if err != nil {
		return history, Result{}, fmt.Errorf("summarizer: %w", err)
	}
	summary := strings.TrimSpace(res.Content)
	if summary == "" {
		return history, Result{}, fmt.Errorf("summarizer returned empty summary")
	}

	out := make([]brain.Message, 0, len(recent)+2)
	out = append(out,
		brain.Message{Role: brain.RoleUser, Content: fmt.Sprintf(
			"%s %s — summary of the earlier conversation; %d messages folded]\n\n%s",
			SummaryPrefix, time.Now().Format("2006-01-02 15:04"), len(older), summary)},
		brain.Message{Role: brain.RoleAssistant, Content: "Understood — continuing from that state."},
	)
	out = append(out, recent...)
	return out, Result{
		BeforeTokens: before, AfterTokens: brain.EstimateTokens(out),
		Summarized: len(older), Kept: len(recent), Summary: summary,
	}, nil
}

// IsSummary reports whether m is a compaction summary message.
func IsSummary(m brain.Message) bool {
	return m.Role == brain.RoleUser && strings.HasPrefix(m.Content, SummaryPrefix)
}

// Transcript renders messages as a plain-text log for the summarizer,
// newest last. Tool results are clipped per message and, when the
// whole thing exceeds budget tokens, the oldest entries are dropped;
// the count dropped is returned so the caller can say so.
func Transcript(msgs []brain.Message, budget int) (string, int) {
	const perResult = 1500 // chars kept from each tool result
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == brain.RoleAssistant && len(m.ToolCalls) > 0:
			var sb strings.Builder
			if m.Content != "" {
				sb.WriteString("assistant: " + m.Content + "\n")
			}
			for _, tc := range m.ToolCalls {
				sb.WriteString(fmt.Sprintf("assistant → %s(%s)", tc.Function.Name, clip(tc.Function.Arguments, 400)))
				sb.WriteString("\n")
			}
			lines = append(lines, strings.TrimRight(sb.String(), "\n"))
		case m.Role == brain.RoleTool:
			lines = append(lines, "tool result: "+clip(m.Content, perResult))
		default:
			lines = append(lines, m.Role+": "+clip(m.Content, 4000))
		}
	}
	omitted := 0
	for len(lines) > 1 && brain.EstimateText(strings.Join(lines, "\n\n")) > budget {
		lines = lines[1:]
		omitted++
	}
	return strings.Join(lines, "\n\n"), omitted
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf(" …[%d more chars]", len(r)-n)
}

// summaryInstructions is the summarizer's system prompt. It asks for
// exactly what the agent needs to keep working after the fold: the
// user's asks in their words, what happened, where things are, and
// what is still open — with identifiers kept literal, because a
// paraphrased file path or bundle id is a path that no longer exists.
const summaryInstructions = `You are compacting the working memory of a computer-use agent so it can continue a long session with less context. Write a dense summary in the same language the user has been writing in. Use these sections, omitting any that are empty:

1. User's goals — what they asked for, in their own words where it matters.
2. Done so far — actions taken, tools used, and their outcomes.
3. Artifacts — every file path, URL, app / bundle id, window title, identifier, number or name that was produced or discovered. Copy these EXACTLY; never paraphrase an identifier.
4. Current state — the step in progress, the current todo list with statuses, anything the agent was about to do.
5. Decisions & preferences — choices made, constraints given, user preferences or corrections learned.
6. Open issues — errors hit, things that failed, questions awaiting the user.

Be concrete and terse. No preamble, no commentary about the summary itself.`
