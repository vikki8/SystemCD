package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/resource"
)

type jsonOut struct {
	Status    string `json:"status"`
	OutOfSync bool   `json:"outOfSync"`
	Results   []struct {
		Resource string `json:"resource"`
		Sync     string `json:"sync"`
		Diff     []struct {
			Have string `json:"have"`
			Want string `json:"want"`
		} `json:"diff"`
	} `json:"results"`
}

func decode(t *testing.T, r *engine.Report) (jsonOut, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := JSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var out jsonOut
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	return out, buf.String()
}

func update(name string) engine.Result {
	return engine.Result{
		ID:     resource.ID{Kind: "File", Name: name},
		Action: engine.ActionUpdate,
		Diff:   resource.Diff{{Field: "mode", Have: "0600", Want: "0644"}},
	}
}

func TestJSONAfterAnApplySaysConvergedResourcesAreSynced(t *testing.T) {
	// An apply that converged a resource leaves it in sync. Reporting it as
	// OutOfSync, beside "status": "Synced", contradicted itself and made
	// every successful self-heal look like drift that was still there.
	now := time.Now()
	failed := update("broken")
	failed.Action, failed.Err = engine.ActionError, errors.New("permission denied")
	r := &engine.Report{Results: []engine.Result{update("motd"), failed}, Started: now, Finished: now}

	out, raw := decode(t, r)
	if out.Results[0].Sync != "Synced" {
		t.Errorf("converged resource sync = %q, want Synced\n%s", out.Results[0].Sync, raw)
	}
	if out.Results[1].Sync != "Error" {
		t.Errorf("failed resource sync = %q, want Error", out.Results[1].Sync)
	}
	if !out.OutOfSync {
		t.Error("a failed resource still differs, so outOfSync should be true")
	}

	r.Results = r.Results[:1]
	if out, raw := decode(t, r); out.OutOfSync || out.Status != "Synced" {
		t.Errorf("clean apply: outOfSync = %v status = %q\n%s", out.OutOfSync, out.Status, raw)
	}

	// A plan changes nothing, so what it found is still there.
	r.DryRun = true
	if out, _ := decode(t, r); !out.OutOfSync || out.Results[0].Sync != "OutOfSync" {
		t.Errorf("plan: %+v", out)
	}
}

func TestJSONResultsIsAnEmptyListNotNull(t *testing.T) {
	now := time.Now()
	_, raw := decode(t, &engine.Report{Started: now, Finished: now})
	if !strings.Contains(raw, `"results": []`) {
		t.Errorf("an empty run should render results as []:\n%s", raw)
	}
}

func TestSensitiveValuesNeverRender(t *testing.T) {
	now := time.Now()
	res := engine.Result{
		ID:     resource.ID{Kind: "File", Name: "secret"},
		Action: engine.ActionUpdate,
		Diff:   resource.Diff{{Field: "checksum", Have: "sha256:aaaa", Want: "sha256:bbbb", Sensitive: true}},
	}
	r := &engine.Report{DryRun: true, Results: []engine.Result{res}, Started: now, Finished: now}

	var text bytes.Buffer
	if err := Text(&text, r, Options{ShowDiff: true}); err != nil {
		t.Fatal(err)
	}
	_, raw := decode(t, r)
	for name, out := range map[string]string{"text": text.String(), "json": raw} {
		if strings.Contains(out, "sha256:aaaa") || strings.Contains(out, "sha256:bbbb") {
			t.Errorf("%s output leaks a sensitive value:\n%s", name, out)
		}
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("é", 100)
	got := truncate(s)
	if !utf8.ValidString(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 72 {
		t.Errorf("truncated to %d characters, want 72", n)
	}
	if short := "héllo"; truncate(short) != short {
		t.Errorf("a short string was changed: %q", truncate(short))
	}
}

func TestNoColorWithoutATerminal(t *testing.T) {
	if DetectColor(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
	now := time.Now()
	var buf bytes.Buffer
	r := &engine.Report{DryRun: true, Results: []engine.Result{update("motd")}, Started: now, Finished: now}
	if err := Text(&buf, r, Options{ShowDiff: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("escape codes without Color:\n%q", buf.String())
	}
}
