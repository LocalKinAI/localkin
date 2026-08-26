package harvest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LocalKinAI/kinclaw/pkg/skill"
)

// StagedRoot is the on-disk root for staged candidates.
//
// Layout (v1.6+):
//
//	~/.kinclaw/harvest/staged/<source>/<candidate>/
//	  ├── original.md         (raw content of the external SKILL.md)
//	  ├── judge.txt           (curator's full response — verdict / reason / domain)
//	  └── meta.txt            (source url / file path / staged_at / verdict)
//
// Forge does NOT happen at stage time. The original markdown sits
// here until the user runs `kinclaw harvest --accept ID`, which
// spawns coder, attempts to forge a real KinClaw SKILL.md, routes
// the result to ./skills/ (success) or ./skills/library/ (defer).
func StagedRoot(home string) string {
	return filepath.Join(home, ".kinclaw", "harvest", "staged")
}

// StagedSkill describes one staged candidate as exposed to
// `harvest --review`.
type StagedSkill struct {
	SourceName   string
	SkillName    string
	StagePath    string
	Verdict      JudgeVerdict
	Reason       string
	Domain       string
	SourceURL    string
	SkillRelPath string
	StagedAt     time.Time
}

// CleanSourceStage removes any prior staged candidates for sourceName.
// Called at the start of each RunSource so re-runs reflect current
// state cleanly without stale entries from earlier versions.
func CleanSourceStage(home, sourceName string) error {
	dir := filepath.Join(StagedRoot(home), sourceName)
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// StageJudged writes a candidate the curator gave verdict=yes/maybe
// to the staging dir. Original markdown is preserved verbatim — the
// pipeline does NOT modify it. Curator's full response is saved to
// judge.txt for human review.
func StageJudged(home string, src Source, cand JudgeCandidate, originalContent string, judge *JudgeResult) (string, error) {
	// Sanitized for the same reason as StageReference: cand.Name is
	// third-party frontmatter being used as a path component.
	safe := sanitizeName(cand.Name)
	if safe == "" {
		safe = "unnamed"
	}
	dir := filepath.Join(StagedRoot(home), sanitizeName(src.Name), safe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir staged dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "original.md"), []byte(originalContent), 0o644); err != nil {
		return "", fmt.Errorf("write original.md: %w", err)
	}

	var jb strings.Builder
	fmt.Fprintf(&jb, "verdict: %s\n", judge.Verdict)
	fmt.Fprintf(&jb, "reason: %s\n", judge.Reason)
	if judge.Domain != "" {
		fmt.Fprintf(&jb, "domain: %s\n", judge.Domain)
	}
	jb.WriteString("\n--- raw curator output ---\n")
	jb.WriteString(judge.FullText)
	jb.WriteString("\n")
	if err := os.WriteFile(filepath.Join(dir, "judge.txt"), []byte(jb.String()), 0o644); err != nil {
		return "", fmt.Errorf("write judge.txt: %w", err)
	}

	var meta strings.Builder
	fmt.Fprintf(&meta, "source_name=%s\n", src.Name)
	fmt.Fprintf(&meta, "source_url=%s\n", src.URL)
	fmt.Fprintf(&meta, "skill_rel=%s\n", cand.SkillRelPath)
	fmt.Fprintf(&meta, "staged_at=%s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&meta, "verdict=%s\n", judge.Verdict)
	fmt.Fprintf(&meta, "reason=%s\n", judge.Reason)
	if judge.Domain != "" {
		fmt.Fprintf(&meta, "domain=%s\n", judge.Domain)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.txt"), []byte(meta.String()), 0o644); err != nil {
		return "", fmt.Errorf("write meta.txt: %w", err)
	}
	return dir, nil
}

// ListStaged enumerates every staged candidate currently on disk.
// Sorted by source then verdict (yes > maybe > others) then name.
// Used by `kinclaw harvest --review`.
func ListStaged(home string) ([]StagedSkill, error) {
	root := StagedRoot(home)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []StagedSkill
	for _, sourceDir := range entries {
		if !sourceDir.IsDir() {
			continue
		}
		skillsDir := filepath.Join(root, sourceDir.Name())
		skillEntries, err := os.ReadDir(skillsDir)
		if err != nil {
			continue
		}
		for _, sk := range skillEntries {
			if !sk.IsDir() {
				continue
			}
			st := StagedSkill{
				SourceName: sourceDir.Name(),
				SkillName:  sk.Name(),
				StagePath:  filepath.Join(skillsDir, sk.Name()),
			}
			st.fillFromMeta()
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceName != out[j].SourceName {
			return out[i].SourceName < out[j].SourceName
		}
		if out[i].Verdict != out[j].Verdict {
			return verdictOrder(out[i].Verdict) < verdictOrder(out[j].Verdict)
		}
		return out[i].SkillName < out[j].SkillName
	})
	return out, nil
}

func verdictOrder(v JudgeVerdict) int {
	switch v {
	case JudgeYes:
		return 0
	case JudgeMaybe:
		return 1
	case JudgeNo:
		return 2
	case JudgeReference:
		// After the actionable verdicts: reference material is for reading
		// later, not for the review queue competing with forgeable candidates.
		return 3
	}
	return 99
}

func (s *StagedSkill) fillFromMeta() {
	data, err := os.ReadFile(filepath.Join(s.StagePath, "meta.txt"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "source_url":
			s.SourceURL = v
		case "skill_rel":
			s.SkillRelPath = v
		case "staged_at":
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				s.StagedAt = t
			}
		case "verdict":
			s.Verdict = JudgeVerdict(v)
		case "reason":
			s.Reason = v
		case "domain":
			s.Domain = v
		}
	}
}

