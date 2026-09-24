package metrics

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vikki8/systemcd/internal/engine"
	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

// sampleLine matches one sample in the Prometheus text format: a metric name,
// optional labels whose values use only the \\, \" and \n escapes, and a
// value.
var sampleLine = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*"(,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*")*\})? [-+0-9.eE]+$`)

func checkFormat(t *testing.T, body string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if strings.HasPrefix(line, "# HELP ") || strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		if !sampleLine.MatchString(line) {
			t.Errorf("line %d is not valid exposition format: %q", i+1, line)
		}
	}
}

func value(t *testing.T, body, series string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			return strings.TrimPrefix(line, series+" ")
		}
	}
	t.Fatalf("series %s missing:\n%s", series, body)
	return ""
}

func newReport(dryRun bool, results ...engine.Result) *engine.Report {
	now := time.Now()
	return &engine.Report{DryRun: dryRun, Revision: "abc123", Results: results, Started: now, Finished: now}
}

func changed(name string) engine.Result {
	return engine.Result{
		ID:     resource.ID{Kind: "File", Name: name},
		Action: engine.ActionUpdate,
		Diff:   resource.Diff{{Field: "mode", Have: "0600", Want: "0644"}},
		Owner:  engine.Ownership{Owned: true, Origin: state.OriginExternal},
	}
}

func TestLabelValuesAreEscapedForPrometheus(t *testing.T) {
	// Go's %q emits \t and \x escapes the format does not have, and a single
	// bad line makes node_exporter drop every metric in the file.
	odd := engine.Result{ID: resource.ID{Kind: "File", Name: "tab\there \"quoted\" back\\slash\nnewline \x01"}, Action: engine.ActionNoop}
	body := string(Render(newReport(true, odd), false))
	checkFormat(t, body)
	if !strings.Contains(body, `name="tab`+"\t"+`here \"quoted\" back\\slash\nnewline`) {
		t.Errorf("label value not escaped as expected:\n%s", body)
	}
}

func TestAnApplyThatConvergedIsNotOutOfSync(t *testing.T) {
	// After a successful self-heal the host matches the repository. Saying
	// otherwise would fire an out-of-sync alert after every heal.
	failed := changed("broken")
	failed.Action, failed.Err = engine.ActionError, errors.New("denied")

	body := string(Render(newReport(false, changed("motd")), false))
	checkFormat(t, body)
	if got := value(t, body, "systemcd_out_of_sync"); got != "0" {
		t.Errorf("systemcd_out_of_sync = %s after a clean apply, want 0", got)
	}
	if got := value(t, body, `systemcd_resource_out_of_sync{kind="File",name="motd",origin="external"}`); got != "0" {
		t.Errorf("per-resource out of sync = %s after it was converged, want 0", got)
	}
	if got := value(t, body, "systemcd_resources_changed"); got != "1" {
		t.Errorf("systemcd_resources_changed = %s, want 1", got)
	}
	if got := value(t, body, "systemcd_external_drift"); got != "1" {
		t.Errorf("the external edit that was healed should still be counted, got %s", got)
	}

	body = string(Render(newReport(false, changed("motd"), failed), false))
	if got := value(t, body, "systemcd_out_of_sync"); got != "1" {
		t.Errorf("a resource that failed to converge still differs, got %s", got)
	}
	if got := value(t, body, "systemcd_resources_out_of_sync"); got != "1" {
		t.Errorf("systemcd_resources_out_of_sync = %s, want 1", got)
	}
}

func TestAPausedPassChangesNothing(t *testing.T) {
	// A paused or report-only pass is a dry run: its pending changes are
	// drift, not changes made.
	body := string(Render(newReport(true, changed("motd"), changed("issue")), true))
	checkFormat(t, body)
	if got := value(t, body, "systemcd_resources_changed"); got != "0" {
		t.Errorf("systemcd_resources_changed = %s during a dry run, want 0", got)
	}
	if got := value(t, body, "systemcd_resources_out_of_sync"); got != "2" {
		t.Errorf("systemcd_resources_out_of_sync = %s, want 2", got)
	}
	if got := value(t, body, "systemcd_out_of_sync"); got != "1" {
		t.Errorf("systemcd_out_of_sync = %s, want 1", got)
	}
	if got := value(t, body, "systemcd_paused"); got != "1" {
		t.Errorf("systemcd_paused = %s, want 1", got)
	}
}

func TestWriteReplacesTheFileAtomically(t *testing.T) {
	m := host.NewMem()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(Write(m, "/var/lib/node_exporter/systemcd.prom", newReport(false), false))
	must(Write(m, "/var/lib/node_exporter/systemcd.prom", newReport(true, changed("x")), true))
	data, err := m.ReadFile("/var/lib/node_exporter/systemcd.prom")
	must(err)
	if !strings.Contains(string(data), "systemcd_paused 1") {
		t.Errorf("second write did not replace the first:\n%s", data)
	}
}
