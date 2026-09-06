// turn.go — the one agent loop both front-ends drive.
//
// The REPL prints to a terminal; `kinclaw serve` pushes SSE events.
// They used to be two hand-mirrored loops (chatLoop / runTurn), which
// was fine while a round was "call the brain, run the tools". Once a
// round also has to consult the permission gate, run hooks, cap and
// spill oversized outputs, track usage and compact the context, two
// copies stop being cheaper than one interface with six methods.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/brain"
	"github.com/LocalKinAI/kinclaw/pkg/compact"
	"github.com/LocalKinAI/kinclaw/pkg/permission"
	"github.com/LocalKinAI/kinclaw/pkg/skill"
)

// turnSink receives what a turn produces, in order. Implementations
// must be cheap and non-blocking — they run on the turn goroutine.
type turnSink interface {
	text(chunk string, thinking bool)
	toolCall(id, name string, params map[string]string)
	toolResult(r skill.ToolResult)
	// notice carries kernel-originated text the user should see but
	// the model did not write: circuit-breaker trips, hook blocks,
	// permission denials, compaction failures.
	notice(msg string)
	usage(u brain.Usage, contextLength int)
	compacted(res compact.Result)
}

// compactConfig derives the compaction settings from the soul.
func compactConfig(sess *session) compact.Config {
	return compact.Config{
		ContextLength: sess.soul.Meta.Brain.ContextLength,
		Threshold:     sess.soul.Meta.Context.CompactAt,
		KeepRecent:    sess.soul.Meta.Context.KeepRecent,
	}
}

// buildMessages assembles the prompt: system (soul, workspace, deferred
// skill index, plan-mode directive when on) followed by the live history.
func buildMessages(sess *session) []brain.Message {
	system := sess.soul.SystemPrompt
	if sess.workspace != "" {
		system += "\n\n## Workspace\n\n" + sess.workspace +
			" — relative paths resolve here and shell commands run here. Writing outside it asks the user first."
	}
	system += skill.DeferredIndex(sess.registry, sess.pendingDeferred())
	if sess.gate != nil && sess.gate.PlanMode() {
		system += permission.PlanModeDirective
	}
	messages := []brain.Message{{Role: brain.RoleSystem, Content: system}}
	return append(messages, sess.history...)
}

// askUser routes an ask_user call to the human, or tells the model
// nobody is there. Never an error: the model must always get a result
// it can act on.
func (s *session) askUser(ctx context.Context, q skill.Question) string {
	if strings.TrimSpace(q.Text) == "" {
		return "ask_user needs a question."
	}
	if s.questioner == nil {
		return "No interactive user is available to answer right now. Proceed with your best judgment, state the assumption you made, and repeat the question in your final reply."
	}
	answer, err := s.questioner.Ask(ctx, q)
	if err != nil {
		return fmt.Sprintf("The user did not answer (%v). Proceed with your best judgment and say which assumption you made.", err)
	}
	if strings.TrimSpace(answer) == "" {
		return "The user sent an empty answer. Proceed with your best judgment and say so."
	}
	return "User answered: " + answer
}

// appendMessage adds m to the live history and the store.
func (s *session) appendMessage(m brain.Message) {
	s.history = append(s.history, m)
	if s.store != nil {
		_ = s.store.SaveMessage(s.id, m)
	}
}

// recordUsage folds one call's usage into the session counters.
func (s *session) recordUsage(u brain.Usage) {
	if u.Total() == 0 && u.OutputTokens == 0 {
		return
	}
	s.lastUsage = u
	s.totalIn += u.Total()
	s.totalOut += u.OutputTokens
}

// compactNow folds the history unconditionally (the /compact command
// and POST /api/compact). Persists the result so a restart resumes
// from the compacted state, and archives the pre-compaction rows so
// nothing is lost.
func compactNow(ctx context.Context, sess *session) (compact.Result, error) {
	newHist, res, err := compact.Compact(ctx, sess.brain, sess.history, compactConfig(sess))
	if err != nil {
		return res, err
	}
	sess.history = newHist
	// The provider-reported size is stale now; the next call reports
	// the real post-compaction number.
	sess.lastUsage = brain.Usage{}
	if sess.store != nil {
		if _, _, err := sess.store.ArchiveSession(sess.id); err != nil {
			return res, fmt.Errorf("archive before compaction: %w", err)
		}
		if err := sess.store.SaveMessages(sess.id, newHist); err != nil {
			return res, fmt.Errorf("persist compacted history: %w", err)
		}
	}
	return res, nil
}

