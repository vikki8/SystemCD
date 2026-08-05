package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/vikki8/systemcd/internal/host"
	"github.com/vikki8/systemcd/internal/manifest"
	"github.com/vikki8/systemcd/internal/state"
)

func TestConflictingClaimsAreRefusedBeforeAnythingIsTouched(t *testing.T) {
	// Two resources pointing at one path do not "mostly work": the result
	// depends on document order. Refuse the repository instead.
	h := host.NewMem()
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf
spec:
  path: /etc/nginx/nginx.conf
  content: "from role A\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: nginx-conf-override
spec:
  path: /etc/nginx/nginx.conf
  content: "from role B\n"
`)
	_, err := Reconcile(context.Background(), Options{Repo: repo, Host: h, Node: manifest.Node{Hostname: "t"}})
	if err == nil {
		t.Fatal("want a conflict error")
	}
	for _, want := range []string{"conflicting ownership", "File/nginx-conf", "File/nginx-conf-override", "path:/etc/nginx/nginx.conf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if len(h.Paths()) != 1 {
		t.Errorf("nothing should have been written; paths = %v", h.Paths())
	}
}

func TestClaimConflictsSpanKinds(t *testing.T) {
	// A File and a SystemdUnit both writing the same unit file is the same
	// bug wearing a different hat.
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: SystemdUnit
metadata:
  name: billing
spec:
  content: "[Service]\nExecStart=/bin/true\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: billing-unit-by-hand
spec:
  path: /etc/systemd/system/billing.service
  content: "[Service]\nExecStart=/bin/false\n"
`)
	_, err := Reconcile(context.Background(), Options{Repo: repo, Host: host.NewMem(), Node: manifest.Node{Hostname: "t"}})
	if err == nil || !strings.Contains(err.Error(), "conflicting ownership") {
		t.Fatalf("error = %v, want a cross-kind ownership conflict", err)
	}
}

func TestServiceAndSystemdUnitForOneDaemonIsNotAConflict(t *testing.T) {
	// Writing the unit file and controlling the running unit are separate
	// responsibilities, and declaring both is the normal pattern.
	h := host.NewMem()
	h.AddStub(host.CommandStub{Match: "is-enabled", Stdout: "enabled"})
	h.AddStub(host.CommandStub{Match: "is-active", Stdout: "active"})
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: SystemdUnit
metadata:
  name: billing
spec:
  content: "[Service]\nExecStart=/bin/true\n"
---
apiVersion: systemcd.dev/v1
kind: Service
metadata:
  name: billing
spec:
  enabled: true
dependsOn: [SystemdUnit/billing]
`)
	if _, err := Reconcile(context.Background(), Options{Repo: repo, Host: h, Node: manifest.Node{Hostname: "t"}}); err != nil {
		t.Fatalf("this pairing must be allowed: %v", err)
	}
}

func TestOwnershipRecordsAdoptionVersusCreation(t *testing.T) {
	h := host.NewMem()
	// /etc/adopted already exists; /etc/created does not.
	h.SetFile("/etc/adopted", "someone else put this here\n", 0o644, 0, 0)

	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: adopted
spec:
  path: /etc/adopted
  content: "managed now\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: created
spec:
  path: /etc/created
  content: "brand new\n"
`)
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	rep := run(t, h, repo, func(o *Options) { o.Store = store })

	if got := result(t, rep, "File/adopted").Action; got != ActionUpdate {
		t.Errorf("adopted file action = %v, want update", got)
	}
	if got := result(t, rep, "File/created").Action; got != ActionCreate {
		t.Errorf("new file action = %v, want create", got)
	}

	snap, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	adopted := snap.Resources["File/adopted"]
	if !adopted.Adopted {
		t.Error("a file that already existed must be recorded as adopted")
	}
	if adopted.PriorState["checksum"] == "" {
		t.Errorf("adoption should record what the host looked like first, got %v", adopted.PriorState)
	}
	if created := snap.Resources["File/created"]; created.Adopted {
		t.Error("a file systemcd created must not be recorded as adopted")
	}
	if adopted.Claims == nil || adopted.Claims[0] != "path:/etc/adopted" {
		t.Errorf("claims = %v, want the managed path", adopted.Claims)
	}
	if adopted.LastChangedAt.IsZero() || adopted.FirstAppliedAt.IsZero() {
		t.Error("ownership timestamps should be populated after a change")
	}
}

