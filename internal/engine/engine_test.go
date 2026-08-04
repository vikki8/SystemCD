package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

// repoFrom writes YAML into a temp dir and loads it as a repository.
func repoFrom(t *testing.T, src string) *manifest.Repository {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := repo.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return repo
}

func run(t *testing.T, h host.Host, repo *manifest.Repository, mutate func(*Options)) *Report {
	t.Helper()
	opts := Options{
		Repo:  repo,
		Host:  h,
		Node:  manifest.Node{Hostname: "testhost"},
		Store: &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	rep, err := Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return rep
}

func result(t *testing.T, rep *Report, ref string) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.ID.String() == ref {
			return r
		}
	}
	t.Fatalf("no result for %s; got %v", ref, refsOf(rep))
	return Result{}
}

func refsOf(rep *Report) []string {
	out := make([]string, 0, len(rep.Results))
	for _, r := range rep.Results {
		out = append(out, r.ID.String()+"="+string(r.Action))
	}
	return out
}

const nginxRepo = `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: nginx
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
spec:
  path: /etc/nginx/nginx.conf
  content: "worker_processes 4;\n"
dependsOn:
  - Package/nginx
notify:
  - Service/nginx
---
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
spec:
  enabled: true
  state: started
dependsOn:
  - Package/nginx
`

// nginxHost is a machine with nginx installed, running, and its config
// already matching the manifest.
func nginxHost(t *testing.T) *host.MemHost {
	t.Helper()
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.24.0"})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active"})
	h.SetFile("/etc/nginx/nginx.conf", "worker_processes 4;\n", 0o644, 0, 0)
	return h
}

func TestReconcileInSyncHostDoesNothing(t *testing.T) {
	h := nginxHost(t)
	rep := run(t, h, repoFrom(t, nginxRepo), nil)

	if rep.OutOfSync() {
		t.Errorf("an already-converged host should report in sync: %v", refsOf(rep))
	}
	if c := rep.Counts(); c.InSync != 3 || c.Changed != 0 {
		t.Errorf("counts = %+v, want 3 in sync", c)
	}
	if h.Ran("systemctl restart") || h.Ran("systemctl start") {
		t.Errorf("nothing should have been restarted; commands = %v", h.Commands)
	}
}

func TestReconcileOrdersByDependency(t *testing.T) {
	h := nginxHost(t)
	rep := run(t, h, repoFrom(t, nginxRepo), nil)

	order := map[string]int{}
	for i, r := range rep.Results {
		order[r.ID.String()] = i
	}
	if order["Package/nginx"] > order["File/nginx-conf"] {
		t.Error("the package must be reconciled before the config it owns")
	}
	if order["File/nginx-conf"] > order["Service/nginx"] {
		t.Error("a notifying file must be reconciled before the service it notifies")
	}
}

func TestConfigChangeNotifiesServiceRestart(t *testing.T) {
	h := nginxHost(t)
	// Someone edited the config by hand.
	h.SetFile("/etc/nginx/nginx.conf", "worker_processes 1;\n", 0o644, 0, 0)

	rep := run(t, h, repoFrom(t, nginxRepo), nil)

	if got := result(t, rep, "File/nginx-conf").Action; got != ActionUpdate {
		t.Errorf("File action = %v, want update", got)
	}
	svc := result(t, rep, "Service/nginx")
	if !svc.Notified {
		t.Error("the service should have been notified by the config change")
	}
	if svc.Action != ActionRefresh {
		t.Errorf("Service action = %v, want refresh", svc.Action)
	}
	if !h.Ran("systemctl restart nginx.service") {
		t.Errorf("the service was not restarted; commands = %v", h.Commands)
	}
	if data, _ := h.ReadFile("/etc/nginx/nginx.conf"); string(data) != "worker_processes 4;\n" {
		t.Errorf("config was not restored, got %q", data)
	}
}

