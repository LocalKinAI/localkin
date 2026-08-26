// harvest.go — implements the `kinclaw harvest` subcommand (v1.6+).
//
// Three commands, that's the user-facing surface:
//
//	kinclaw harvest                  scan + curator triage → stage yes/maybe
//	kinclaw harvest --review         show staged candidates
//	kinclaw harvest --accept ID      forge via coder → ./skills/<name>/
//	                                 (or fallback to ./skills/library/ on defer)
//
// The scan-time triage uses a strong KinClaw-aware LLM (curator soul,
// Kimi K2.6 by default) that knows the current ./skills/ inventory
// and decides yes/maybe/no per candidate. Forge happens only at
// --accept time, on one specific candidate the user chose. Cron mode
// (--no-judge) skips the LLM entirely and just keeps source caches
// warm.

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"flag"

	"github.com/LocalKinAI/kinclaw/pkg/harvest"
)

func runHarvest(argv []string) {
	// Pre-parse positional subcommand shorthand. Users naturally type
	// `kinclaw harvest review` and `kinclaw harvest accept <id>` (no
	// double-dash) to mirror the help text's "Three commands" listing.
	// Without this translation, the bare word falls through as an
	// ignored positional and harvest silently re-runs the full scan
	// — wasting ~3 min + LLM tokens on an "I just wanted to look"
	// command.
	if len(argv) > 0 && len(argv[0]) > 0 && argv[0][0] != '-' {
		switch argv[0] {
		case "review":
			argv = append([]string{"--review"}, argv[1:]...)
		case "accept":
			if len(argv) < 2 {
				fmt.Fprintln(os.Stderr, "harvest accept: missing skill id (expected: kinclaw harvest accept <source>/<skill-name>)")
				os.Exit(2)
			}
			argv = append([]string{"--accept", argv[1]}, argv[2:]...)
		}
	}

	fs := flag.NewFlagSet("harvest", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: kinclaw harvest [flags]

Read external agent skill libraries (configured in your harvest.toml
manifest), let the curator soul triage them against KinClaw's actual
skill inventory, stage candidates for review.

Three commands:

  kinclaw harvest                  scan + triage → stage yes/maybe candidates
  kinclaw harvest --review         list what's staged
  kinclaw harvest --accept ID      coder forges this one into ./skills/<name>/
                                   (or copies to ./skills/library/ if coder defers)

Manifest: %s

Flags:
`, harvest.DefaultManifestPath())
		fs.PrintDefaults()
	}

	manifestPath := fs.String("manifest", harvest.DefaultManifestPath(), "Path to harvest TOML manifest")
	sourceName := fs.String("source", "", "Run for one source only (manifest [[source]] name)")
	diff := fs.Bool("diff", false, "Dry-run — scan + triage but don't write to staging")
	review := fs.Bool("review", false, "List staged candidates and exit")
	accept := fs.String("accept", "", "Forge a staged candidate into ./skills/ (id: <source>/<skill-name>)")
	noJudge := fs.Bool("no-judge", false, "Skip the curator triage. Just count candidates per source (cron / cheap mode)")
	reJudge := fs.Bool("rejudge", false, "Ignore cached verdicts and judge every candidate again. Use after changing the curator soul or adding skills, since both make past decisions stale.")
	judgeWorkers := fs.Int("judge-workers", 8, "Parallel curator spawns. Higher = faster + more concurrent LLM calls (default 8). Set 1 for sequential.")
	skillsDir := fs.String("skills-dir", "skills", "--accept destination for forged skills (default ./skills)")
	libraryDir := fs.String("library-dir", "skills/library", "--accept fallback dir for coder-deferred candidates (default ./skills/library)")

	if err := fs.Parse(argv); err != nil {
		os.Exit(2)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest: cannot resolve $HOME: %v\n", err)
		os.Exit(1)
	}
	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest: cannot locate kinclaw binary: %v\n", err)
		os.Exit(1)
	}

	if *review {
		runHarvestReview(home)
		return
	}

	if *accept != "" {
		runHarvestAccept(context.Background(), home, bin, *skillsDir, *libraryDir, *accept)
		return
	}

	manifest, err := harvest.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest: %v\n", err)
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			fmt.Fprintf(os.Stderr, "\nCreate one based on harvest.example.toml at the repo root,\nor copy it to %s\n", *manifestPath)
		}
		os.Exit(1)
	}

	curatorSoul := ""
	if !*noJudge {
		curatorSoul = resolveSoulFile("curator.soul.md")
		if curatorSoul == "" {
			fmt.Fprintln(os.Stderr, "harvest: souls/curator.soul.md not found in ./souls/ or ~/.localkin/souls/.")
			fmt.Fprintln(os.Stderr, "         falling back to --no-judge mode (just count candidates).")
			*noJudge = true
		}
	}

	// Read current ./skills/ inventory once at run start. Curator gets
	// this in every per-candidate prompt so its yes/maybe/no decision
	// is grounded in actual skill state, not memory.
	inv, ierr := harvest.LoadInventory(*skillsDir)
	if ierr != nil {
		fmt.Fprintf(os.Stderr, "harvest: warning — couldn't read inventory at %s: %v\n", *skillsDir, ierr)
		inv = &harvest.SkillInventory{}
	}
	if !*noJudge {
		fmt.Fprintf(os.Stderr, "current inventory: %d skill(s) at %s\n\n", len(inv.Skills), *skillsDir)
	}

	// Past verdicts, so a nightly run only pays for what changed. Judging is
	// the entire cost of a harvest — without this, every run re-derives the
	// same answers for the same unchanged files.
	verdicts := harvest.LoadVerdictCache(home)
	if !*noJudge && verdicts.Len() > 0 {
		fmt.Fprintf(os.Stderr, "verdict cache: %d prior decision(s)%s\n",
			verdicts.Len(), map[bool]string{true: " (ignored: --rejudge)"}[*reJudge])
	}

	opts := harvest.Options{
		Home:            home,
		Verdicts:        verdicts,
		ReJudge:         *reJudge,
		KinclawBin:      bin,
		CuratorSoulPath: curatorSoul,
		SkipJudge:       *noJudge,
		Inventory:       inv,
		DryRun:          *diff,
		Out:             os.Stderr,
		JudgeWorkers:    *judgeWorkers,
	}

	ctx := context.Background()
	if *sourceName != "" {
		src := manifest.FindSource(*sourceName)
		if src == nil {
			fmt.Fprintf(os.Stderr, "harvest: source %q not in manifest\n", *sourceName)
			os.Exit(1)
		}
		r := harvest.RunSource(ctx, *src, opts)
		saveVerdicts(verdicts)
		printSummary([]harvest.Result{r}, *diff, *noJudge)
		return
	}

	results := make([]harvest.Result, 0, len(manifest.Sources))
	for _, s := range manifest.Sources {
		results = append(results, harvest.RunSource(ctx, s, opts))
	}
	saveVerdicts(verdicts)
	printSummary(results, *diff, *noJudge)
}

func runHarvestReview(home string) {
	staged, err := harvest.ListStaged(home)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest: list staged: %v\n", err)
		os.Exit(1)
	}
	if len(staged) == 0 {
		fmt.Println("No staged candidates. Run `kinclaw harvest` first.")
		return
	}

	fmt.Printf("Staged at %s\n\n", harvest.StagedRoot(home))
	for _, s := range staged {
		var mark string
		switch s.Verdict {
		case harvest.JudgeYes:
			mark = "✓"
		case harvest.JudgeMaybe:
			mark = "?"
		case harvest.JudgeReference:
			// Reference material lives outside staging, so this shouldn't
			// appear here — render it rather than falling through to "·".
			mark = "📎"
		case harvest.JudgeNo:
			mark = "✗" // shouldn't be in staging, but render gracefully
		default:
			mark = "·"
		}
		domain := ""
		if s.Domain != "" {
			domain = " [" + s.Domain + "]"
		}
		fmt.Printf("%s  %s/%s%s\n", mark, s.SourceName, s.SkillName, domain)
		fmt.Printf("    source : %s\n", s.SourceURL)
		fmt.Printf("    file   : %s\n", s.SkillRelPath)
		fmt.Printf("    reason : %s\n", s.Reason)
		fmt.Printf("    accept : kinclaw harvest --accept %s/%s\n\n", s.SourceName, s.SkillName)
	}
}

func runHarvestAccept(ctx context.Context, home, bin, skillsDir, libraryDir, skillID string) {
	coderSoul := resolveSoulFile("coder.soul.md")
	res, err := harvest.AcceptStaged(ctx, harvest.AcceptOptions{
		Home:          home,
		KinclawBin:    bin,
		CoderSoulPath: coderSoul,
		SkillsDir:     skillsDir,
		LibraryDir:    libraryDir,
		Out:           os.Stderr,
	}, skillID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest: %v\n", err)
		os.Exit(1)
	}
	switch res.Verdict {
	case harvest.AcceptForged:
		fmt.Printf("✓ forged %s → %s\n", skillID, res.DestPath)
		fmt.Printf("  Skill name (post-forge): %s\n", res.ForgedName)
		fmt.Printf("  %s\n", res.Reason)
		fmt.Printf("  Run kinclaw to pick it up — external skills auto-load from %s.\n", skillsDir)
	case harvest.AcceptLibrary:
		fmt.Printf("📜 deferred %s → %s\n", skillID, res.DestPath)
		fmt.Printf("  reason: %s\n", res.Reason)
		fmt.Printf("  Original markdown saved as inspiration; not loadable as a runnable skill.\n")
	case harvest.AcceptDuplicate:
		fmt.Printf("✗ duplicate: %s\n", res.DestPath)
		fmt.Printf("  %s\n", res.Reason)
		fmt.Printf("  Remove the existing skill first, then retry.\n")
		os.Exit(1)
	case harvest.AcceptError:
		fmt.Fprintf(os.Stderr, "✗ accept failed: %s\n", res.Reason)
		os.Exit(1)
	}
}

// resolveSoulFile finds a soul by name in the standard search path
// (./souls/<name> then ~/.localkin/souls/<name>). Returns absolute
// path on hit, empty string on miss.
func resolveSoulFile(name string) string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join("souls", name),
		filepath.Join(home, ".localkin", "souls", name),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return ""
}

func printSummary(results []harvest.Result, dryRun, skipJudge bool) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "── summary")

	var totalCand, totalYes, totalMaybe, totalNo, totalPending, totalErr int
	var totalRef, totalCached int
	for _, r := range results {
		totalCand += r.Candidates
		totalYes += len(r.Yes)
		totalMaybe += len(r.Maybe)
		totalNo += len(r.No)
		totalRef += len(r.Reference)
		totalCached += len(r.Cached)
		totalPending += len(r.Pending)
		totalErr += len(r.Errors)
	}
	fmt.Fprintf(os.Stderr, "  %d candidates across %d source(s)\n", totalCand, len(results))
	if totalYes > 0 {
		fmt.Fprintf(os.Stderr, "  ✓  %d yes (staged)\n", totalYes)
	}
	if totalMaybe > 0 {
		fmt.Fprintf(os.Stderr, "  ?  %d maybe (staged)\n", totalMaybe)
	}
	if totalNo > 0 {
		if totalRef > 0 {
			fmt.Fprintf(os.Stderr, "  📎 %d reference (archived at %s)\n",
				totalRef, harvest.ReferenceRoot(homeDir()))
		}
		if totalCached > 0 {
			fmt.Fprintf(os.Stderr, "  ⚡ %d reused from cache (not re-judged)\n", totalCached)
		}
		fmt.Fprintf(os.Stderr, "  ✗  %d no (dropped — curator says not useful)\n", totalNo)
	}
	if totalPending > 0 {
		hint := "rerun without --no-judge to triage them"
		fmt.Fprintf(os.Stderr, "  ⏸ %d pending (no triage) — %s\n", totalPending, hint)
	}
	if totalErr > 0 {
		fmt.Fprintf(os.Stderr, "  ⚠  %d source-level error(s) — license / clone / glob / curator failures\n", totalErr)
	}

	if dryRun {
		fmt.Fprintln(os.Stderr, "\n  --diff: nothing was written.")
		return
	}
	fmt.Fprintln(os.Stderr)
	if totalYes+totalMaybe > 0 {
		fmt.Fprintln(os.Stderr, "  Review:        kinclaw harvest --review")
		fmt.Fprintln(os.Stderr, "  Forge one:     kinclaw harvest --accept <source>/<skill-name>")
	}
}

// saveVerdicts persists the decision cache. A failure is reported but not
// fatal: the scan already happened and its staged output is on disk; losing
// the cache only costs a re-judge next time.
func saveVerdicts(v *harvest.VerdictCache) {
	if err := v.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "harvest: couldn't save verdict cache: %v\n", err)
	}
}
