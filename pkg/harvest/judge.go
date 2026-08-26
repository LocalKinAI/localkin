package harvest

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Judge spawns the curator specialist with the current KinClaw skill
// inventory + one external candidate, asks it to make a yes/maybe/no
// decision, parses the structured response.
//
// This is the v1.6+ harvest scan-time primitive. Replaces the v1.5.x
// per-candidate forge spawn (which was expensive and always tried to
// produce a runnable SKILL.md). Forge happens at --accept time now,
// triggered explicitly per-candidate by the user — see AcceptStaged
// for that path.
//
// Per-call cost is small: ~1k input tokens (architecture summary +
// inventory + candidate excerpt) + ~50 output tokens. Sequential at
// ~3-5s per call on Kimi K2.6; trivially parallelizable later if the
// 172-candidate × 5s = 14-minute serial path becomes the bottleneck.
//
// Mirrors the spawn skill's exec pattern (cmd.Output + KINCLAW_SPAWN_
// DEPTH=1 env guard so curator can't itself spawn).
func Judge(ctx context.Context, kinclawBin, curatorSoulPath string, inv *SkillInventory, candidate JudgeCandidate) (*JudgeResult, error) {
	prompt := buildJudgePrompt(inv, candidate)

	// 120s — Kimi K2.6 cloud round-trips average 5-15s but slower
	// candidates (longer body excerpts, network jitter) occasionally
	// push past 60s. Tighter than 120s causes spurious timeouts;
	// longer than 120s wastes worker time on hung calls.
	ctx, cancel := withTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, kinclawBin,
		"-soul", curatorSoulPath,
		"-exec", prompt)
	cmd.Env = append(cmd.Environ(), "KINCLAW_SPAWN_DEPTH=1")

	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("curator timed out after 120s")
	}
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("curator exec failed: %w\n--- stderr ---\n%s", err, truncate(stderr, 500))
	}

	body := strings.TrimSpace(string(out))
	return parseJudgeResponse(body), nil
}

// JudgeCandidate is the per-call input the harvest pipeline assembles
// for each external SKILL.md it scans. Curator reads these fields,
// references the SkillInventory passed alongside, returns a verdict.
type JudgeCandidate struct {
	SourceURL    string // e.g. https://github.com/anthropics/skills
	SourceName   string // manifest source name, used for staging path
	SkillRelPath string // path within the source repo
	Name         string // candidate skill name (from frontmatter or dir)
	Description  string // candidate description (from frontmatter)
	BodyExcerpt  string // first ~800 chars of candidate's markdown body
}

// JudgeResult is the structured outcome of one curator call.
type JudgeResult struct {
	FullText string       // raw curator output (saved with the staged candidate)
	Verdict  JudgeVerdict // yes | maybe | no | unparseable
	Reason   string       // one-sentence explanation
	Domain   string       // short tag — apple / git / web / ml / creative / ...
}

// JudgeVerdict is the four-way decision the harvest pipeline routes on.
type JudgeVerdict string

const (
	JudgeYes   JudgeVerdict = "yes"
	JudgeMaybe JudgeVerdict = "maybe"
	JudgeNo    JudgeVerdict = "no"
	// JudgeReference is for candidates worth keeping but not worth forging.
	//
	// The pipeline used to be binary: forge it into skills/, or throw it away.
	// That loses a real category — methodology and reference material like
	// skill-creator, mcp-builder, or a brand-guidelines document. Those are
	// genuinely not expressible as `command + args`, so "no" is the correct
	// judgment about forging them, but discarding them throws away the ideas
	// that made harvest worth running in the first place.
	//
	// Reference candidates are archived for reading, never registered as
	// skills, and never touch the repo.
	JudgeReference   JudgeVerdict = "reference"
	JudgeUnparseable JudgeVerdict = "unparseable"
)

var (
	judgeVerdictRe = regexp.MustCompile(`(?im)^verdict\s*[:：]\s*(\S+)`)
	judgeReasonRe  = regexp.MustCompile(`(?im)^reason\s*[:：]\s*(.+)$`)
	judgeDomainRe  = regexp.MustCompile(`(?im)^domain\s*[:：]\s*(\S+)`)
)

func parseJudgeResponse(body string) *JudgeResult {
	r := &JudgeResult{FullText: body}

	if m := judgeReasonRe.FindStringSubmatch(body); m != nil {
		r.Reason = strings.TrimSpace(m[1])
	}
	if m := judgeDomainRe.FindStringSubmatch(body); m != nil {
		r.Domain = strings.ToLower(strings.TrimSpace(m[1]))
	}

	vm := judgeVerdictRe.FindStringSubmatch(body)
	if vm == nil {
		r.Verdict = JudgeUnparseable
		return r
	}
	switch v := strings.ToLower(strings.TrimSpace(vm[1])); v {
	case "yes", "y", "通过", "是":
		r.Verdict = JudgeYes
	case "maybe", "perhaps", "也许", "或许", "看情况":
		r.Verdict = JudgeMaybe
	case "no", "n", "拒绝", "否":
		r.Verdict = JudgeNo
	case "reference", "ref", "参考", "存档":
		r.Verdict = JudgeReference
	default:
		r.Verdict = JudgeUnparseable
	}
	return r
}

func buildJudgePrompt(inv *SkillInventory, c JudgeCandidate) string {
	var b strings.Builder
	b.WriteString("## current_skills\n")
	b.WriteString(inv.String())
	b.WriteString("\n\n## candidate\n")
	fmt.Fprintf(&b, "source_url: %s\n", c.SourceURL)
	fmt.Fprintf(&b, "file: %s\n", c.SkillRelPath)
	fmt.Fprintf(&b, "name: %s\n", c.Name)
	fmt.Fprintf(&b, "description: %s\n", c.Description)
	body := strings.TrimSpace(c.BodyExcerpt)
	if len(body) > 800 {
		body = body[:800] + "...[truncated]"
	}
	fmt.Fprintf(&b, "body_excerpt: |\n%s\n", indent(body, "  "))
	b.WriteString("\nReply per your soul's three-line format (verdict / reason / domain).\n")
	return b.String()
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
