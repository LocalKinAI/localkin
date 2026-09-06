package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiff_Basic(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	new := "a\nb\nc\nD\ne\nf\ng\nh\n"
	d := UnifiedDiff(old, new)
	if !strings.Contains(d, diffRed+"-d"+diffReset) || !strings.Contains(d, diffGreen+"+D"+diffReset) {
		t.Fatalf("missing change lines:\n%s", d)
	}
	// Context is 3 lines: "a" is 3 above "d"? a,b,c are the 3 before; h is beyond 3 after (e,f,g).
	if strings.Contains(d, " h") {
		t.Fatalf("h is outside the 3-line context:\n%s", d)
	}
	if UnifiedDiff("same", "same") != "" {
		t.Fatal("identical content must produce no diff")
	}
}

func TestUnifiedDiff_NewFileAndCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("line\n")
	}
	d := UnifiedDiff("", sb.String())
	if strings.Contains(d, diffRed+"-") {
		t.Fatalf("a new file must not show a removed line:\n%s", d[:200])
	}
	if !strings.Contains(d, "more diff lines not shown") {
		t.Fatalf("expected cap note, got %d bytes", len(d))
	}
	if strings.Count(d, "\n") > diffMaxOutLines+5 {
		t.Fatalf("output not capped: %d lines", strings.Count(d, "\n"))
	}
}

func TestFileSkills_WorkspaceAndDiff(t *testing.T) {
	ws := t.TempDir()
	w := NewFileWriteSkill()
	w.(WorkspaceAware).SetWorkspace(ws)
	out, err := w.Execute(map[string]string{"path": "notes/a.txt", "content": "one\ntwo\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes", "a.txt")); err != nil {
		t.Fatalf("relative path should land in the workspace: %v", err)
	}
	if !strings.Contains(out, "+one") {
		t.Fatalf("new file should show additions:\n%s", out)
	}
	e := NewFileEditSkill()
	e.(WorkspaceAware).SetWorkspace(ws)
	out, err = e.Execute(map[string]string{"path": "notes/a.txt", "old_text": "two", "new_text": "TWO"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Edited ") || !strings.Contains(out, "-two") || !strings.Contains(out, "+TWO") {
		t.Fatalf("edit output should carry the diff:\n%s", out)
	}
	r := NewFileReadSkill()
	r.(WorkspaceAware).SetWorkspace(ws)
	got, err := r.Execute(map[string]string{"path": "notes/a.txt"})
	if err != nil || !strings.Contains(got, "TWO") {
		t.Fatalf("read via workspace: %v %q", err, got)
	}
}

func TestToolSearch_LoadsMatches(t *testing.T) {
	r := NewRegistry()
	r.Register(&mockSkill{name: "record", desc: "Record the screen to an mp4 video with audio."})
	r.Register(&mockSkill{name: "tts", desc: "Speak text aloud."})
	r.Register(&mockSkill{name: "web", desc: "Drive a Playwright browser: click, type, extract."})
	hidden := map[string]bool{"record": true, "tts": true, "web": true}
	deferred := func() []string {
		var out []string
		for n := range hidden {
			out = append(out, n)
		}
		return out
	}
	load := func(names []string) {
		for _, n := range names {
			delete(hidden, n)
		}
	}
	ts := NewToolSearchSkill(r, deferred, load)
	out, err := ts.Execute(map[string]string{"query": "record a video"})
	if err != nil || !strings.Contains(out, "Loaded 1 skill(s): record") {
		t.Fatalf("%v %q", err, out)
	}
	if hidden["record"] {
		t.Fatal("record should be loaded")
	}
	out, _ = ts.Execute(map[string]string{"load": "web,tts"})
	if !strings.Contains(out, "web") || !strings.Contains(out, "tts") || len(hidden) != 0 {
		t.Fatalf("explicit load: %q hidden=%v", out, hidden)
	}
	out, _ = ts.Execute(map[string]string{"query": "anything"})
	if !strings.Contains(out, "No deferred skills") {
		t.Fatalf("%q", out)
	}
	idx := DeferredIndex(r, []string{"record", "web"})
	if !strings.Contains(idx, "- record — Record the screen") || !strings.Contains(idx, "tool_search") {
		t.Fatalf("index: %q", idx)
	}
}
