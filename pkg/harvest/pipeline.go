// Package harvest implements `kinclaw harvest` — pull external agent
// skill libraries, ask the curator specialist to judge each candidate
// against KinClaw's actual skill inventory, stage yes/maybe survivors
// for human review.
//
// v1.6 reframed harvest from "forge runnable skills at scan time" to
// "scan + lightweight LLM triage". The heavy work (forge → real exec
// SKILL.md) only happens at `--accept` time, on one specific
// candidate the user picked, never in bulk. See AcceptStaged.
//
// Pipeline (per candidate, fast):
//
//	read content + frontmatter
//	  → spawn curator with (KinClaw skill inventory + candidate excerpt)
//	  → curator returns verdict: yes | maybe | no + reason + domain
//	  → if yes/maybe, stage original markdown + judge.txt + meta.txt
//	  → if no, drop (counted in summary)
package harvest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/soul"
	"gopkg.in/yaml.v3"
)

// Options configures one harvest pipeline run.
type Options struct {
	Home            string          // user home; staging + cache live under here
	KinclawBin      string          // self-path for spawning curator (and coder at accept-time)
	CuratorSoulPath string          // resolved souls/curator.soul.md; empty disables judging
	SkipJudge       bool            // --no-judge: just count candidates, no curator spawn (cron / cheap mode)
	Inventory       *SkillInventory // current ./skills/ state, injected into curator prompts
	// Verdicts remembers past decisions so a scheduled run only pays for
	// candidates it has not seen. nil disables caching entirely.
	Verdicts *VerdictCache
	// ReJudge ignores the cache — for when the curator soul or the skill
	// inventory changed, which makes every past verdict potentially stale.
	ReJudge      bool
	DryRun       bool          // --diff: scan + judge but don't write to staging
	Out          io.Writer     // human-readable progress, default os.Stderr
	CloneTimeout time.Duration // per-source git clone/pull deadline (default 120s)
	JudgeWorkers int           // parallel curator spawns (default 8)
}

// Result is the per-source outcome of a pipeline run.
type Result struct {
	SourceName string
	Candidates int      // total files matched by skill_paths
	Yes        []string // staged with verdict=yes
	Maybe      []string // staged with verdict=maybe
	No         []string // dropped — curator said no
	Reference  []string // archived for reading, not forged
	Cached     []string // verdict reused from a previous run
	Pending    []string // candidates we'd judge but couldn't (--no-judge or curator unavailable)
	Errors     []string // source-level errors (clone / license / glob)
}

// RunSource executes the pipeline against one [[source]] from the
// manifest. Always returns a Result, even on partial failure.
func RunSource(ctx context.Context, src Source, opts Options) Result {
	r := Result{SourceName: src.Name}
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	if opts.CloneTimeout == 0 {
		opts.CloneTimeout = 120 * time.Second
	}

	fmt.Fprintf(out, "── %s\n", src.Name)
	cloneCtx, cancel := withTimeout(ctx, opts.CloneTimeout)
	defer cancel()
	repoDir, err := PullSource(cloneCtx, src, opts.Home)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("clone/pull: %v", err))
		fmt.Fprintf(out, "   ✗ clone/pull failed: %v\n", err)
		return r
	}

	license := FindLicense(repoDir)
	if !LicenseAllowed(license, src.LicenseAllow) {
		detected := license
		if detected == "" {
			detected = "(none detected)"
		}
		msg := fmt.Sprintf("license %s not in allowlist %v", detected, src.LicenseAllow)
		r.Errors = append(r.Errors, msg)
		fmt.Fprintf(out, "   ✗ license: %s — not in allowlist %v\n", detected, src.LicenseAllow)
		switch detected {
		case "proprietary":
			fmt.Fprintf(out, "      → proprietary (\"All rights reserved\"). Set license_allow=[\"*\"] to opt in.\n")
		case "(none detected)":
			fmt.Fprintf(out, "      → no LICENSE file found. Set license_allow=[\"*\"] if you've inspected the repo.\n")
		default:
			fmt.Fprintf(out, "      → add %q to license_allow for this source to include it.\n", detected)
		}
		return r
	}
	if license != "" {
		fmt.Fprintf(out, "   license: %s ✓\n", license)
	} else {
		fmt.Fprintf(out, "   license: (none detected, allowed by *)\n")
	}

	matches, err := globFiles(repoDir, src.SkillPaths)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("glob: %v", err))
		return r
	}
	r.Candidates = len(matches)
	if len(matches) == 0 {
		fmt.Fprintf(out, "   matched 0 candidates under skill_paths=%v\n", src.SkillPaths)
		fmt.Fprintf(out, "      → check the manifest's skill_paths globs against the repo's actual layout.\n")
		return r
	}
	fmt.Fprintf(out, "   matched %d candidate(s)\n", len(matches))

	if !opts.DryRun {
		if err := CleanSourceStage(opts.Home, src.Name); err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("clean staged: %v", err))
		}
	}

	// Concurrency: curator spawns are LLM round-trips, ~10-15s each.
	// Sequential = brutal (85 × ~15s = 21 min per source). With 8
	// workers it drops to ~3 min. r and out are mutated from goroutines
	// — guard with a single mutex (output reordering is fine; user
	// reads candidate name on each line so cross-correlation works).
	workers := opts.JudgeWorkers
	if workers <= 0 {
		workers = 8
	}
	if opts.SkipJudge || opts.CuratorSoulPath == "" {
		// Cheap mode — no LLM calls, sequential is fine and avoids
		// goroutine overhead entirely.
		workers = 1
	}

	var mu sync.Mutex
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, path := range matches {
		path := path
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rel, _ := filepath.Rel(repoDir, path)
			rel = filepath.ToSlash(rel)
			processCandidate(ctx, &r, &mu, src, opts, repoDir, path, rel, out)
		}()
	}
	wg.Wait()

	renderSourceSummary(out, &r, opts.SkipJudge)
	return r
}

