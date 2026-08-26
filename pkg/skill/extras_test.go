package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEffectiveEnableMerges — extras add to the soul's list, never replace it.
func TestEffectiveEnableMerges(t *testing.T) {
	got := EffectiveEnable([]string{"screen", "shell"}, []string{"pdf", "mcp_*"})
	want := []string{"screen", "shell", "pdf", "mcp_*"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestEmptySoulListStaysPermissive is the edge case that would silently break
// an agent: an empty skills.enable means "expose everything", so adding an
// extra must not convert it into a one-item allowlist.
func TestEmptySoulListStaysPermissive(t *testing.T) {
	if got := EffectiveEnable(nil, []string{"pdf"}); got != nil {
		t.Errorf("got %v, want nil (permissive); extras narrowed an open soul", got)
	}
}

func TestEffectiveEnableDeduplicates(t *testing.T) {
	got := EffectiveEnable([]string{"screen", "pdf"}, []string{"pdf", "pdf"})
	if len(got) != 2 {
		t.Errorf("got %v, want 2 unique entries", got)
	}
}

func TestExtrasRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill_extras.json")

	e := LoadExtras(path)
	if len(e.For("Pilot")) != 0 {
		t.Fatal("fresh overlay was not empty")
	}
	e.BySoul["Pilot"] = []string{"pdf", "mcp_*"}
	if err := e.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	back := LoadExtras(path)
	if len(back.For("Pilot")) != 2 {
		t.Errorf("got %v after reload, want 2 entries", back.For("Pilot"))
	}
	// Per-soul isolation: one soul's extras must not leak to another.
	if len(back.For("Coder")) != 0 {
		t.Errorf("extras leaked across souls: %v", back.For("Coder"))
	}
}

// A corrupt overlay must not take down skill loading.
func TestCorruptExtrasDegradeToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill_extras.json")
	if err := writeFile(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	e := LoadExtras(path)
	if e == nil || len(e.BySoul) != 0 {
		t.Errorf("corrupt overlay yielded %v, want an empty overlay", e)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
