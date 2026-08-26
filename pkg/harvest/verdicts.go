package harvest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// VerdictCache remembers what the curator already decided, so a scheduled
// harvest only pays for candidates it hasn't seen.
//
// Without this, every run re-judges everything: 161 candidates × ~5s and ~1k
// input tokens each, every night, to re-derive answers that haven't changed.
// The sources move slowly — a nightly run typically finds nothing new — so
// nearly all of that cost buys nothing.
//
// Keyed on a hash of the candidate's content, not its path. A renamed file
// with identical content should not be re-judged, and an edited file with the
// same name must be: the curator's answer is about what the skill *does*.
//
// "no" verdicts are cached too, and that is the point — those are 134 of the
// 161, and they are exactly what a path-based or staged-dir-based check would
// miss, since rejected candidates leave nothing on disk.
type VerdictCache struct {
	mu      sync.Mutex
	path    string
	Entries map[string]VerdictEntry `json:"entries"`
}

// VerdictEntry is one remembered decision.
type VerdictEntry struct {
	Verdict string `json:"verdict"` // yes / maybe / no
	Reason  string `json:"reason"`
	Name    string `json:"name"`   // candidate name at judging time, for logs
	Source  string `json:"source"` // which source it came from
	At      string `json:"at"`     // RFC3339, so a stale cache can be spotted
}

// VerdictCachePath is where the cache lives, alongside the staged candidates.
func VerdictCachePath(home string) string {
	return filepath.Join(StagedRoot(home), ".verdicts.json")
}

// CandidateKey is the content hash a verdict is stored under.
func CandidateKey(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:16])
}

// LoadVerdictCache reads the cache. A missing or corrupt file yields an empty
// cache rather than an error: the cost of a cache miss is re-judging, which is
// merely slow, while failing the whole harvest over an unreadable cache would
// turn a performance aid into a liability.
func LoadVerdictCache(home string) *VerdictCache {
	c := &VerdictCache{
		path:    VerdictCachePath(home),
		Entries: map[string]VerdictEntry{},
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var parsed VerdictCache
	if err := json.Unmarshal(data, &parsed); err != nil {
		return c
	}
	if parsed.Entries != nil {
		c.Entries = parsed.Entries
	}
	return c
}

// Get returns a remembered verdict, if any.
func (c *VerdictCache) Get(key string) (VerdictEntry, bool) {
	if c == nil {
		return VerdictEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.Entries[key]
	return e, ok
}

// Put records a verdict. Safe for the concurrent judge workers.
func (c *VerdictCache) Put(key string, e VerdictEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	c.Entries[key] = e
}

// Save writes the cache back. Best-effort: losing it costs a re-judge next
// run, which is not worth failing a completed harvest over.
func (c *VerdictCache) Save() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}

// Len reports how many decisions are remembered.
func (c *VerdictCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Entries)
}
