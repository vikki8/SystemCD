package textdiff

import (
	"strings"
	"testing"
)

func render(before, after string) string {
	var b strings.Builder
	for _, line := range Unified(before, after, DefaultOptions()) {
		b.WriteByte(line.Kind)
		b.WriteString(line.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestUnifiedShowsOnlyWhatChanged(t *testing.T) {
	before := "a\nb\nc\nworker_processes 2;\ne\nf\ng\n"
	after := "a\nb\nc\nworker_processes 8;\ne\nf\ng\n"

	out := render(before, after)
	if !strings.Contains(out, "-worker_processes 2;") || !strings.Contains(out, "+worker_processes 8;") {
		t.Fatalf("diff should show the changed line:\n%s", out)
	}
	if strings.Count(out, "\n") > 8 {
		t.Errorf("only the neighbourhood of the change should be shown:\n%s", out)
	}
}

func TestUnifiedHandlesAdditionAndRemoval(t *testing.T) {
	if out := render("", "new\n"); !strings.Contains(out, "+new") {
		t.Errorf("creating content should be all additions:\n%s", out)
	}
	if out := render("gone\n", ""); !strings.Contains(out, "-gone") {
		t.Errorf("emptying content should be all removals:\n%s", out)
	}
	if out := render("same\n", "same\n"); out != "" {
		t.Errorf("identical content should produce no diff, got:\n%s", out)
	}
}

func TestUnifiedTruncatesEnormousRewrites(t *testing.T) {
	// A rewritten 4,000-line config must not bury the rest of the plan.
	before := strings.Repeat("old line\n", 400)
	after := strings.Repeat("new line\n", 400)

	lines := Unified(before, after, Options{Context: 3, MaxLines: 20})
	if len(lines) > 21 {
		t.Fatalf("diff was not truncated: %d lines", len(lines))
	}
	last := lines[len(lines)-1]
	if last.Kind != '!' || !strings.Contains(last.Text, "more changed line") {
		t.Errorf("truncation should be announced, got %q", last.Text)
	}
}

func TestStatCountsLines(t *testing.T) {
	added, removed := Stat("a\nb\n", "a\nc\nd\n")
	if added != 2 || removed != 1 {
		t.Errorf("added=%d removed=%d, want 2 and 1", added, removed)
	}
}

func TestIsTextRejectsBinary(t *testing.T) {
	if !IsText([]byte("plain text\n")) {
		t.Error("text should be accepted")
	}
	if IsText([]byte("binary\x00payload")) {
		t.Error("NUL bytes should mark content as binary")
	}
}

func TestHugeInputsFallBackWithoutHanging(t *testing.T) {
	// The LCS table is quadratic; past a threshold the diff degrades to a
	// whole-file replacement rather than allocating gigabytes.
	before := strings.Repeat("x\n", 3000)
	after := strings.Repeat("y\n", 3000)
	if got := Unified(before, after, Options{Context: 1, MaxLines: 5}); len(got) == 0 {
		t.Error("a very large diff should still render something")
	}
}