// maybeCompact runs compactNow when the prompt has crossed the soul's
// threshold. Failure is reported and otherwise ignored: an oversized
// prompt is the provider's problem to reject, not a reason to abort
// the user's turn early.
func maybeCompact(ctx context.Context, sess *session, sink turnSink) {
	if sess.soul.Meta.Context.Disabled {
		return
	}
	if !compact.Needed(sess.lastUsage.Total(), sess.soul.SystemPrompt, sess.history, compactConfig(sess)) {
		return
	}
	res, err := compactNow(ctx, sess)
	if err != nil {
		sink.notice("[context] compaction skipped: " + err.Error())
		return
	}
	sink.compacted(res)
}

// runTurnCore drives one user → assistant → tool* → assistant cycle,
// appending and persisting every message as it goes so an interrupted
// turn leaves the store consistent (LoadHistory sanitizes the rest).
// Returns the assistant's final text.
func runTurnCore(ctx context.Context, sess *session, input string, sink turnSink) (string, error) {
	sess.appendMessage(brain.Message{Role: brain.RoleUser, Content: input})

	cb := skill.NewCircuitBreaker()
	forgeFired := false
	onChunk := func(chunk string, thinking bool) error {
		sink.text(chunk, thinking)
		return nil
	}

	for round := 0; round < maxToolRounds; round++ {
		maybeCompact(ctx, sess, sink)

		result, err := sess.brain.Chat(ctx, buildMessages(sess), sess.toolDefs, onChunk)
		if err != nil {
			return "", err
		}
		sess.recordUsage(result.Usage)
		if result.Usage.Total() > 0 {
			sink.usage(result.Usage, sess.soul.Meta.Brain.ContextLength)
		}

		if len(result.ToolCalls) == 0 {
			sess.appendMessage(brain.Message{Role: brain.RoleAssistant, Content: result.Content})
			if forgeFired {
				sess.refreshToolDefs()
			}
			if sess.hooks != nil && !sess.hooks.Empty() {
				// Stop hooks are notifications; never hold the reply for them.
				reply := result.Content
				go sess.hooks.Stop(context.Background(), reply)
			}
			return result.Content, nil
		}

		sess.appendMessage(brain.Message{
			Role: brain.RoleAssistant, Content: result.Content, ToolCalls: result.ToolCalls,
		})

		results := runRound(ctx, sess, result.ToolCalls, sink, &forgeFired)

		// tool_search loaded something: the next round's tool list must
		// carry the new schemas, otherwise the model was told "callable
		// from your next message" and it isn't.
		for _, tc := range result.ToolCalls {
			if tc.Function.Name == skill.ToolSearchName || (forgeFired && tc.Function.Name == "forge") || sess.deferred[tc.Function.Name] {
				sess.refreshToolDefs()
				break
			}
		}

		if tripped, tripMsg := cb.Record(results); tripped {
			sink.notice(tripMsg)
			sess.appendMessage(brain.Message{Role: brain.RoleUser, Content: tripMsg})
		}

		// Cerebellum short-circuit: a lone "ok:" result is itself the
		// answer for one-shot / benchmark souls; skip the "I'm done"
		// round trip. See soul.Meta.Cerebellum.ExitOnOK.
		if sess.soul.Meta.Cerebellum.ExitOnOK && len(results) == 1 && isOkLine(results[0].Output) {
			if sess.debug {
				fmt.Fprintf(os.Stderr, "\033[2m[cerebellum.exit_on_ok: short-circuit after %s]\033[0m\n", results[0].Name)
			}
			return results[0].Output, nil
		}
	}
	return "", fmt.Errorf("too many tool call rounds (max %d)", maxToolRounds)
}

