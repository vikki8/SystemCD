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
	// header, '!' for a truncation notice, and '\\' for the "\ No newline at
	// end of file" marker that follows a line lacking its newline.
	Kind byte
	Text string
}

// noNewline follows a removed or added last line that has no trailing
// newline, as in diff -u; without it "a" and "a\n" differ with nothing shown.
const noNewline = `\ No newline at end of file`

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
		out = append(out, Line{Kind: op.kind, Text: strings.TrimSuffix(op.text, "\n")})
		if op.kind != ' ' && !strings.HasSuffix(op.text, "\n") {
			out = append(out, Line{Kind: '\\', Text: noNewline})
		}
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

// splitLines splits s into lines that keep their "\n", so a final line with
// and without one compare as different.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

const (
	// maxCells bounds the LCS table.
	maxCells = 4_000_000
	// maxEdits bounds the Myers search, whose trace grows with its square.
	maxEdits = 1000
)

// diffOps computes a line-level diff.
//
// Lines shared at both ends are unchanged whatever the algorithm, so they are
// set aside first; a one-line edit to a 10,000-line file then costs nothing.
// What remains goes through the classic LCS dynamic program when its table is
// small, and Myers' O((N+M)·D) search when it is not, so a few scattered
// edits to a large file still show as a few lines. Only a large file with
// more than maxEdits changed lines falls back to a whole-block replacement.
func diffOps(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	out := make([]op, 0, len(a)+len(b)-pre-suf)
	for _, line := range a[:pre] {
		out = append(out, op{kind: ' ', text: line})
	}
	midA, midB := a[pre:len(a)-suf], b[pre:len(b)-suf]
	switch {
	case len(midA)*len(midB) <= maxCells:
		out = append(out, lcsOps(midA, midB)...)
	default:
		ops, ok := myersOps(midA, midB, maxEdits)
		if !ok {
			ops = make([]op, 0, len(midA)+len(midB))
			for _, line := range midA {
				ops = append(ops, op{kind: '-', text: line})
			}
			for _, line := range midB {
				ops = append(ops, op{kind: '+', text: line})
			}
		}
		out = append(out, ops...)
	}
	for _, line := range a[len(a)-suf:] {
		out = append(out, op{kind: ' ', text: line})
	}
	return out
}

// myersOps finds a shortest edit script with Myers' greedy algorithm, giving
// up (false) once more than maxD insertions and deletions would be needed.
// The trace keeps, per edit distance d, the furthest x reached on diagonals
// -d-1..d+1, so memory is O(D²) rather than O(N·M).
func myersOps(a, b []string, maxD int) ([]op, bool) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxD {
		limit = maxD
	}
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int
	for d := 0; d <= limit; d++ {
		snap := make([]int, 2*d+3)
		copy(snap, v[off-d-1:off+d+2])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1] // down: insert b[y]
			} else {
				x = v[off+k-1] + 1 // right: delete a[x]
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= n && y >= m {
				return myersPath(a, b, trace), true
			}
		}
	}
	return nil, false
}

// myersPath walks the trace back from the end of both inputs.
func myersPath(a, b []string, trace [][]int) []op {
	x, y := len(a), len(b)
	var rev []op
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d] // v[k+d+1] is the furthest x on diagonal k before round d
		k := x - y
		prevK := k - 1
		if k == -d || (k != d && v[k-1+d+1] < v[k+1+d+1]) {
			prevK = k + 1
		}
		prevX := v[prevK+d+1]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, op{kind: ' ', text: a[x-1]})
			x, y = x-1, y-1
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, op{kind: '+', text: b[y-1]})
			} else {
				rev = append(rev, op{kind: '-', text: a[x-1]})
			}
		}
		x, y = prevX, prevY
	}
	out := make([]op, len(rev))
	for i, o := range rev {
		out[len(rev)-1-i] = o
	}
	return out
}

// lcsOps is the classic LCS dynamic program: exact, short, and O(n·m).
func lcsOps(a, b []string) []op {
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