// processCandidate is the v1.6 per-file body. Single path: extract
// name+description+body excerpt, ask curator (unless --no-judge),
// stage based on verdict. No forge, no critic, no special branching
// between exec-style and procedural-style — they all flow through
// the same triage.
//
// Called concurrently from the RunSource worker pool. mu guards
// every r-write and every out-write so concurrent goroutines don't
// race or interleave each other's output mid-line. The curator
// LLM call (Judge) is the expensive bit; it runs OUTSIDE the lock.
func processCandidate(
	ctx context.Context,
	r *Result,
	mu *sync.Mutex,
	src Source,
	opts Options,
	repoDir, path, rel string,
	out io.Writer,
) {
	content, err := os.ReadFile(path)
	if err != nil {
		mu.Lock()
		r.Errors = append(r.Errors, fmt.Sprintf("%s: read: %v", rel, err))
		mu.Unlock()
		return
	}

	cand := extractCandidate(content, rel, src)

	// --no-judge mode: just record presence, don't spawn curator.
	// Used by cron / CI to keep source caches warm without LLM cost.
	if opts.SkipJudge || opts.CuratorSoulPath == "" || opts.KinclawBin == "" {
		mu.Lock()
		r.Pending = append(r.Pending, cand.Name)
		mu.Unlock()
		return
	}

	// Reuse a past decision when this exact content was judged before.
	//
	// Keyed on content, so an edited skill is re-judged and a renamed one is
	// not. Cached "no" verdicts matter most: they are the large majority, and
	// they leave nothing on disk, so nothing else could tell us we had already
	// looked at them.
	key := CandidateKey(content)
	if !opts.ReJudge {
		if prev, ok := opts.Verdicts.Get(key); ok {
			mu.Lock()
			r.Cached = append(r.Cached, cand.Name)
			switch prev.Verdict {
			case string(JudgeYes):
				r.Yes = append(r.Yes, cand.Name)
			case string(JudgeMaybe):
				r.Maybe = append(r.Maybe, cand.Name)
			case string(JudgeReference):
				r.Reference = append(r.Reference, cand.Name)
			default:
				r.No = append(r.No, cand.Name)
			}
			mu.Unlock()
			return
		}
	}

	// Judge is the slow path (LLM round-trip, ~10-15s). Outside the
	// lock so peer workers can also call Judge concurrently.
	res, err := Judge(ctx, opts.KinclawBin, opts.CuratorSoulPath, opts.Inventory, cand)

	mu.Lock()
	defer mu.Unlock()

	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("%s: judge: %v", rel, err))
		fmt.Fprintf(out, "   ⚠ %s — judge failed: %v\n", rel, err)
		return
	}

	opts.Verdicts.Put(key, VerdictEntry{
		Verdict: string(res.Verdict),
		Reason:  res.Reason,
		Name:    cand.Name,
		Source:  src.Name,
	})

	switch res.Verdict {
	case JudgeYes:
		r.Yes = append(r.Yes, cand.Name)
		fmt.Fprintf(out, "   ✓ %s — %s\n", cand.Name, res.Reason)
		if !opts.DryRun {
			if _, serr := StageJudged(opts.Home, src, cand, string(content), res); serr != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: stage: %v", rel, serr))
			}
		}
	case JudgeMaybe:
		r.Maybe = append(r.Maybe, cand.Name)
		fmt.Fprintf(out, "   ? %s — %s\n", cand.Name, res.Reason)
		if !opts.DryRun {
			if _, serr := StageJudged(opts.Home, src, cand, string(content), res); serr != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: stage: %v", rel, serr))
			}
		}
	case JudgeReference:
		// Archived, never forged. These are the methodology and reference
		// documents — skill-creator, mcp-builder, brand guidelines — that are
		// genuinely not expressible as `command + args`, which is why they are
		// not staged, but whose ideas are the reason for reading an external
		// skill library at all. Dropping them was the pipeline losing exactly
		// what it was meant to harvest.
		r.Reference = append(r.Reference, cand.Name)
		fmt.Fprintf(out, "   📎 %s — %s\n", cand.Name, res.Reason)
		if !opts.DryRun {
			if _, serr := StageReference(opts.Home, src, cand, string(content), res); serr != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: reference: %v", rel, serr))
			}
		}
	case JudgeNo:
		r.No = append(r.No, cand.Name)
		fmt.Fprintf(out, "   ✗ %s — %s\n", cand.Name, res.Reason)
	case JudgeUnparseable:
		r.Errors = append(r.Errors, fmt.Sprintf("%s: curator response unparseable", rel))
		fmt.Fprintf(out, "   ⚠ %s — curator response unparseable; raw output kept\n", rel)
	}
}

