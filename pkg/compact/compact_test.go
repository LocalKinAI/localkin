package compact

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LocalKinAI/kinclaw/pkg/brain"
)

type fakeBrain struct {
	reply string
	seen  []brain.Message
}

func (f *fakeBrain) Chat(_ context.Context, msgs []brain.Message, _ []json.RawMessage, _ brain.StreamFunc) (*brain.ChatResult, error) {
	f.seen = msgs
	return &brain.ChatResult{Content: f.reply}, nil
}

func tc(id string) brain.ToolCall {
	t := brain.ToolCall{ID: id, Type: "function"}
	t.Function.Name = "ui"
	t.Function.Arguments = `{"action":"tree"}`
	return t
}

// turn is user → assistant(tool) → tool → assistant, 4 messages.
func turn(n string) []brain.Message {
	return []brain.Message{
		{Role: brain.RoleUser, Content: "task " + n},
		{Role: brain.RoleAssistant, ToolCalls: []brain.ToolCall{tc("c" + n)}},
		{Role: brain.RoleTool, ToolCallID: "c" + n, Content: strings.Repeat("x", 3000)},
		{Role: brain.RoleAssistant, Content: "done " + n},
	}
}

func TestSplitPoint_LandsOnUserMessage(t *testing.T) {
	var h []brain.Message
	for _, n := range []string{"1", "2", "3", "4"} {
		h = append(h, turn(n)...)
	}
	// 16 messages, keep 6 → naive split at 10 (a tool result); must
	// walk back to index 8 (user "task 3").
	i := SplitPoint(h, 6)
	if i != 8 || h[i].Role != brain.RoleUser || h[i].Content != "task 3" {
		t.Fatalf("split=%d role=%s content=%q", i, h[i].Role, h[i].Content)
	}
}

func TestSplitPoint_TooShort(t *testing.T) {
	if got := SplitPoint(turn("1"), 8); got != 0 {
		t.Fatalf("expected 0 for short history, got %d", got)
	}
}

func TestNeeded_UsesProviderUsageFirst(t *testing.T) {
	var h []brain.Message
	for _, n := range []string{"1", "2", "3", "4"} {
		h = append(h, turn(n)...)
	}
	cfg := Config{ContextLength: 10000, Threshold: 0.75, KeepRecent: 4}
	if Needed(1000, "", h, cfg) {
		t.Fatal("1000/10000 should not need compaction")
	}
	if !Needed(8000, "", h, cfg) {
		t.Fatal("8000/10000 should need compaction")
	}
	// No usage reported → estimate. 4 × 3000 chars of tool output ≈
	// 3000+ tokens against a 3000 window → compaction.
	if !Needed(0, "", h, Config{ContextLength: 3000, KeepRecent: 4}) {
		t.Fatal("estimate path should trigger")
	}
}

func TestCompact_ProducesSummaryPlusTail(t *testing.T) {
	var h []brain.Message
	for _, n := range []string{"1", "2", "3", "4"} {
		h = append(h, turn(n)...)
	}
	fb := &fakeBrain{reply: "1. Goals — tasks 1-2.\n3. Artifacts — /tmp/a.txt"}
	out, res, err := Compact(context.Background(), fb, h, Config{ContextLength: 100000, KeepRecent: 6})
	if err != nil {
		t.Fatal(err)
	}
	if !IsSummary(out[0]) {
		t.Fatalf("first message should be the summary, got %q", out[0].Content)
	}
	if out[1].Role != brain.RoleAssistant {
		t.Fatalf("second message should be the ack, got %s", out[1].Role)
	}
	// Kept tail starts at "task 3" (index 8 of 16) → 8 kept + 2.
	if res.Kept != 8 || res.Summarized != 8 || len(out) != 10 {
		t.Fatalf("kept=%d summarized=%d len=%d", res.Kept, res.Summarized, len(out))
	}
	if res.AfterTokens >= res.BeforeTokens {
		t.Fatalf("compaction should shrink: before=%d after=%d", res.BeforeTokens, res.AfterTokens)
	}
	// Summarizer saw a transcript, not raw messages with tool calls.
	if len(fb.seen) != 2 || !strings.Contains(fb.seen[1].Content, "assistant → ui(") {
		t.Fatalf("unexpected summarizer input: %+v", fb.seen)
	}
	// Sanitized: the new history must still be provider-valid.
	if s := brain.SanitizeHistory(out); len(s) != len(out) {
		t.Fatalf("compacted history not provider-valid: %d vs %d", len(s), len(out))
	}
}

func TestTranscript_DropsOldestWhenOverBudget(t *testing.T) {
	var h []brain.Message
	for _, n := range []string{"1", "2", "3", "4", "5", "6"} {
		h = append(h, turn(n)...)
	}
	txt, omitted := Transcript(h, 800)
	if omitted == 0 {
		t.Fatal("expected some messages omitted under a tight budget")
	}
	if !strings.Contains(txt, "done 6") {
		t.Fatal("newest message must survive")
	}
}