func TestRestartHappensAfterTheConfigIsWritten(t *testing.T) {
	// Restarting before the new config lands would put the old config back
	// into service, which is the classic ordering bug in this kind of tool.
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)

	var contentAtRestart string
	h.AddStub(host.CommandStub{Match: "systemctl restart", Do: func(m *host.MemHost, args []string) {
		data, _ := m.ReadFile("/etc/nginx/nginx.conf")
		contentAtRestart = string(data)
	}})

	run(t, h, repoFrom(t, nginxRepo), nil)

	if contentAtRestart != "worker_processes 4;\n" {
		t.Errorf("config at restart time = %q, want the new content", contentAtRestart)
	}
}

func TestServiceStartedByItsOwnDiffIsNotAlsoRestarted(t *testing.T) {
	// A stopped service that both needs starting and was notified should be
	// started once, not started and then restarted.
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)
	h.Stubs = nil
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.24.0"})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "inactive"})

	run(t, h, repoFrom(t, nginxRepo), nil)

	if !h.Ran("systemctl start nginx.service") {
		t.Errorf("the service should have been started; commands = %v", h.Commands)
	}
	if h.Ran("systemctl restart nginx.service") {
		t.Errorf("a freshly started service must not also be restarted; commands = %v", h.Commands)
	}
}

func TestDryRunPlansWithoutTouchingTheHost(t *testing.T) {
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "drifted\n", 0o644, 0, 0)

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.DryRun = true })

	if !rep.OutOfSync() {
		t.Fatal("plan should report drift")
	}
	if data, _ := h.ReadFile("/etc/nginx/nginx.conf"); string(data) != "drifted\n" {
		t.Errorf("plan must not modify the host, content = %q", data)
	}
	if h.Ran("systemctl restart") {
		t.Errorf("plan must not restart anything; commands = %v", h.Commands)
	}
	// The plan should still show the restart the change would cause.
	if got := result(t, rep, "Service/nginx").Action; got != ActionRefresh {
		t.Errorf("Service action in plan = %v, want refresh so the operator sees it coming", got)
	}
	if _, err := h.Stat("/var/lib/systemcd/state.json"); err == nil {
		t.Error("a dry run must not write state")
	}
}

func TestFailureBlocksDependentsButNotSiblings(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	h.AddStub(host.CommandStub{Match: "apt-get install", ExitCode: 100, Stderr: "E: Unable to locate package nginx"})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "disabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "inactive"})

	src := nginxRepo + `
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: unrelated
spec:
  path: /etc/motd
  content: "hello\n"
`
	rep := run(t, h, repoFrom(t, src), nil)

	if result(t, rep, "Package/nginx").Err == nil {
		t.Fatal("the package install should have failed")
	}
	for _, ref := range []string{"File/nginx-conf", "Service/nginx"} {
		if got := result(t, rep, ref).Action; got != ActionSkip {
			t.Errorf("%s action = %v, want skipped because its dependency failed", ref, got)
		}
	}
	// An independent resource must still be reconciled.
	if got := result(t, rep, "File/unrelated").Action; got != ActionCreate {
		t.Errorf("File/unrelated action = %v, want create", got)
	}
	if rep.Err() == nil {
		t.Error("the report should carry an error")
	}
}