func TestDriftOriginDistinguishesExternalEditsFromRepoChanges(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	v1 := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/app.conf
  content: "mode = production\n"
`
	run(t, h, repoFrom(t, v1), func(o *Options) { o.Store = store; o.Revision = "rev1" })

	t.Run("a repo change is attributed to the repo", func(t *testing.T) {
		v2 := strings.Replace(v1, "mode = production", "mode = staging", 1)
		rep := run(t, h, repoFrom(t, v2), func(o *Options) {
			o.Store = store
			o.DryRun = true
			o.Revision = "rev2"
		})
		res := result(t, rep, "File/conf")
		if res.Owner.Origin != state.OriginRepo {
			t.Errorf("origin = %q, want %q: the host still holds what systemcd wrote", res.Owner.Origin, state.OriginRepo)
		}
		if len(rep.ExternalDrift()) != 0 {
			t.Error("a repo change must not be reported as external drift")
		}
	})

	t.Run("a hand edit is attributed to the outside world", func(t *testing.T) {
		h.SetFile("/etc/app.conf", "mode = whatever-i-felt-like\n", 0o644, 0, 0)
		rep := run(t, h, repoFrom(t, v1), func(o *Options) {
			o.Store = store
			o.DryRun = true
			o.Revision = "rev1"
		})
		res := result(t, rep, "File/conf")
		if res.Owner.Origin != state.OriginExternal {
			t.Errorf("origin = %q, want %q: the host no longer holds what systemcd wrote", res.Owner.Origin, state.OriginExternal)
		}
		if got := rep.ExternalDrift(); len(got) != 1 {
			t.Errorf("external drift = %d resources, want 1", len(got))
		}
		if rep.Counts().ExternalDrift != 1 {
			t.Errorf("counts = %+v", rep.Counts())
		}
	})

	t.Run("a never-applied resource is new, not drifted", func(t *testing.T) {
		rep := run(t, h, repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: fresh
spec:
  path: /etc/fresh.conf
  content: "x\n"
`), func(o *Options) { o.Store = store; o.DryRun = true })
		if got := result(t, rep, "File/fresh").Owner.Origin; got != state.OriginNew {
			t.Errorf("origin = %q, want %q", got, state.OriginNew)
		}
	})
}

func TestUndeclaredResourcesAreReportedAsOrphansWithoutPrune(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	full := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: keep
spec:
  path: /etc/keep.conf
  content: "k\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: retired
spec:
  path: /etc/retired.conf
  content: "r\n"
`
	run(t, h, repoFrom(t, full), func(o *Options) { o.Store = store })

	trimmed := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: keep
spec:
  path: /etc/keep.conf
  content: "k\n"
`
	rep := run(t, h, repoFrom(t, trimmed), func(o *Options) { o.Store = store })

	orphan := result(t, rep, "File/retired")
	if orphan.Action != ActionOrphan {
		t.Fatalf("action = %v, want orphaned — a resource systemcd still owns must not go invisible", orphan.Action)
	}
	if !orphan.Owner.Owned {
		t.Error("an orphan is still owned; that is the whole point")
	}
	if rep.Counts().Orphaned != 1 {
		t.Errorf("counts = %+v, want one orphan", rep.Counts())
	}
	// An orphan is not a change the repository is asking for, so it must not
	// make `plan` claim there is drift to apply.
	if orphan.OutOfSync() {
		t.Error("an orphan should not count as out of sync")
	}
	if _, err := h.Stat("/etc/retired.conf"); err != nil {
		t.Error("without --prune the file must stay on disk")
	}
}

