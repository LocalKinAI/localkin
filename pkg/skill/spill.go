package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxToolOutputBytes caps what one tool result contributes to the
// model's context. Skills already truncate at 128KB, but 128KB is a
// third of a 100K-token window — three such results and the model has
// forgotten the task. Anything beyond the cap is written to a file and
// the result ends with the path, so the model can file_read the rest
// if it actually needs it. Same shape as Claude Code's "output too
// large, saved to …" behaviour.
const MaxToolOutputBytes = 24 * 1024

// spillMaxAge is how long spilled outputs are kept.
const spillMaxAge = 7 * 24 * time.Hour

// SpillDir is where oversized tool outputs are written.
func SpillDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kinclaw-tool-results")
	}
	return filepath.Join(home, ".kinclaw", "tool-results")
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// CapOutput returns out unchanged when it fits, otherwise the head of
// it plus a note saying how much was cut and where the full text went.
// callID names the file; it comes from the model's tool_call id.
// Writing failures degrade to plain truncation — the note then says so.
func CapOutput(callID, out string) string {
	if len(out) <= MaxToolOutputBytes {
		return out
	}
	total := len(out)
	head := out[:MaxToolOutputBytes]
	// Don't cut a multi-byte rune in half.
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	if i := strings.LastIndexByte(head, '\n'); i > MaxToolOutputBytes/2 {
		head = head[:i]
	}

	dir := SpillDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return head + fmt.Sprintf("\n…[output truncated: %d of %d bytes shown; could not save the rest: %v]", len(head), total, err)
	}
	pruneSpill(dir)
	name := unsafeID.ReplaceAllString(callID, "_")
	if name == "" {
		name = "call"
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.txt", time.Now().Format("20060102-150405"), name))
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return head + fmt.Sprintf("\n…[output truncated: %d of %d bytes shown; could not save the rest: %v]", len(head), total, err)
	}
	return head + fmt.Sprintf(
		"\n…[output truncated: %d of %d bytes shown. Full output saved to %s — use file_read on it (or shell grep) only if you need the rest.]",
		len(head), total, path)
}

// pruneSpill deletes spilled files older than spillMaxAge. Best-effort
// and cheap: one ReadDir per spill, which is itself rare.
func pruneSpill(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-spillMaxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
