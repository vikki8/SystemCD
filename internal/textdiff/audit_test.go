package textdiff

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestTrailingNewlineChangeIsVisible(t *testing.T) {
	// "a" and "a\n" are different files; a plan that says the checksum
	// changed but shows no line is unreviewable.
	if added, removed := Stat("a\nb\n", "a\nb"); added != 1 || removed != 1 {
		t.Errorf("Stat = +%d -%d, want +1 -1", added, removed)
	}
	out := render("a\nb\n", "a\nb")
	if !strings.Contains(out, "-b\n") || !strings.Contains(out, "+b\n") || !strings.Contains(out, "No newline at end of file") {
		t.Errorf("diff should show the last line and the missing newline:\n%s", out)
	}
	if out := render("a\nb", "a\nb"); out != "" {
		t.Errorf("identical content without a final newline should not differ:\n%s", out)
	}
}

func numbered(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("setting_%d = %d", i, i)
	}
	return lines
}

func join(lines []string) string { return strings.Join(lines, "\n") + "\n" }

func TestSmallEditsToLargeFilesStaySmall(t *testing.T) {
	// Past the LCS table limit the whole file used to be reported as
	// replaced: a one-line edit to a 5,000-line config read "+5000 -5000".
	before := numbered(5000)

	one := append([]string(nil), before...)
	one[2500] = "setting_2500 = changed"
	if added, removed := Stat(join(before), join(one)); added != 1 || removed != 1 {
		t.Errorf("one edit: Stat = +%d -%d, want +1 -1", added, removed)
	}

	// Edits at both ends leave a large middle that prefix and suffix
	// trimming cannot remove.
	two := append([]string(nil), before...)
	two[10] = "setting_10 = changed"
	two[4990] = "setting_4990 = changed"
	two = append(two[:2000], append([]string{"inserted = 1"}, two[2000:]...)...)
	start := time.Now()
	if added, removed := Stat(join(before), join(two)); added != 3 || removed != 2 {
		t.Errorf("scattered edits: Stat = +%d -%d, want +3 -2", added, removed)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("diff took %v", elapsed)
	}
	lines := Unified(join(before), join(two), Options{Context: 1, MaxLines: 100})
	var changed []string
	for _, l := range lines {
		if l.Kind == '+' || l.Kind == '-' {
			changed = append(changed, string(l.Kind)+l.Text)
		}
	}
	if len(changed) != 5 {
		t.Errorf("changed lines = %v, want exactly the five edits", changed)
	}
}

// lcsLength is the reference: the minimal edit count is n+m-2·LCS.
func lcsLength(a, b []string) int {
	prev := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

func TestMyersProducesMinimalValidScripts(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", "c", "d"}
	gen := func() []string {
		out := make([]string, rng.Intn(30))
		for i := range out {
			out[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return out
	}
	for iter := 0; iter < 2000; iter++ {
		a, b := gen(), gen()
		ops, ok := myersOps(a, b, len(a)+len(b))
		if !ok {
			t.Fatalf("myersOps gave up within its own bound for %v -> %v", a, b)
		}
		var gotA, gotB []string
		edits := 0
		for _, o := range ops {
			switch o.kind {
			case ' ':
				gotA, gotB = append(gotA, o.text), append(gotB, o.text)
			case '-':
				gotA = append(gotA, o.text)
				edits++
			case '+':
				gotB = append(gotB, o.text)
				edits++
			}
		}
		if strings.Join(gotA, ",") != strings.Join(a, ",") || strings.Join(gotB, ",") != strings.Join(b, ",") {
			t.Fatalf("script does not reproduce the inputs: %v -> %v gave %v", a, b, ops)
		}
		if want := len(a) + len(b) - 2*lcsLength(a, b); edits != want {
			t.Fatalf("%v -> %v: %d edits, want the minimal %d", a, b, edits, want)
		}
	}
	if _, ok := myersOps([]string{"a", "b"}, []string{"c", "d"}, 3); ok {
		t.Error("myersOps should give up past its edit bound")
	}
}