func TestDestructivePrunesRequireConfirmation(t *testing.T) {
	h := host.NewMem()
	h.Binaries["apt-get"] = true
	h.AddStub(host.CommandStub{Match: "dpkg-query", Stdout: "installed 1.0"})
	h.AddStub(host.CommandStub{Match: "getent passwd", Stdout: "svc:x:999:999::/var/lib/svc:/usr/sbin/nologin"})
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}

	full := `
apiVersion: systemcd.dev/v1
kind: Package
metadata:
  name: telnet
---
apiVersion: systemcd.dev/v1
kind: User
metadata:
  name: svc
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: note
spec:
  path: /etc/note
  content: "n\n"
`
	run(t, h, repoFrom(t, full), func(o *Options) { o.Store = store })

	empty := "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: keeper\nspec:\n  path: /etc/keeper\n  content: \"k\\n\"\n"

	t.Run("without --confirm the irreversible ones are withheld", func(t *testing.T) {
		rep := run(t, h, repoFrom(t, empty), func(o *Options) { o.Store = store; o.Prune = true })

		for _, ref := range []string{"Package/telnet", "User/svc"} {
			res := result(t, rep, ref)
			if res.Action != ActionSkip {
				t.Errorf("%s action = %v, want skipped pending confirmation", ref, res.Action)
			}
			if !strings.Contains(strings.Join(res.Messages, " "), "--confirm") {
				t.Errorf("%s should explain how to proceed, got %v", ref, res.Messages)
			}
		}
		// A file prune is recoverable from the backup directory, so it goes
		// ahead without ceremony.
		if got := result(t, rep, "File/note").Action; got != ActionPrune {
			t.Errorf("File/note action = %v, want prune", got)
		}
		if rep.Counts().NeedsConfirm != 2 {
			t.Errorf("counts = %+v, want two withheld", rep.Counts())
		}
		if h.Ran("apt-get remove") || h.Ran("userdel") {
			t.Errorf("nothing irreversible should have run; commands = %v", h.Commands)
		}
	})

	t.Run("with --confirm they proceed", func(t *testing.T) {
		rep := run(t, h, repoFrom(t, empty), func(o *Options) {
			o.Store = store
			o.Prune = true
			o.ConfirmDestructive = true
		})
		for _, ref := range []string{"Package/telnet", "User/svc"} {
			if got := result(t, rep, ref).Action; got != ActionPrune {
				t.Errorf("%s action = %v, want prune", ref, got)
			}
		}
		if !h.Ran("apt-get remove") || !h.Ran("userdel svc") {
			t.Errorf("commands = %v", h.Commands)
		}
	})
}

func TestOnlySuppressesPruningAndOrphanReporting(t *testing.T) {
	// With --only, the resources that are absent are absent by request. If
	// prune ran here it would delete most of the machine.
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	full := `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: a
spec:
  path: /etc/a
  content: "a\n"
---
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: b
spec:
  path: /etc/b
  content: "b\n"
`
	run(t, h, repoFrom(t, full), func(o *Options) { o.Store = store })

	rep := run(t, h, repoFrom(t, full), func(o *Options) {
		o.Store = store
		o.Prune = true
		o.Only = []string{"File/a"}
	})

	for _, res := range rep.Results {
		if res.Action == ActionPrune || res.Action == ActionOrphan {
			t.Errorf("%s was %v under --only; ownership cannot be judged from a partial view", res.ID, res.Action)
		}
	}
	if _, err := h.Stat("/etc/b"); err != nil {
		t.Fatal("the unselected file must survive")
	}
	if len(rep.Notes) == 0 {
		t.Error("the report should say why pruning was skipped")
	}
}

func TestFilePruneIsRecoverableFromBackup(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	run(t, h, repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: doomed
spec:
  path: /etc/doomed.conf
  content: "important\n"
`), func(o *Options) { o.Store = store })

	run(t, h, repoFrom(t, "apiVersion: systemcd.dev/v1\nkind: File\nmetadata:\n  name: other\nspec:\n  path: /etc/other\n  content: \"o\\n\"\n"),
		func(o *Options) { o.Store = store; o.Prune = true })

	if _, err := h.Stat("/etc/doomed.conf"); err == nil {
		t.Fatal("the file should have been pruned")
	}
	var found bool
	for _, p := range h.Paths() {
		if strings.HasPrefix(p, "/var/lib/systemcd/backups/etc_doomed.conf.") {
			data, _ := h.ReadFile(p)
			if string(data) == "important\n" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("a pruned file must be recoverable from the backup directory; paths = %v", h.Paths())
	}
}

func TestLastChangedTracksChangesNotConfirmations(t *testing.T) {
	h := host.NewMem()
	store := &state.Store{Host: h, Path: "/var/lib/systemcd/state.json"}
	repo := repoFrom(t, `
apiVersion: systemcd.dev/v1
kind: File
metadata:
  name: conf
spec:
  path: /etc/conf
  content: "v1\n"
`)
	run(t, h, repo, func(o *Options) { o.Store = store; o.Revision = "rev1" })
	snap, _ := store.Load()
	firstChange := snap.Resources["File/conf"].LastChangedAt

	// A second, no-op reconcile at a new revision confirms the resource but
	// does not change it.
	run(t, h, repo, func(o *Options) { o.Store = store; o.Revision = "rev2" })
	snap, _ = store.Load()
	rec := snap.Resources["File/conf"]

	if !rec.LastChangedAt.Equal(firstChange) {
		t.Error("confirming a resource is already correct must not count as changing it")
	}
	if !rec.LastAppliedAt.After(firstChange) && !rec.LastAppliedAt.Equal(firstChange) {
		t.Error("last applied should advance on every reconcile")
	}
	if rec.Revision != "rev1" {
		t.Errorf("revision = %q, want the revision that last actually changed it", rec.Revision)
	}
}
