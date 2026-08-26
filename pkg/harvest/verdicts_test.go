package harvest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateKeyIsContentBased(t *testing.T) {
	a := []byte("---\nname: foo\n---\nbody")
	b := []byte("---\nname: foo\n---\nbody")
	c := []byte("---\nname: foo\n---\nbody changed")

	if CandidateKey(a) != CandidateKey(b) {
		t.Error("identical content produced different keys")
	}
	// The whole point of hashing content rather than paths: an edited skill
	// must be re-judged even though its name and location didn't change.
	if CandidateKey(a) == CandidateKey(c) {
		t.Error("edited content reused the same key; it would never be re-judged")
	}
}

func TestVerdictCacheRoundTrip(t *testing.T) {
	home := t.TempDir()
	c := LoadVerdictCache(home)
	if c.Len() != 0 {
		t.Fatalf("fresh cache had %d entries", c.Len())
	}

	key := CandidateKey([]byte("some skill"))
	c.Put(key, VerdictEntry{Verdict: "no", Reason: "duplicates existing", Name: "dup", Source: "src"})
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded := LoadVerdictCache(home)
	entry, ok := reloaded.Get(key)
	if !ok {
		t.Fatal("verdict did not survive a reload")
	}
	if entry.Verdict != "no" || entry.Name != "dup" {
		t.Errorf("got %+v, want verdict=no name=dup", entry)
	}
	if entry.At == "" {
		t.Error("timestamp not stamped; a stale cache would be undetectable")
	}
}

// TestNoVerdictsAreCached is the case that matters most for cost: rejections
// are the large majority and leave nothing on disk, so without caching them
// every run re-judges them forever.
func TestNoVerdictsAreCached(t *testing.T) {
	home := t.TempDir()
	c := LoadVerdictCache(home)
	key := CandidateKey([]byte("rejected skill"))
	c.Put(key, VerdictEntry{Verdict: "no", Reason: "not a shell-exec wrapper"})
	_ = c.Save()

	if _, ok := LoadVerdictCache(home).Get(key); !ok {
		t.Error("a 'no' verdict was not remembered")
	}
}

// TestCorruptCacheDegradesGracefully — an unreadable cache must cost a
// re-judge, not fail the harvest.
func TestCorruptCacheDegradesGracefully(t *testing.T) {
	home := t.TempDir()
	path := VerdictCachePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := LoadVerdictCache(home)
	if c == nil {
		t.Fatal("corrupt cache returned nil instead of an empty cache")
	}
	if c.Len() != 0 {
		t.Errorf("corrupt cache reported %d entries", c.Len())
	}
	// And it must still be usable afterwards.
	c.Put("k", VerdictEntry{Verdict: "yes"})
	if err := c.Save(); err != nil {
		t.Errorf("could not recover a corrupt cache: %v", err)
	}
}

// TestNilCacheIsSafe — Options.Verdicts may be nil (callers that don't want
// caching); the pipeline calls Get/Put unconditionally.
func TestNilCacheIsSafe(t *testing.T) {
	var c *VerdictCache
	if _, ok := c.Get("anything"); ok {
		t.Error("nil cache reported a hit")
	}
	c.Put("k", VerdictEntry{}) // must not panic
	if err := c.Save(); err != nil {
		t.Errorf("nil cache Save returned %v", err)
	}
	if c.Len() != 0 {
		t.Error("nil cache reported entries")
	}
}