// AcceptOptions configures one --accept call.
type AcceptOptions struct {
	Home          string
	KinclawBin    string
	CoderSoulPath string // resolved souls/coder.soul.md; empty disables forge (library-fallback only)
	SkillsDir     string // ./skills/ — destination for forged candidates
	LibraryDir    string // ./skills/library/ — destination for coder-deferred candidates
	Out           io.Writer
}

// AcceptResult tells the caller what happened to a --accept invocation.
type AcceptResult struct {
	Verdict    AcceptVerdict
	DestPath   string // where the candidate landed
	Reason     string // forged reason / coder defer reason / error
	ForgedName string // skill name as parsed from the forged SKILL.md (when Forged)
}

// AcceptVerdict is the four-way outcome of accept:
//
//	AcceptForged   = coder produced a real KinClaw SKILL.md, validated, written to ./skills/<name>/
//	AcceptLibrary  = coder said defer_to_procedural; original.md copied to ./skills/library/<source>/<name>/
//	AcceptError    = forge attempt failed (parse / validate / unparseable); nothing written
//	AcceptDuplicate = ./skills/<name>/ already exists; user must clear it first
type AcceptVerdict string

const (
	AcceptForged    AcceptVerdict = "forged"
	AcceptLibrary   AcceptVerdict = "library"
	AcceptError     AcceptVerdict = "error"
	AcceptDuplicate AcceptVerdict = "duplicate"
)

