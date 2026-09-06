package skill

import (
	"fmt"
	"strings"
)

// ANSI colours of the diff lines, matching kincode's file_edit output so
// KinClaw Mac's DiffView renders both kernels' edits with one parser.
const (
	diffRed   = "\033[31m"
	diffGreen = "\033[32m"
	diffGray  = "\033[90m"
	diffReset = "\033[0m"
)

// diff limits: LCS is O(n·m); past these the simple scan is used, and
// the rendered hunks are capped so a 5000-line rewrite doesn't become a
// 5000-line tool result.
const (
	diffLCSMaxLines = 1500
	diffMaxOutLines = 200
	diffContext     = 3
)

// UnifiedDiff renders old → new as a coloured unified diff with three
// lines of context per hunk. Empty when the contents are identical.
func UnifiedDiff(oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	// A missing / empty file has no lines, not one empty line —
	// otherwise a new file's diff starts with a spurious "-".
	var oldLines, newLines []string
	if oldContent != "" {
		oldLines = strings.Split(oldContent, "\n")
	}
	if newContent != "" {
		newLines = strings.Split(newContent, "\n")
	}
	var ops []diffOp
	if len(oldLines) <= diffLCSMaxLines && len(newLines) <= diffLCSMaxLines {
		ops = lcsDiff(oldLines, newLines)
	} else {
		ops = scanDiff(oldLines, newLines)
	}
	return renderHunks(ops)
}

type diffOp struct {
	kind byte // ' ' context, '-' removed, '+' added
	text string
}

// lcsDiff computes a minimal line diff via a longest-common-subsequence
// table — the same algorithm behind `diff`'s default output.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[i:], b[j:]
	dp := make([]int32, (n+1)*(m+1))
	idx := func(i, j int) int { return i*(m+1) + j }
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[idx(i, j)] = dp[idx(i+1, j+1)] + 1
			} else if dp[idx(i+1, j)] >= dp[idx(i, j+1)] {
				dp[idx(i, j)] = dp[idx(i+1, j)]
			} else {
				dp[idx(i, j)] = dp[idx(i, j+1)]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[idx(i+1, j)] >= dp[idx(i, j+1)]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// scanDiff is the cheap fallback for very large files: common prefix and
// suffix are context, everything between is a replacement.
func scanDiff(a, b []string) []diffOp {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	var ops []diffOp
	for _, l := range a[:p] {
		ops = append(ops, diffOp{' ', l})
	}
	for _, l := range a[p : len(a)-s] {
		ops = append(ops, diffOp{'-', l})
	}
	for _, l := range b[p : len(b)-s] {
		ops = append(ops, diffOp{'+', l})
	}
	for _, l := range a[len(a)-s:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// renderHunks keeps diffContext lines around each change and drops the
// unchanged stretches in between, capping total output.
func renderHunks(ops []diffOp) string {
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		for k := i - diffContext; k <= i+diffContext; k++ {
			if k >= 0 && k < len(ops) {
				keep[k] = true
			}
		}
	}
	var sb strings.Builder
	lines := 0
	skipping := false
	for i, op := range ops {
		if !keep[i] {
			if !skipping && lines > 0 {
				sb.WriteString(diffGray + " …" + diffReset + "\n")
			}
			skipping = true
			continue
		}
		skipping = false
		if lines >= diffMaxOutLines {
			rest := 0
			for k := i; k < len(ops); k++ {
				if keep[k] {
					rest++
				}
			}
			fmt.Fprintf(&sb, "%s … %d more diff lines not shown%s\n", diffGray, rest, diffReset)
			break
		}
		switch op.kind {
		case '-':
			sb.WriteString(diffRed + "-" + op.text + diffReset + "\n")
		case '+':
			sb.WriteString(diffGreen + "+" + op.text + diffReset + "\n")
		default:
			sb.WriteString(diffGray + " " + op.text + diffReset + "\n")
		}
		lines++
	}
	return strings.TrimRight(sb.String(), "\n")
}