// extractCandidate pulls the bits curator needs — name, description,
// body excerpt — from a candidate file. Tries YAML frontmatter first
// (Anthropic / Hermes / Cursor / KinClaw all use it); falls back to
// the file's parent dir name + first markdown paragraph for files
// without frontmatter (`.cursorrules`, plain `.md` rules collections).
func extractCandidate(content []byte, rel string, src Source) JudgeCandidate {
	c := JudgeCandidate{
		SourceURL:    src.URL,
		SourceName:   src.Name,
		SkillRelPath: rel,
	}

	rawYAML, body, err := soul.SplitFrontmatter(content)
	if err == nil && len(rawYAML) > 0 {
		var fm map[string]any
		if yaml.Unmarshal(rawYAML, &fm) == nil {
			if n, ok := fm["name"].(string); ok {
				c.Name = n
			}
			if d, ok := fm["description"].(string); ok {
				c.Description = d
			}
		}
		c.BodyExcerpt = strings.TrimSpace(string(body))
	}
	if c.BodyExcerpt == "" {
		c.BodyExcerpt = strings.TrimSpace(string(content))
	}

	if c.Name == "" {
		// Use parent dir name (Anthropic / Hermes nest as
		// <topic>/<name>/SKILL.md) with file base as fallback.
		dir := filepath.Dir(rel)
		if dir != "" && dir != "." {
			c.Name = filepath.Base(dir)
		} else {
			base := filepath.Base(rel)
			c.Name = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}
	if c.Description == "" {
		// First paragraph of body, capped to 200 chars.
		body := c.BodyExcerpt
		if i := strings.Index(body, "\n\n"); i > 0 {
			body = body[:i]
		}
		body = strings.TrimSpace(body)
		if len(body) > 200 {
			body = body[:197] + "..."
		}
		c.Description = body
	}
	return c
}

func renderSourceSummary(out io.Writer, r *Result, skipJudge bool) {
	parts := []string{}
	if len(r.Yes) > 0 {
		parts = append(parts, fmt.Sprintf("%d ✓", len(r.Yes)))
	}
	if len(r.Maybe) > 0 {
		parts = append(parts, fmt.Sprintf("%d ?", len(r.Maybe)))
	}
	if len(r.No) > 0 {
		parts = append(parts, fmt.Sprintf("%d ✗", len(r.No)))
	}
	if len(r.Pending) > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", len(r.Pending)))
	}
	if len(parts) == 0 {
		parts = []string{"0 candidates"}
	}
	fmt.Fprintf(out, "   ── %s\n", strings.Join(parts, ", "))

	if skipJudge && len(r.Pending) > 0 {
		fmt.Fprintf(out, "      Run without --no-judge to triage these via curator.\n")
	}
}