// AcceptStaged is the v1.6 accept path: read the staged candidate's
// original.md, spawn coder to attempt a forge, route the result.
//
// Forge succeeds → SKILL.md placed at <SkillsDir>/<forged_name>/.
// Forge defers   → original.md copied to <LibraryDir>/<source>/<staged_name>/.
// Forge errors   → nothing written; AcceptResult.Reason explains.
//
// `skillID` matches the format printed by --review: `<source>/<name>`.
// The staged dir's `original.md` is what gets fed to coder; the
// staged `<name>` may differ from the forge'd name (curator might
// have used a hyphenated name from the source dir; coder is expected
// to produce a snake_case forge_name).
func AcceptStaged(ctx context.Context, opts AcceptOptions, skillID string) (*AcceptResult, error) {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	source, name, ok := strings.Cut(skillID, "/")
	if !ok {
		return nil, fmt.Errorf("skill id %q must be of form <source>/<skill-name>", skillID)
	}
	stageDir := filepath.Join(StagedRoot(opts.Home), source, name)
	originalPath := filepath.Join(stageDir, "original.md")
	originalBytes, err := os.ReadFile(originalPath)
	if err != nil {
		return nil, fmt.Errorf("read staged original %q: %w", skillID, err)
	}

	// Without coder, accept can ONLY fall back to library/. Honest path
	// for users without Ollama auth or who explicitly disabled coder.
	if opts.CoderSoulPath == "" || opts.KinclawBin == "" {
		dest, err := copyToLibrary(opts.LibraryDir, source, name, originalBytes, "no coder soul available")
		if err != nil {
			return nil, fmt.Errorf("library fallback: %w", err)
		}
		return &AcceptResult{Verdict: AcceptLibrary, DestPath: dest, Reason: "no coder soul; copied to library"}, nil
	}

	// Read source URL + rel from meta.txt so the inspire prompt has
	// proper provenance context.
	meta := readMetaKV(filepath.Join(stageDir, "meta.txt"))
	srcURL := meta["source_url"]
	srcRel := meta["skill_rel"]

	fmt.Fprintf(out, "── coder forging %s …\n", skillID)
	res, ferr := Inspire(ctx, opts.KinclawBin, opts.CoderSoulPath, string(originalBytes), srcURL, srcRel)
	if ferr != nil {
		return &AcceptResult{Verdict: AcceptError, Reason: ferr.Error()}, nil
	}

	switch res.Verdict {
	case InspireForged:
		// Round-trip the forged content through LoadExternalSkill
		// + ValidateSkillMeta — same gate as live registry boot.
		loaded, perr := loadFromString(res.ForgedContent)
		if perr != nil {
			return &AcceptResult{
				Verdict: AcceptError,
				Reason:  fmt.Sprintf("forged but unparseable: %v", perr),
			}, nil
		}
		if verr := skill.ValidateSkillMeta(loaded.Meta()); verr != nil {
			return &AcceptResult{
				Verdict: AcceptError,
				Reason:  fmt.Sprintf("forged but failed forge gate v2: %v", verr),
			}, nil
		}
		forgedName := loaded.Name()
		dest := filepath.Join(opts.SkillsDir, forgedName)
		if _, err := os.Stat(dest); err == nil {
			return &AcceptResult{
				Verdict:    AcceptDuplicate,
				DestPath:   dest,
				ForgedName: forgedName,
				Reason:     "destination already exists; remove it first",
			}, nil
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dest, err)
		}
		if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte(res.ForgedContent), 0o644); err != nil {
			return nil, fmt.Errorf("write SKILL.md: %w", err)
		}
		return &AcceptResult{
			Verdict:    AcceptForged,
			DestPath:   dest,
			ForgedName: forgedName,
			Reason:     "coder forged + parsed + passed forge gate v2",
		}, nil

	case InspireDeferred:
		reason := res.DeferReason
		if reason == "" {
			reason = "coder deferred without specific reason"
		}
		dest, err := copyToLibrary(opts.LibraryDir, source, name, originalBytes, reason)
		if err != nil {
			return nil, fmt.Errorf("library fallback: %w", err)
		}
		return &AcceptResult{
			Verdict:  AcceptLibrary,
			DestPath: dest,
			Reason:   reason,
		}, nil

	case InspireUnparseable:
		return &AcceptResult{
			Verdict: AcceptError,
			Reason:  "coder response unparseable; raw output above in stderr",
		}, nil
	}
	return nil, fmt.Errorf("unknown inspire verdict %q", res.Verdict)
}

func copyToLibrary(libraryDir, source, name string, originalBytes []byte, reason string) (string, error) {
	dest := filepath.Join(libraryDir, source, name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dest, err)
	}
	if err := os.WriteFile(filepath.Join(dest, "original.md"), originalBytes, 0o644); err != nil {
		return "", fmt.Errorf("write library original.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "defer_reason.txt"), []byte(reason+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write library defer_reason.txt: %w", err)
	}
	return dest, nil
}

