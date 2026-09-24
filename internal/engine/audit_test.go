package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/state"
)

func fileDoc(name, path, content string) string {
	return "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: " + name +
		"\nspec:\n  path: " + path + "\n  content: \"" + content + "\\n\"\n"
}

func TestProviderMessagesReachTheResult(t *testing.T) {
	// What a provider did ("wrote ...", "systemctl restart ...") is what the
	// operator reads in the apply output. It must not be dropped.
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)
	rep := run(t, h, repoFrom(t, nginxRepo), nil)

	file := result(t, rep, "File/nginx-conf")
	if !strings.Contains(strings.Join(file.Messages, "\n"), "wrote /etc/nginx/nginx.conf") {
		t.Errorf("File messages = %q, want the write reported", file.Messages)
	}
	if file.Duration <= 0 {
		t.Errorf("File duration = %v, want it measured", file.Duration)
	}
	svc := result(t, rep, "Service/nginx")
	if !strings.Contains(strings.Join(svc.Messages, "\n"), "systemctl restart nginx.service") {
		t.Errorf("Service messages = %q, want the restart reported", svc.Messages)
	}
}

func TestUnparsableOrphanIsReportedUnderItsOwnRef(t *testing.T) {
	// The unparsable record sorts after a live one, so any mix-up between
	// "index among stale records" and "index among all records" names the
	// wrong resource.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, fileDoc("a-live", "/etc/a", "a"))
	run(t, h, repo, func(o *Options) { o.Store = store })

	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	snap.Resources["File/z-broken"] = state.Record{Kind: "File", Name: "z-broken", Manifest: "kind: [unterminated"}
	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}

	rep := run(t, h, repo, func(o *Options) { o.Store = store; o.DryRun = true })
	broken := result(t, rep, "File/z-broken")
	if broken.Action != ActionSkip || !strings.Contains(strings.Join(broken.Messages, " "), "unparsable") {
		t.Errorf("File/z-broken = %v %v, want skipped as unparsable", broken.Action, broken.Messages)
	}
	for _, r := range rep.Results {
		if r.ID.String() == "File/a-live" && r.Action != ActionNoop {
			t.Errorf("the live resource was reported as %v %v", r.Action, r.Messages)
		}
	}
}

func TestPruneNeverDeletesWhatALiveResourceClaims(t *testing.T) {
	// Renaming a resource in git leaves the old name in state as an orphan
	// that claims the same path as the new name. Pruning the old name must
	// not delete the file the new name just wrote.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	run(t, h, repoFrom(t, fileDoc("app-conf-old", "/etc/app.conf", "v1")), func(o *Options) { o.Store = store })

	renamed := repoFrom(t, fileDoc("app-conf", "/etc/app.conf", "v2"))

	plan := run(t, h, renamed, func(o *Options) { o.Store = store; o.Prune = true; o.DryRun = true })
	if got := result(t, plan, "File/app-conf-old").Action; got == ActionPrune {
		t.Errorf("plan would prune File/app-conf-old, whose path File/app-conf still manages")
	}

	rep := run(t, h, renamed, func(o *Options) { o.Store = store; o.Prune = true })
	old := result(t, rep, "File/app-conf-old")
	if old.Action == ActionPrune {
		t.Errorf("File/app-conf-old was pruned although File/app-conf claims the same path")
	}
	if !strings.Contains(strings.Join(old.Messages, " "), "File/app-conf") {
		t.Errorf("the result should name the resource that now manages the path, got %v", old.Messages)
	}
	data, err := h.ReadFile("/etc/app.conf")
	if err != nil || string(data) != "v2\n" {
		t.Fatalf("/etc/app.conf = %q (%v), want the live resource's content left in place", data, err)
	}

	// Without --prune it is still an orphan, but the hint must not be "run
	// --prune", since that would not remove it.
	orphan := result(t, run(t, h, renamed, func(o *Options) { o.Store = store; o.DryRun = true }), "File/app-conf-old")
	if orphan.Action != ActionOrphan {
		t.Errorf("action = %v, want orphaned", orphan.Action)
	}
	if strings.Contains(strings.Join(orphan.Messages, " "), "--prune to remove") {
		t.Errorf("an orphan superseded by a live resource should not suggest --prune, got %v", orphan.Messages)
	}
}