func TestPruneRemovesResourcesDroppedFromTheRepo(t *testing.T) {
	h := host.NewMem()
	full := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: keep
spec:
  path: /etc/keep.conf
  content: "keep\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: retired
spec:
  path: /etc/retired.conf
  content: "retired\n"
`
	run(t, h, repoFrom(t, full), nil)
	if _, err := h.Stat("/etc/retired.conf"); err != nil {
		t.Fatalf("setup: retired.conf should exist: %v", err)
	}

	trimmed := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: keep
spec:
  path: /etc/keep.conf
  content: "keep\n"
`
	rep := run(t, h, repoFrom(t, trimmed), func(o *Options) { o.Prune = true })

	if got := result(t, rep, "File/retired").Action; got != ActionPrune {
		t.Errorf("action = %v, want prune", got)
	}
	if _, err := h.Stat("/etc/retired.conf"); err == nil {
		t.Error("the pruned file should have been deleted")
	}
	if _, err := h.Stat("/etc/keep.conf"); err != nil {
		t.Error("the surviving file must be left alone")
	}

	// A second prune run has nothing left to do.
	rep2 := run(t, h, repoFrom(t, trimmed), func(o *Options) { o.Prune = true })
	for _, r := range rep2.Results {
		if r.Action == ActionPrune {
			t.Errorf("prune should be idempotent, got another prune of %s", r.ID)
		}
	}
}

func TestPruneIsOptOutByDefault(t *testing.T) {
	h := host.NewMem()
	run(t, h, repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: retired
spec:
  path: /etc/retired.conf
  content: "x\n"
`), nil)

	empty := "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: other\nspec:\n  path: /etc/other.conf\n  content: \"y\\n\"\n"
	run(t, h, repoFrom(t, empty), nil) // Prune not enabled

	if _, err := h.Stat("/etc/retired.conf"); err != nil {
		t.Error("without --prune, a removed resource must be left in place")
	}
}

func TestPruneRunsInReverseDependencyOrder(t *testing.T) {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active"})

	full := `
apiVersion: systemcd.dev/v1
kind: SystemdUnit
metadata:
  name: worker
spec:
  content: "[Service]\nExecStart=/bin/true\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: worker-env
spec:
  path: /etc/worker.env
  content: "MODE=prod\n"
dependsOn:
  - SystemdUnit/worker
`
	run(t, h, repoFrom(t, full), nil)

	rep := run(t, h, repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: keeper\nspec:\n  path: /etc/keeper\n  content: \"k\\n\"\n"),
		func(o *Options) { o.Prune = true })

	var pruneOrder []string
	for _, r := range rep.Results {
		if r.Action == ActionPrune {
			pruneOrder = append(pruneOrder, r.ID.String())
		}
	}
	if len(pruneOrder) != 2 {
		t.Fatalf("prune order = %v, want both resources", pruneOrder)
	}
	if pruneOrder[0] != "File/worker-env" {
		t.Errorf("prune order = %v, want the dependent removed first", pruneOrder)
	}
}

func TestOnlySelectorsFilterTheRun(t *testing.T) {
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "drifted\n", 0o644, 0, 0)

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) {
		o.DryRun = true
		o.Only = []string{"File/*"}
	})
	if len(rep.Results) != 1 || rep.Results[0].ID.String() != "File/nginx-conf" {
		t.Errorf("results = %v, want only the File", refsOf(rep))
	}
}

func TestTargetingSkipsResourcesForOtherHosts(t *testing.T) {
	h := host.NewMem()
	src := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: web-config
  targets:
    labels:
      role: web
spec:
  path: /etc/web.conf
  content: "web\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: shared
spec:
  path: /etc/shared.conf
  content: "shared\n"
`
	run(t, h, repoFrom(t, src), func(o *Options) {
		o.Node = manifest.Node{Hostname: "db-01", Labels: map[string]string{"role": "db"}}
	})

	if _, err := h.Stat("/etc/web.conf"); err == nil {
		t.Error("a web-targeted resource must not be applied on a db host")
	}
	if _, err := h.Stat("/etc/shared.conf"); err != nil {
		t.Error("an untargeted resource should apply everywhere")
	}
}

func TestHealthCheckSurfacesAFailedService(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.24.0"})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	// The unit reports inactive during observation, then fails to come up.
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "failed"})

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.HealthChecks = true })

	svc := result(t, rep, "Service/nginx")
	if svc.Health != resource.HealthDegraded {
		t.Errorf("health = %v, want Degraded", svc.Health)
	}
	if len(rep.Degraded()) != 1 {
		t.Errorf("report should list one degraded resource, got %d", len(rep.Degraded()))
	}
}

