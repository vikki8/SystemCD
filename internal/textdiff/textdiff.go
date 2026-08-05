// Package textdiff renders unified diffs between two versions of a text file.
//
// A plan that says a checksum changed tells an operator that something is
// different. A plan that shows the three lines that differ tells them whether
// to approve it. That gap is most of the review value in a config change.
package textdiff

import (
	"fmt"
	"strings"
)

// Options control rendering.
type Options struct {
	// Context is the number of unchanged lines shown around each change.
	Context int
	// MaxLines caps the rendered diff, so a rewritten 4,000-line config does
	// not bury the rest of the plan.
	MaxLines int
}

// DefaultOptions are tuned for terminal review.
func DefaultOptions() Options { return Options{Context: 3, MaxLines: 60} }

// Line is one line of a rendered diff.
type Line struct {
	// Kind is ' ' for context, '-' for removed, '+' for added, '@' for a hunk
	// header, and '!' for a truncation notice.
	Kind byte
	Text string
}

// Unified renders the difference between before and after.
func Unified(before, after string, opts Options) []Line {
	if opts.Context <= 0 {
		opts.Context = 3
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = 60
	}

	a := splitLines(before)
	b := splitLines(after)
	ops := diffOps(a, b)

	// Keep only the neighbourhoods around real changes.
	interesting := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		for j := i - opts.Context; j <= i+opts.Context; j++ {
			if j >= 0 && j < len(ops) {
				interesting[j] = true
			}
		}
	}

	var out []Line
	inHunk := false
	for i, op := range ops {
		if !interesting[i] {
			inHunk = false
			continue
		}
		if !inHunk {
			if len(out) > 0 {
				out = append(out, Line{Kind: '@', Text: "…"})
			}
			inHunk = true
		}
		out = append(out, Line{Kind: op.kind, Text: op.text})
		if len(out) >= opts.MaxLines {
			remaining := 0
			for _, rest := range ops[i+1:] {
				if rest.kind != ' ' {
					remaining++
				}
			}
			if remaining > 0 {
				out = append(out, Line{Kind: '!', Text: fmt.Sprintf("… and %d more changed line(s)", remaining)})
			}
			break
		}
	}
	return out
}

// Stat summarizes a diff as added and removed line counts.
func Stat(before, after string) (added, removed int) {
	for _, op := range diffOps(splitLines(before), splitLines(after)) {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// IsText reports whether content is safe to render as a diff. Binary payloads
// under /etc exist, and dumping them to a terminal helps nobody.
func IsText(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return false
		}
	}
	return true
}

type op struct {
	kind byte
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffOps computes a line-level diff using the classic LCS dynamic program.
//
// Config files are small enough that O(n·m) is the right trade: it is exact,
// short, and needs no dependency. Very large inputs fall back to a whole-file
// replacement rather than allocating a huge table.
func diffOps(a, b []string) []op {
	const maxCells = 4_000_000
	if len(a)*len(b) > maxCells {
		out := make([]op, 0, len(a)+len(b))
		for _, line := range a {
			out = append(out, op{kind: '-', text: line})
		}
		for _, line := range b {
			out = append(out, op{kind: '+', text: line})
		}
		return out
	}

	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []op
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, op{kind: ' ', text: a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, op{kind: '-', text: a[i]})
			i++
		default:
			out = append(out, op{kind: '+', text: b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, op{kind: '-', text: a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, op{kind: '+', text: b[j]})
	}
	return out
}
