package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/resource"
	"github.com/vikki8/systemcd/internal/state"
)

const secretRepo = `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: secret
spec:
  path: /etc/app/secret.env
  mode: "0600"
  content: "TOKEN=hunter2\n"
  sensitive: true
`

func stateBytes(t *testing.T, h *host.MemHost) string {
	t.Helper()
	data, err := h.ReadFile("/var/lib/systemcd/state.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSensitiveContentNeverReachesTheStateFile(t *testing.T) {
	// README: sensitive: true keeps the value out of output and state.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/app/secret.env", "TOKEN=old-secret\n", 0o600, 0, 0)

	adopt(t, h, store, repoFrom(t, secretRepo), false)
	if s := stateBytes(t, h); strings.Contains(s, "hunter2") || strings.Contains(s, "old-secret") {
		t.Fatalf("adopt wrote a sensitive value into state:\n%s", s)
	}
	run(t, h, repoFrom(t, secretRepo), func(o *Options) { o.Store = store })
	if s := stateBytes(t, h); strings.Contains(s, "hunter2") || strings.Contains(s, "old-secret") {
		t.Fatalf("apply wrote a sensitive value into state:\n%s", s)
	}

	// The stored manifest must still be good enough to restore from.
	rep := release(t, h, store, repoFrom(t, fileDoc("other", "/etc/other", "o")), func(o *ReleaseOptions) {
		o.Refs = []string{"File/secret"}
		o.Mode = ReleaseRestore
	})
	if res := result(t, rep, "File/secret"); res.Err != nil || res.Action != ActionRestore {
		t.Fatalf("restore = %v (%v)", res.Action, res.Err)
	}
	if data, _ := h.ReadFile("/etc/app/secret.env"); string(data) != "TOKEN=old-secret\n" {
		t.Errorf("content = %q, want the original back", data)
	}
}

func TestSensitiveFileIsStillPrunable(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	run(t, h, repoFrom(t, secretRepo), func(o *Options) { o.Store = store })

	rep := run(t, h, repoFrom(t, fileDoc("other", "/etc/other", "o")), func(o *Options) { o.Store = store; o.Prune = true })
	if got := result(t, rep, "File/secret").Action; got != ActionPrune {
		t.Errorf("action = %v, want prune", got)
	}
	if _, err := h.Stat("/etc/app/secret.env"); err == nil {
		t.Error("the sensitive file should have been pruned")
	}
}

func TestRestorePreviewHidesSensitiveChecksums(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/app/secret.env", "TOKEN=old-secret\n", 0o600, 0, 0)
	adopt(t, h, store, repoFrom(t, secretRepo), false)
	run(t, h, repoFrom(t, secretRepo), func(o *Options) { o.Store = store })

	rep := release(t, h, store, nil, func(o *ReleaseOptions) {
		o.Refs = []string{"File/secret"}
		o.Mode = ReleaseRestore
		o.DryRun = true
	})
	res := result(t, rep, "File/secret")
	if !res.Diff.Has("checksum") {
		t.Fatalf("diff = %+v, want the content change previewed", res.Diff)
	}
	for _, f := range res.Diff {
		if f.Field == "checksum" && !f.Sensitive {
			t.Errorf("the checksum of a sensitive file is shown: %+v", f)
		}
	}
}

func serviceHost() *host.MemHost {
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "disabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "inactive"})
	return h
}

func TestRestorePreviewListsOnlyWhatRestoreTouches(t *testing.T) {
	// Service.Restore reverts enabled only when the manifest managed it, and
	// active only when state is not unmanaged. The preview must agree.
	cases := []struct {
		name, spec  string
		want, avoid string
	}{
		{"enabled unmanaged", "  state: started\n", "active", "enabled"},
		{"state unmanaged", "  state: unmanaged\n  enabled: true\n", "enabled", "active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := serviceHost()
			store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
			repo := repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: app\nspec:\n"+tc.spec)
			adopt(t, h, store, repo, false)
			run(t, h, repo, func(o *Options) { o.Store = store })

			rep := release(t, h, store, nil, func(o *ReleaseOptions) {
				o.Refs = []string{"Service/app"}
				o.Mode = ReleaseRestore
				o.DryRun = true
			})
			res := result(t, rep, "Service/app")
			if !res.Diff.Has(tc.want) {
				t.Errorf("diff = %+v, want %s previewed", res.Diff, tc.want)
			}
			if res.Diff.Has(tc.avoid) {
				t.Errorf("diff = %+v lists %s, which restore leaves alone", res.Diff, tc.avoid)
			}
		})
	}
}

func restarts(h *host.MemHost) int {
	n := 0
	for _, c := range h.Commands {
		if strings.Contains(c, "systemctl restart nginx.service") {
			n++
		}
	}
	return n
}