func readMetaKV(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// loadFromString writes skillContent to a temp file and runs the
// registry's standard loader on it. Used by AcceptStaged to validate
// coder's forged output before placing it in ./skills/.
func loadFromString(skillContent string) (*skill.ExternalSkill, error) {
	tmpDir, err := os.MkdirTemp("", "kinclaw-harvest-accept-*")
	if err != nil {
		return nil, fmt.Errorf("tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmpSkill := filepath.Join(tmpDir, "SKILL.md")
	if err := os.WriteFile(tmpSkill, []byte(skillContent), 0o644); err != nil {
		return nil, fmt.Errorf("tmp write: %w", err)
	}
	return skill.LoadExternalSkill(tmpSkill)
}

// sanitizeName makes a candidate name safe to use as a path component.
//
// Candidate names come from frontmatter in third-party repositories, and they
// are used to build directory and file paths. A name of "../../../tmp/x" would
// otherwise write outside the harvest tree entirely. Keeping only characters
// that can appear in a skill identifier removes the whole class rather than
// blacklisting the traversal sequences people think to test for.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			// Dots are allowed inside a name but a name that is only dots
			// (".", "..") is rejected below — that is the traversal case.
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return ""
	}
	return out
}

// ReferenceRoot is where reference-only candidates are archived.
//
// Deliberately outside StagedRoot: `--review` and `--accept` operate on staged
// candidates, and reference material must never be forgeable — it is kept for
// reading, not for turning into a skill. Separate directories make that
// structural instead of a flag someone has to remember to check.
func ReferenceRoot(home string) string {
	return filepath.Join(home, ".kinclaw", "harvest", "reference")
}

// StageReference archives a candidate the curator marked as reference.
//
// Stores the original text unchanged plus the curator's reasoning. No forging
// happens here and none ever will — the value of this material is the idea it
// describes, which is lost the moment it is compressed into a command line.
func StageReference(home string, src Source, cand JudgeCandidate, content string, res *JudgeResult) (string, error) {
	dir := filepath.Join(ReferenceRoot(home), src.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	base := sanitizeName(cand.Name)
	if base == "" {
		base = "unnamed"
	}
	path := filepath.Join(dir, base+".md")

	var b strings.Builder
	b.WriteString("<!-- harvested reference — not a skill, never forged -->\n")
	fmt.Fprintf(&b, "<!-- source: %s -->\n", src.URL)
	fmt.Fprintf(&b, "<!-- file:   %s -->\n", cand.SkillRelPath)
	fmt.Fprintf(&b, "<!-- why:    %s -->\n\n", res.Reason)
	b.WriteString(content)

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ListReference returns archived reference material, newest first.
func ListReference(home string) ([]ReferenceDoc, error) {
	root := ReferenceRoot(home)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []ReferenceDoc
	for _, srcDir := range entries {
		if !srcDir.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, srcDir.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			doc := ReferenceDoc{
				SourceName: srcDir.Name(),
				Name:       strings.TrimSuffix(f.Name(), ".md"),
				Path:       filepath.Join(root, srcDir.Name(), f.Name()),
			}
			if info, err := f.Info(); err == nil {
				doc.ArchivedAt = info.ModTime()
			}
			doc.fillReason()
			out = append(out, doc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ArchivedAt.After(out[j].ArchivedAt)
	})
	return out, nil
}

// ReferenceDoc is one archived reference document.
type ReferenceDoc struct {
	SourceName string
	Name       string
	Path       string
	Reason     string
	ArchivedAt time.Time
}

// fillReason pulls the curator's rationale back out of the header comment.
func (d *ReferenceDoc) fillReason() {
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n")[:min(8, len(strings.Split(string(data), "\n")))] {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "<!-- why:"); ok {
			d.Reason = strings.TrimSpace(strings.TrimSuffix(rest, "-->"))
			return
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
