package skill

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapOutput_SmallPassesThrough(t *testing.T) {
	if got := CapOutput("id", "hello"); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestCapOutput_SpillsAndPointsAtFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	big := strings.Repeat("line of tool output 中文也算\n", 3000) // ~100KB
	got := CapOutput("call_abc/../x", big)
	if len(got) > MaxToolOutputBytes+600 {
		t.Fatalf("capped output too long: %d", len(got))
	}
	if !strings.Contains(got, "Full output saved to") {
		t.Fatalf("missing spill note: %q", got[len(got)-300:])
	}
	// The path in the note exists and holds the full text.
	i := strings.Index(got, "saved to ")
	rest := got[i+len("saved to "):]
	path := rest[:strings.Index(rest, " —")]
	data, err := os.ReadFile(path)
	if err != nil || string(data) != big {
		t.Fatalf("spill file missing or wrong: %v", err)
	}
	if strings.Contains(path, "..") {
		t.Fatalf("call id must be sanitized in the filename: %s", path)
	}
	if !utf8.ValidString(got) {
		t.Fatal("cut a rune in half")
	}
}