func pendingOf(t *testing.T, store *state.Store) []string {
	t.Helper()
	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return snap.PendingRefresh
}

func TestRefreshOwedToAFailedTargetIsDeliveredNextRun(t *testing.T) {
	h := nginxHost(t)
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)
	// The service cannot even be observed this time.
	h.Stubs = append([]host.CommandStub{{Match: "is-enabled", Err: errors.New("dbus unavailable")}}, h.Stubs...)

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if result(t, rep, "Service/nginx").Err == nil {
		t.Fatal("setup: the service should fail")
	}

	h.Stubs = h.Stubs[1:]
	plan := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store; o.DryRun = true })
	if got := result(t, plan, "Service/nginx").Action; got != ActionRefresh {
		t.Errorf("plan = %v, want the owed refresh shown", got)
	}

	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if restarts(h) != 1 {
		t.Fatalf("restarts = %d, want the owed restart delivered once; commands = %v", restarts(h), h.Commands)
	}
	if p := pendingOf(t, store); len(p) != 0 {
		t.Errorf("pending = %v, want it cleared once delivered", p)
	}
	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if restarts(h) != 1 {
		t.Errorf("restarts = %d, want no further restart", restarts(h))
	}
}

func TestRefreshOwedToASkippedTargetIsDeliveredNextRun(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	h.AddStub(host.CommandStub{Match: "apt-get install", ExitCode: 100})
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active"})
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, `
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
notify: [Service/nginx]
---
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: nginx
dependsOn: [Package/nginx]
`)
	rep := run(t, h, repo, func(o *Options) { o.Store = store })
	if got := result(t, rep, "Service/nginx").Action; got != ActionSkip {
		t.Fatalf("setup: service = %v, want skipped", got)
	}

	h.Stubs = h.Stubs[2:]
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.24"})
	run(t, h, repo, func(o *Options) { o.Store = store })
	if restarts(h) != 1 {
		t.Errorf("restarts = %d, want the owed restart delivered; commands = %v", restarts(h), h.Commands)
	}
}

func TestFailedRefreshIsRetried(t *testing.T) {
	h := nginxHost(t)
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)
	h.Stubs = append([]host.CommandStub{{Match: "systemctl restart", ExitCode: 1, Stderr: "Job failed"}}, h.Stubs...)

	rep := run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if result(t, rep, "Service/nginx").Err == nil {
		t.Fatal("setup: the restart should fail")
	}
	h.Stubs = h.Stubs[1:]
	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if restarts(h) != 2 {
		t.Errorf("restart attempts = %d, want the failed one retried; commands = %v", restarts(h), h.Commands)
	}
}

func TestRefreshOwedToAResourceOutsideOnlyIsDeliveredLater(t *testing.T) {
	h := nginxHost(t)
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)

	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store; o.Only = []string{"File/nginx-conf"} })
	if restarts(h) != 0 {
		t.Fatal("setup: --only must not restart the excluded service")
	}
	if p := pendingOf(t, store); len(p) != 1 || p[0] != "Service/nginx" {
		t.Fatalf("pending = %v, want Service/nginx owed a refresh", p)
	}

	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store })
	if restarts(h) != 1 {
		t.Errorf("restarts = %d, want the owed restart delivered by the full run", restarts(h))
	}
}

func TestOwedRefreshIsDroppedWhenTheTargetLeavesTheRepo(t *testing.T) {
	h := nginxHost(t)
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	h.SetFile("/etc/nginx/nginx.conf", "stale\n", 0o644, 0, 0)
	run(t, h, repoFrom(t, nginxRepo), func(o *Options) { o.Store = store; o.Only = []string{"File/nginx-conf"} })

	withoutService := strings.SplitN(nginxRepo, "---\napiVersion: systemcd.dev/v1\nkind: Service", 2)[0]
	withoutService = strings.Replace(withoutService, "notify:\n  - Service/nginx\n", "", 1)
	run(t, h, repoFrom(t, withoutService), func(o *Options) { o.Store = store })
	if p := pendingOf(t, store); len(p) != 0 {
		t.Errorf("pending = %v, want it dropped once the target is gone", p)
	}
}

func TestAlwaysExecIsNotExternalDrift(t *testing.T) {
	// An `always: true` Exec is pending on every plan by design. That is not
	// someone editing the host, and must not feed systemcd_external_drift.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: Exec\nmetadata:\n  name: tick\nspec:\n  command: \"true\"\n  always: true\n")
	run(t, h, repo, func(o *Options) { o.Store = store })

	rep := run(t, h, repo, func(o *Options) { o.Store = store; o.DryRun = true })
	if got := result(t, rep, "Exec/tick").Owner.Origin; got == state.OriginExternal {
		t.Errorf("origin = %q, want no external attribution for a command that simply runs every time", got)
	}
	if c := rep.Counts(); c.ExternalDrift != 0 {
		t.Errorf("counts = %+v, want no external drift", c)
	}
}

