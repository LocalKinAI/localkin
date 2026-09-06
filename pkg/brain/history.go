package brain

import (
	"strings"
	"unicode"
)

// SanitizeHistory repairs a message slice so every provider will
// accept it as a conversation:
//
//   - a tool result must directly answer a tool call in the preceding
//     assistant message (Claude: "tool_result without tool_use" →
//     HTTP 400; OpenAI-compatible: "tool message must follow
//     tool_calls" → HTTP 400);
//   - every tool call must be answered before the next non-tool
//     message (Claude: "tool_use ids were found without tool_result");
//   - the conversation cannot open on a tool result or an assistant
//     turn.
//
// Histories get into these states in two ways: LoadHistory takes the
// most recent N rows, which can begin in the middle of a tool group,
// and an interrupted turn can persist a tool_call whose results never
// came back. Dropping the incomplete fragments loses a little context;
// leaving them in loses the whole session to a 400 on every retry,
// which is the failure that used to need "New session" to clear.
//
// Orphaned tool calls are dropped along with their partial results
// rather than back-filled with synthetic errors: an invented result
// would teach the model a failure that never happened.
func SanitizeHistory(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	i := 0
	// Leading fragments: skip everything until the first user turn.
	for i < len(msgs) && msgs[i].Role != RoleUser {
		i++
	}
	for i < len(msgs) {
		m := msgs[i]
		switch {
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0:
			// Collect the results that immediately follow.
			want := make(map[string]bool, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				want[tc.ID] = true
			}
			j := i + 1
			var results []Message
			for j < len(msgs) && msgs[j].Role == RoleTool {
				if want[msgs[j].ToolCallID] {
					results = append(results, msgs[j])
					delete(want, msgs[j].ToolCallID)
				}
				j++
			}
			if len(want) == 0 {
				out = append(out, m)
				out = append(out, results...)
			}
			// Incomplete group: drop the call and its partial results.
			i = j
		case m.Role == RoleTool:
			// A result with no preceding call — orphan, drop.
			i++
		default:
			out = append(out, m)
			i++
		}
	}
	// A trailing assistant message with tool calls is only kept if its
	// results are present (handled above); nothing else to trim.
	return out
}

// EstimateTokens approximates the prompt size of a message slice when
// the provider hasn't reported usage yet. It counts CJK characters as
// one token each and Latin text at roughly four characters per token,
// plus a fixed per-message overhead for role/framing tokens. The old
// words×1.3 heuristic undercounted Chinese text by an order of
// magnitude — there are no spaces to split on — which made the /info
// number meaningless for exactly the users kinclaw is built for.
func EstimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 // role + separators
		total += EstimateText(m.Content)
		for _, tc := range m.ToolCalls {
			total += EstimateText(tc.Function.Name) + EstimateText(tc.Function.Arguments)
		}
		// An inline image costs on the order of a thousand tokens on
		// vision models; count it so screenshot-heavy turns show up.
		total += 1000 * len(m.Images)
	}
	return total
}

// EstimateText is the per-string half of EstimateTokens.
func EstimateText(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// Preview returns the first n runes of s on one line, for logs.
func Preview(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
