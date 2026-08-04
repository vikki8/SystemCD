package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
)

// harness wires an App to an in-memory host and captured output.
type harness struct {
	app    *App
	mem    *host.MemHost
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	dir    string
}

func newHarness(t *testing.T, manifests string) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(manifests), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := host.NewMem()
	h := &harness{
		mem:    mem,
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		dir:    dir,
	}
	h.app = &App{Host: mem, Stdout: h.stdout, Stderr: h.stderr}
	return h
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return h.app.Run(append(args, "-C", h.dir, "--no-color"))
}

const simpleManifest = `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: motd
spec:
  path: /etc/motd
  content: "welcome to the fleet\n"
`

func TestPlanExitsTwoOnDrift(t *testing.T) {
	h := newHarness(t, simpleManifest)

	if code := h.run("plan"); code != ExitOutOfSync {
		t.Fatalf("plan exit = %d, want %d (drift)\n%s", code, ExitOutOfSync, h.stdout.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "File/motd") {
		t.Errorf("plan should name the drifting resource:\n%s", out)
	}
	if !strings.Contains(out, "OutOfSync") {
		t.Errorf("plan should summarize as OutOfSync:\n%s", out)
	}
	if _, err := h.mem.Stat("/etc/motd"); err == nil {
		t.Error("plan must not modify the host")
	}
}

func TestApplyThenPlanIsClean(t *testing.T) {
	h := newHarness(t, simpleManifest)

	if code := h.run("apply"); code != ExitOK {
		t.Fatalf("apply exit = %d\n%s\n%s", code, h.stdout.String(), h.stderr.String())
	}
	data, err := h.mem.ReadFile("/etc/motd")
	if err != nil {
		t.Fatalf("apply did not write the file: %v", err)
	}
	if string(data) != "welcome to the fleet\n" {
		t.Errorf("content = %q", data)
	}

	if code := h.run("plan"); code != ExitOK {
		t.Errorf("plan after apply exit = %d, want 0 (converged)\n%s", code, h.stdout.String())
	}
	if !strings.Contains(h.stdout.String(), "in sync") {
		t.Errorf("plan output should say everything is in sync:\n%s", h.stdout.String())
	}
}

func TestStatusJSONIsMachineReadable(t *testing.T) {
	h := newHarness(t, simpleManifest)

	code := h.run("status", "--json")
	if code != ExitOutOfSync {
		t.Fatalf("status exit = %d, want %d", code, ExitOutOfSync)
	}

	var got struct {
		Status    string `json:"status"`
		OutOfSync bool   `json:"outOfSync"`
		Counts    struct {
			Total   int `json:"Total"`
			Changed int `json:"Changed"`
		} `json:"counts"`
		Results []struct {
			Resource string `json:"resource"`
			Sync     string `json:"sync"`
			Diff     []struct {
				Field string `json:"field"`
			} `json:"diff"`
		} `json:"results"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, h.stdout.String())
	}
	if !got.OutOfSync || got.Status != "OutOfSync" {
		t.Errorf("status = %q outOfSync = %v", got.Status, got.OutOfSync)
	}
	if len(got.Results) != 1 || got.Results[0].Resource != "File/motd" {
		t.Fatalf("results = %+v", got.Results)
	}
	if got.Results[0].Sync != "OutOfSync" || len(got.Results[0].Diff) == 0 {
		t.Errorf("result = %+v, want per-field diff detail", got.Results[0])
	}
}

func TestValidateRejectsABrokenManifest(t *testing.T) {
	h := newHarness(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: broken
spec:
  path: not/absolute
  content: "x\n"
`)
	if code := h.run("validate"); code != ExitError {
		t.Fatalf("validate exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(h.stderr.String(), "must be absolute") {
		t.Errorf("stderr should explain the problem:\n%s", h.stderr.String())
	}
}

func TestValidateAcceptsAGoodRepo(t *testing.T) {
	h := newHarness(t, simpleManifest)
	if code := h.run("validate"); code != ExitOK {
		t.Fatalf("validate exit = %d\n%s", code, h.stderr.String())
	}
}

func TestOnlySelectorNarrowsTheRun(t *testing.T) {
	h := newHarness(t, simpleManifest+`
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: issue
spec:
  path: /etc/issue
  content: "issue\n"
`)
	if code := h.run("apply", "--only", "File/motd"); code != ExitOK {
		t.Fatalf("apply exit = %d\n%s", code, h.stderr.String())
	}
	if _, err := h.mem.Stat("/etc/motd"); err != nil {
		t.Error("the selected resource should have been applied")
	}
	if _, err := h.mem.Stat("/etc/issue"); err == nil {
		t.Error("an unselected resource must be left alone")
	}
}

func TestLabelFlagDrivesTargeting(t *testing.T) {
	manifests := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: web-only
  targets:
    labels:
      role: web
spec:
  path: /etc/web.conf
  content: "web\n"
`
	h := newHarness(t, manifests)
	if code := h.run("apply", "--label", "role=db"); code != ExitOK {
		t.Fatalf("apply exit = %d\n%s", code, h.stderr.String())
	}
	if _, err := h.mem.Stat("/etc/web.conf"); err == nil {
		t.Error("a web-targeted file must not land on a db-labelled node")
	}

	h2 := newHarness(t, manifests)
	if code := h2.run("apply", "--label", "role=web"); code != ExitOK {
		t.Fatalf("apply exit = %d\n%s", code, h2.stderr.String())
	}
	if _, err := h2.mem.Stat("/etc/web.conf"); err != nil {
		t.Error("a matching node should receive the file")
	}
}

func TestMalformedLabelIsRejected(t *testing.T) {
	h := newHarness(t, simpleManifest)
	if code := h.run("apply", "--label", "role"); code != ExitError {
		t.Fatalf("exit = %d, want an error for a label without '='", code)
	}
	if !strings.Contains(h.stderr.String(), "key=value") {
		t.Errorf("stderr = %s", h.stderr.String())
	}
}

func TestUnknownCommandFails(t *testing.T) {
	app := &App{Host: host.NewMem(), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if code := app.Run([]string{"reticulate"}); code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
}

func TestKindsListsRegisteredResources(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Host: host.NewMem(), Stdout: stdout, Stderr: &bytes.Buffer{}}
	if code := app.Run([]string{"kinds"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"File", "Package", "Service", "SystemdUnit", "User", "Sysctl", "Exec"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("kinds output is missing %q:\n%s", want, stdout.String())
		}
	}
}