func TestHealthIsReportedInADryRunWithoutChangingSync(t *testing.T) {
	// status is a dry run that wants the health column filled in. The health
	// verdict must not make an in-sync resource look out of sync.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active", Do: func(m *host.MemHost, _ []string) {
		// Observed active, then fails before the health check looks.
		m.Stubs = []host.CommandStub{
			{Match: "is-enabled", Stdout: "enabled"},
			{Match: "is-active", Stdout: "failed"},
		}
	}})
	repo := repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: Service\nmetadata:\n  name: app\nspec:\n  enabled: true\n")

	rep := run(t, h, repo, func(o *Options) { o.DryRun = true; o.HealthChecks = true })
	res := result(t, rep, "Service/app")
	if res.Health != resource.HealthDegraded {
		t.Errorf("health = %q, want Degraded reported in a dry run", res.Health)
	}
	if rep.OutOfSync() {
		t.Error("a degraded but in-sync resource must not make the plan out of sync")
	}
	if h.Ran("systemctl start") || h.Ran("systemctl restart") {
		t.Errorf("a dry run changed the service; commands = %v", h.Commands)
	}
}

func TestBaselineNamesDoNotCollide(t *testing.T) {
	a := baselineName(resource.ID{Kind: "File", Name: "a/b"})
	b := baselineName(resource.ID{Kind: "File", Name: "a_b"})
	if a == b {
		t.Errorf("File/a/b and File/a_b share the baseline %q", a)
	}
	for _, name := range []string{a, b, baselineName(resource.ID{Kind: "File", Name: "../../etc/passwd"})} {
		if strings.Contains(name, "/") || name == ".." || name == "." {
			t.Errorf("baseline name %q is not a single path element", name)
		}
	}
	if got := baselineName(resource.ID{Kind: "File", Name: "app-conf"}); got != "File_app-conf" {
		t.Errorf("an ordinary name should stay readable, got %q", got)
	}
}

func TestCheckAllowsOnePathForDisjointLabelTargets(t *testing.T) {
	variant := func(name, key, value string) string {
		return "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: " + name +
			"\n  targets:\n    labels:\n      " + key + ": " + value +
			"\nspec:\n  path: /etc/motd\n  content: \"" + name + "\\n\"\n"
	}
	disjoint := repoFrom(t, variant("motd-web", "role", "web")+"---\n"+variant("motd-db", "role", "db"))
	if err := Check(disjoint); err != nil {
		t.Errorf("role=web and role=db can never select the same host: %v", err)
	}

	for name, src := range map[string]string{
		"different keys": variant("motd-web", "role", "web") + "---\n" + variant("motd-prod", "env", "prod"),
		"one untargeted": variant("motd-web", "role", "web") + "---\n" + fileDoc("motd", "/etc/motd", "all"),
		"host globs": strings.Replace(variant("motd-a", "role", "web"), "labels:\n      role: web", `hosts: ["web-*"]`, 1) +
			"---\n" + strings.Replace(variant("motd-b", "role", "db"), "labels:\n      role: db", `hosts: ["db-*"]`, 1),
	} {
		if err := Check(repoFrom(t, src)); err == nil || !strings.Contains(err.Error(), "conflicting ownership") {
			t.Errorf("%s: err = %v, want a conflict (both could select one host)", name, err)
		}
	}
}

func TestFailedCreateIsNotAdoptedNextRun(t *testing.T) {
	// The file is written, then the chown fails. On the next run the file
	// exists, but systemcd created it: recording it as adopted, with
	// systemcd's own bytes as the "original", would make restore a lie.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: app-conf
spec:
  path: /etc/app.conf
  owner: svc
  content: "x\n"
`)
	if res := result(t, run(t, h, repo, func(o *Options) { o.Store = store }), "File/app-conf"); res.Err == nil {
		t.Fatal("setup: the chown should fail")
	}
	h.Users["svc"] = 1000
	run(t, h, repo, func(o *Options) { o.Store = store })

	snap, _ := store.Load()
	rec := snap.Resources["File/app-conf"]
	if rec.Adopted || rec.BaselinePath != "" {
		t.Errorf("record = %+v, want it recorded as created by systemcd, with no baseline", rec)
	}
}

func TestFailedCreateThatChangedNothingRecordsNothing(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", ExitCode: 1})
	h.AddStub(host.CommandStub{Match: "apt-get install", ExitCode: 100})
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	run(t, h, repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: Package\nmetadata:\n  name: nginx\n"), func(o *Options) { o.Store = store })

	if snap, _ := store.Load(); len(snap.Resources) != 0 {
		t.Errorf("state = %v, want nothing owned: the install never happened", snap.Refs())
	}
}