func TestStateRecordsAppliedRevision(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}

	repo := repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: a\nspec:\n  path: /etc/a\n  content: \"a\\n\"\n")
	for _, rev := range []string{"rev-one", "rev-two"} {
		if _, err := Reconcile(context.Background(), Options{
			Repo: repo, Host: h, Node: manifest.Node{Hostname: "t"}, Store: store, Revision: rev,
		}); err != nil {
			t.Fatalf("reconcile %s: %v", rev, err)
		}
	}

	snap, err := store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if snap.Revision != "rev-two" {
		t.Errorf("current revision = %q", snap.Revision)
	}
	prev, ok := snap.PreviousRevision()
	if !ok || prev != "rev-one" {
		t.Errorf("previous revision = %q (%v), want rev-one — rollback depends on this", prev, ok)
	}
	if _, ok := snap.Resources["File/a"]; !ok {
		t.Errorf("state should record the applied resource, got %v", snap.Refs())
	}
}

func TestCycleIsReportedClearly(t *testing.T) {
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: a
spec:
  path: /etc/a
  content: "a\n"
dependsOn:
  - File/b
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: b
spec:
  path: /etc/b
  content: "b\n"
dependsOn:
  - File/a
`)
	_, err := Reconcile(context.Background(), Options{Repo: repo, Host: host.NewMem(), Node: manifest.Node{Hostname: "t"}})
	if err == nil {
		t.Fatal("want a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "File/a") {
		t.Errorf("error = %v, want it to name the cycle members", err)
	}
}

func TestNotifyImpliesOrderingWithoutAnExplicitDependsOn(t *testing.T) {
	// The service is declared first in the file, but notify must still make
	// the file win the ordering.
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: app
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app-conf
spec:
  path: /etc/app.conf
  content: "x\n"
notify:
  - Service/app
`)
	g, err := build(repo.Documents)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var order []string
	for _, n := range g.sorted {
		order = append(order, n.id.String())
	}
	if order[0] != "File/app-conf" {
		t.Errorf("order = %v, want the notifier first", order)
	}
}

func TestCheckValidatesEveryDocumentRegardlessOfTargeting(t *testing.T) {
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: broken
  targets:
    hosts: ["some-other-host"]
spec:
  path: relative/path
  content: "x\n"
`)
	if err := Check(repo); err == nil {
		t.Fatal("validate must catch a broken manifest even when it targets another host")
	}
}

func TestClassifyUsesDiffDirectionNotAStateField(t *testing.T) {
	// Package and Sysctl have no "state" key; a plan must still say "create"
	// when everything is appearing rather than falling back to "update".
	cases := []struct {
		name string
		diff resource.Diff
		want Action
	}{
		{"package appearing", resource.Diff{{Field: "pkg:nginx", Have: "absent", Want: "present"}}, ActionCreate},
		{"file appearing", resource.Diff{{Field: "state", Have: "absent", Want: "present"}, {Field: "mode", Have: "<absent>", Want: "0644"}}, ActionCreate},
		{"package disappearing", resource.Diff{{Field: "pkg:telnet", Have: "present", Want: "absent"}}, ActionDelete},
		{"value drift", resource.Diff{{Field: "sysctl:net.ipv4.ip_forward", Have: "0", Want: "1"}}, ActionUpdate},
		{"mixed", resource.Diff{{Field: "pkg:a", Have: "absent", Want: "present"}, {Field: "pkg:b", Have: "present", Want: "absent"}}, ActionUpdate},
		{"exec", resource.Diff{{Field: "run", Have: "pending", Want: "satisfied"}}, ActionRun},
		{"empty", nil, ActionNoop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.diff); got != tc.want {
				t.Errorf("classify() = %v, want %v", got, tc.want)
			}
		})
	}
}