// runRound turns one assistant message's tool calls into tool results:
// parse → hooks(pre) → permission gate → execute → cap/spill →
// hooks(post) → persist. Results come back in call order, one per
// call, including the ones that never executed.
func runRound(ctx context.Context, sess *session, calls []brain.ToolCall, sink turnSink, forgeFired *bool) []skill.ToolResult {
	results := make([]skill.ToolResult, len(calls))
	decided := make([]bool, len(calls))
	var toRun []skill.ToolCallInfo
	var runIdx []int

	for i, tc := range calls {
		name := tc.Function.Name
		if name == "forge" {
			*forgeFired = true
		}
		params, perr := tc.ParseArguments()
		if perr != nil {
			results[i] = skill.ToolResult{ToolCallID: tc.ID, Name: name, Output: "Error: " + perr.Error(), Err: perr}
			decided[i] = true
			continue
		}
		if sess.debug {
			fmt.Fprintf(os.Stderr, "\033[2m[tool: %s %v]\033[0m\n", name, params)
		}
		sink.toolCall(tc.ID, name, params)

		// ask_user is answered by a human, not a skill: it needs the
		// turn's context and the front-end's prompt channel.
		if name == skill.AskUserName {
			answer := sess.askUser(ctx, skill.ParseQuestion(tc.ID, params))
			results[i] = skill.ToolResult{ToolCallID: tc.ID, Name: name, Output: answer}
			decided[i] = true
			continue
		}

		// A deferred skill called before tool_search loaded it: load it
		// now and hand the model the schema instead of "skill not
		// found". Small models skip the search step; the parameters they
		// guessed without a schema are not trusted, so the call itself
		// is not executed.
		if sess.deferred[name] && !sess.loaded[name] {
			sess.loaded[name] = true
			desc := ""
			if sk, err := sess.registry.Get(name); err == nil {
				desc = sk.Description()
			}
			results[i] = skill.ToolResult{ToolCallID: tc.ID, Name: name, Output: fmt.Sprintf(
				"%s was deferred and is now loaded with its full schema (see your tool list). Call it again with the correct parameters.\n\n%s: %s",
				name, name, desc)}
			decided[i] = true
			continue
		}

		// Hooks first: a deterministic user rule outranks a prompt.
		if sess.hooks != nil {
			if out := sess.hooks.PreTool(ctx, name, params); out.Blocked {
				msg := "Blocked by pre_tool hook: " + out.Message
				sink.notice(fmt.Sprintf("[hook] %s: %s", name, out.Message))
				results[i] = skill.ToolResult{ToolCallID: tc.ID, Name: name, Output: msg, Err: fmt.Errorf("%s", msg)}
				decided[i] = true
				continue
			}
		}
		if sess.gate != nil {
			v := sess.gate.Check(ctx, name, params)
			if !v.Allowed {
				if v.Asked {
					sink.notice(fmt.Sprintf("[permission] denied: %s", permission.Summary(name, params)))
				}
				results[i] = skill.ToolResult{ToolCallID: tc.ID, Name: name, Output: v.Reason, Err: fmt.Errorf("permission denied")}
				decided[i] = true
				continue
			}
		}
		toRun = append(toRun, skill.ToolCallInfo{ID: tc.ID, Name: name, Params: params})
		runIdx = append(runIdx, i)
	}

	if ctx.Err() != nil {
		// Interrupted while a human was deciding — don't start work the
		// user just cancelled.
		for i := range results {
			if !decided[i] {
				results[i] = skill.ToolResult{ToolCallID: calls[i].ID, Name: calls[i].Function.Name,
					Output: "Cancelled: turn interrupted", Err: ctx.Err()}
			}
		}
	} else {
		ran := skill.ExecuteToolCalls(sess.registry, toRun)
		for k, r := range ran {
			results[runIdx[k]] = r
		}
	}

	for i := range results {
		r := &results[i]
		if !decided[i] && sess.hooks != nil {
			params, _ := calls[i].ParseArguments()
			if out := sess.hooks.PostTool(ctx, r.Name, params, r.Output); out.Blocked && out.Message != "" {
				r.Output = strings.TrimRight(r.Output, "\n") + "\n\n[post_tool hook] " + out.Message
			}
		}
		r.Output = skill.CapOutput(r.ToolCallID, r.Output)
		if sess.debug {
			fmt.Fprintf(os.Stderr, "\033[2m[%s -> %s]\033[0m\n", r.Name, brain.Preview(r.Output, 200))
		}
		sink.toolResult(*r)
		sess.appendMessage(brain.Message{
			Role: brain.RoleTool, Content: r.Output, ToolCallID: r.ToolCallID, Images: r.Images,
		})
	}
	return results
}

// abortNote is appended after a failed turn so the next user message
// isn't read as "carry on with the previous plan".
func abortNote(err error) brain.Message {
	return brain.Message{
		Role:    brain.RoleAssistant,
		Content: fmt.Sprintf("(Turn aborted at %s: %v. Reply 'continue' to resume or rephrase to start fresh.)", time.Now().Format("15:04"), err),
	}
}