func TestNotifiedExecThatAlsoNeedsToRunRunsOnce(t *testing.T) {
	// The Exec's own apply is the run. The notification must not run the
	// command a second time in the same reconcile.
	h := host.NewMem()
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: schema
spec:
  path: /etc/app/schema.sql
  content: "create table t;\n"
notify: [Exec/migrate]
---
apiVersion: systemcd.dev/v1
kind: Exec
metadata:
  name: migrate
spec:
  command: "app-migrate --apply"
  creates: /var/lib/app/migrated
`)
	rep := run(t, h, repo, nil)

	if got := result(t, rep, "Exec/migrate").Action; got != ActionRun {
		t.Errorf("Exec action = %v, want run", got)
	}
	runs := 0
	for _, c := range h.Commands {
		if strings.Contains(c, "app-migrate --apply") {
			runs++
		}
	}
	if runs != 1 {
		t.Errorf("the command ran %d times, want exactly once; commands = %v", runs, h.Commands)
	}
}

func TestPlanDoesNotPromiseARefreshApplyWillNotPerform(t *testing.T) {
	// A File cannot be refreshed. Notifying one does nothing on apply, so the
	// plan must not claim otherwise (and must not exit 2 over it).
	h := host.NewMem()
	h.SetFile("/etc/b.conf", "b\n", 0o644, 0, 0)
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: a
spec:
  path: /etc/a.conf
  content: "a\n"
notify: [File/b]
---
`+fileDoc("b", "/etc/b.conf", "b"))

	plan := run(t, h, repo, func(o *Options) { o.DryRun = true })
	apply := run(t, h, repo, nil)

	planned, applied := result(t, plan, "File/b").Action, result(t, apply, "File/b").Action
	if planned != applied {
		t.Errorf("plan said %v but apply did %v", planned, applied)
	}
	if planned != ActionNoop {
		t.Errorf("File/b plan action = %v, want in-sync", planned)
	}
}

func TestInSyncFirstApplyCapturesABaselineForRestore(t *testing.T) {
	// The file already matches the manifest when systemcd first applies it,
	// so it is recorded as adopted. A later repository change rewrites it;
	// release --restore must still be able to put the original back.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/app.conf", "original\n", 0o640, 0, 0)

	v1 := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app-conf
spec:
  path: /etc/app.conf
  mode: "0640"
  content: "original\n"
`
	if got := result(t, run(t, h, repoFrom(t, v1), func(o *Options) { o.Store = store }), "File/app-conf").Action; got != ActionNoop {
		t.Fatalf("setup: first apply = %v, want in-sync", got)
	}
	run(t, h, repoFrom(t, strings.Replace(v1, "original", "rewritten", 1)), func(o *Options) { o.Store = store })

	rep, err := Release(context.Background(), ReleaseOptions{
		Host: h, Store: store, Node: manifest.Node{Hostname: "t"},
		Refs: []string{"File/app-conf"}, Mode: ReleaseRestore,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := result(t, rep, "File/app-conf")
	if res.Err != nil || res.Action != ActionRestore {
		t.Fatalf("release --restore = %v (%v), want restore", res.Action, res.Err)
	}
	if data, _ := h.ReadFile("/etc/app.conf"); string(data) != "original\n" {
		t.Errorf("content = %q, want the original back", data)
	}
}

func TestInSyncPlanWritesNoBaseline(t *testing.T) {
	h := host.NewMem()
	h.SetFile("/etc/app.conf", "same\n", 0o644, 0, 0)
	run(t, h, repoFrom(t, fileDoc("app-conf", "/etc/app.conf", "same")), func(o *Options) { o.DryRun = true })
	for _, p := range h.Paths() {
		if strings.HasPrefix(p, "/var/lib/systemcd") {
			t.Errorf("a plan wrote %s", p)
		}
	}
}

func TestPruningAnAbsentAssertionDeletesNothing(t *testing.T) {
	// "Make sure telnet is not installed" was removed from the repository.
	// That retires the rule; it must not uninstall a telnet that someone
	// has since installed on purpose, or delete a file someone has since
	// created at that path.
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}

	full := `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: telnet
spec:
  state: absent
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: no-legacy
spec:
  path: /etc/legacy.conf
  state: absent
`
	run(t, h, repoFrom(t, full), func(o *Options) { o.Store = store })
	if snap, _ := store.Load(); len(snap.Resources) != 2 {
		t.Fatalf("setup: state = %v", snap.Refs())
	}

	// Both things now exist, put there by someone else.
	h.Stubs = nil
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 0.17"})
	h.SetFile("/etc/legacy.conf", "wanted now\n", 0o644, 0, 0)

	rep := run(t, h, repoFrom(t, fileDoc("keeper", "/etc/keeper", "k")), func(o *Options) {
		o.Store = store
		o.Prune = true
		o.ConfirmDestructive = true
	})
	if h.Ran("apt-get remove") {
		t.Errorf("pruning an absent assertion uninstalled the package; commands = %v", h.Commands)
	}
	if _, err := h.Stat("/etc/legacy.conf"); err != nil {
		t.Errorf("pruning an absent assertion deleted /etc/legacy.conf")
	}
	for _, ref := range []string{"Package/telnet", "File/no-legacy"} {
		if res := result(t, rep, ref); res.Err != nil {
			t.Errorf("%s: %v", ref, res.Err)
		}
	}
	if snap, _ := store.Load(); len(snap.Resources) != 1 {
		t.Errorf("the retired records should be dropped, state = %v", snap.Refs())
	}
}

func TestUnbuildableOrphanDoesNotFailEveryRun(t *testing.T) {
	// A stored manifest that no longer builds (a kind removed or validation
	// tightened in a newer systemcd) cannot be pruned, but it must not turn
	// every plan into exit 1.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, fileDoc("a", "/etc/a", "a"))
	run(t, h, repo, func(o *Options) { o.Store = store })

	snap, _ := store.Load()
	snap.Resources["Widget/x"] = state.Record{Kind: "Widget", Name: "x",
		Manifest: "apiVersion: systemcd.dev/v1\nkind: Widget\nmetadata:\n  name: x\n"}
	if err := store.Save(snap); err != nil {
		t.Fatal(err)
	}

	rep := run(t, h, repo, func(o *Options) { o.Store = store; o.DryRun = true })
	if err := rep.Err(); err != nil {
		t.Errorf("an unbuildable orphan failed the plan: %v", err)
	}
	res := result(t, rep, "Widget/x")
	if res.Action != ActionSkip || !strings.Contains(strings.Join(res.Messages, " "), "release") {
		t.Errorf("Widget/x = %v %v, want skipped with a way out", res.Action, res.Messages)
	}
}

func TestOnlyReportsANotificationItCannotDeliver(t *testing.T) {
	// --only File/nginx-conf changes the config, but the service it notifies
	// is outside the run. The restart is lost for good (the next full run
	// sees the file in sync), so the report must say so.
	h := nginxHost(t)
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Only = []string{"File/nginx-conf"} })
	if h.Ran("systemctl restart") {
		t.Fatalf("setup: the service was restarted although --only excluded it")
	}
	if joined := strings.Join(rep.Notes, "\n"); !strings.Contains(joined, "Service/nginx") {
		t.Errorf("notes = %q, want the undelivered notification to Service/nginx called out", rep.Notes)
	}

	// Nothing to say when nothing changed.
	quiet := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Only = []string{"File/nginx-conf"} })
	if len(quiet.Notes) != 0 {
		t.Errorf("notes = %q, want none when no notification was due", quiet.Notes)
	}
}

func TestMovingAResourceToANewPathIsARepositoryChange(t *testing.T) {
	// Changing spec.path points the same resource at a different file. The
	// record describes the old file, so comparing the new one against it
	// would report "changed outside systemcd" for a pure repository change,
	// and a later restore would write the old file's bytes into the new path.
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)
	run(t, h, repoFrom(t, adoptRepo), func(o *Options) { o.Store = store })

	moved := strings.Replace(adoptRepo, "/etc/app.conf", "/etc/app/app.conf", 1)
	plan := run(t, h, repoFrom(t, moved), func(o *Options) { o.Store = store; o.DryRun = true })
	res := result(t, plan, "File/app-conf")
	if res.Owner.Origin == state.OriginExternal {
		t.Errorf("origin = %q: a path change in the repository is not an external edit", res.Owner.Origin)
	}
	if !strings.Contains(strings.Join(res.Messages, " "), "/etc/app.conf") {
		t.Errorf("the plan should say the old path is no longer managed, got %v", res.Messages)
	}

	run(t, h, repoFrom(t, moved), func(o *Options) { o.Store = store })
	release(t, h, store, repoFrom(t, fileDoc("other", "/etc/other", "o")), func(o *ReleaseOptions) { o.Mode = ReleaseRestore })
	if data, _ := h.ReadFile("/etc/app/app.conf"); string(data) == legacyConf {
		t.Errorf("restore wrote the old path's pre-adoption contents into the new path")
	}
}

func TestCheckRejectsAPatchEdgeToAnUndeclaredResource(t *testing.T) {
	// A typo in an edge a patch adds would otherwise be dropped silently at
	// plan time as "outside the working set", losing the ordering for good.
	repo := loadUnvalidated(t, fileDoc("app-conf", "/etc/app.conf", "x")+`
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: order-it
spec:
  target: File/app-conf
  dependsOn: [Package/ngnix]
  notify: [Service/ngnix]
`)
	if repo.Validate() == nil {
		t.Error("the manifest layer must reject edges to resources the repository does not declare")
	}
	err := Check(repo)
	if err == nil {
		t.Fatal("validate must reject edges to resources the repository does not declare")
	}
	for _, want := range []string{"Package/ngnix", "Service/ngnix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got: %v", want, err)
		}
	}
}

func TestReleaseStillRefusesWhenAPatchIsBroken(t *testing.T) {
	// A patch that cannot be applied on this host must not make the
	// repository look empty to release's "still declared" check.
	h, store := adoptHost(t)
	adopt(t, h, store, repoFrom(t, adoptRepo), false)

	// A patch that is not a mapping cannot be merged, so selecting this
	// host's documents fails.
	broken := loadUnvalidated(t, adoptRepo+`
---
apiVersion: systemcd.dev/v1
kind: Patch
metadata:
  name: tighten
spec:
  target: File/app-conf
  patch: [mode, "0600"]
`)
	if _, err := broken.SelectForE(manifest.Node{Hostname: "legacy-01"}); err == nil {
		t.Fatal("setup: the patch should fail to apply on legacy-01")
	}

	rep := release(t, h, store, broken, nil)
	if res := result(t, rep, "File/app-conf"); res.Err == nil {
		t.Errorf("release = %v, want a refusal: the repository still declares File/app-conf", res.Action)
	}
	if snap, _ := store.Load(); len(snap.Resources) != 1 {
		t.Error("ownership must be left intact")
	}
}

// loadUnvalidated loads a repository without the manifest layer's Validate,
// for tests that need a repository it would reject.
func loadUnvalidated(t *testing.T, src string) *manifest.Repository {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifests.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return repo
}
